// REST v3 (/api/v3): repos, refs, branches, commits, contents, labels,
// issues, comments, pull requests. Faithful port of the Node forge.

package main

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// --- JSON shapes (field order mirrors the Node forge) ---

func userJSON(login string) []byte {
	id := 0
	if login == viewerLogin {
		id = viewerID
	}
	b := []byte(`{"login":`)
	b = appendJSONString(b, login)
	b = append(b, []byte(`,"id":`)...)
	b = strconv.AppendInt(b, int64(id), 10)
	b = append(b, []byte(`,"type":"User","site_admin":false}`)...)
	return b
}

func repoJSON(r *repo) []byte {
	open := 0
	for _, i := range r.issues {
		if i.state == "open" {
			open++
		}
	}
	var b []byte
	b = append(b, `{"id":`...)
	b = strconv.AppendInt(b, int64(r.id), 10)
	b = append(b, `,"node_id":`...)
	b = appendJSONString(b, globalID("repo", r.owner+"/"+r.name))
	b = append(b, `,"name":`...)
	b = appendJSONString(b, r.name)
	b = append(b, `,"full_name":`...)
	b = appendJSONString(b, r.owner+"/"+r.name)
	b = append(b, `,"owner":`...)
	b = append(b, userJSON(r.owner)...)
	b = append(b, `,"private":`...)
	if r.private {
		b = append(b, "true"...)
	} else {
		b = append(b, "false"...)
	}
	b = append(b, `,"html_url":`...)
	b = appendJSONString(b, "https://example.invalid/"+r.owner+"/"+r.name)
	b = append(b, `,"description":`...)
	b = appendJSONString(b, r.description)
	b = append(b, `,"fork":false,"url":`...)
	b = appendJSONString(b, "/api/v3/repos/"+r.owner+"/"+r.name)
	b = append(b, `,"created_at":"`+fixedDate+`","updated_at":"`+fixedDate+`","pushed_at":"`+fixedDate+`","default_branch":`...)
	b = appendJSONString(b, r.defaultBranch)
	b = append(b, `,"has_issues":true,"has_projects":true,"has_wiki":true,"has_pages":false,"has_downloads":true,"archived":false,"disabled":false,"visibility":`...)
	if r.private {
		b = appendJSONString(b, "private")
	} else {
		b = appendJSONString(b, "public")
	}
	b = append(b, `,"license":null,"topics":[],"open_issues_count":`...)
	b = strconv.AppendInt(b, int64(open), 10)
	b = append(b, '}')
	return b
}

func labelJSON(r *repo, l *label) []byte {
	var b []byte
	b = append(b, `{"id":`...)
	b = strconv.AppendInt(b, int64(l.id), 10)
	b = append(b, `,"node_id":`...)
	b = appendJSONString(b, globalID("label", r.owner+"/"+r.name+":"+l.name))
	b = append(b, `,"url":`...)
	b = appendJSONString(b, "/api/v3/repos/"+r.owner+"/"+r.name+"/labels/"+url.PathEscape(l.name))
	b = append(b, `,"name":`...)
	b = appendJSONString(b, l.name)
	b = append(b, `,"color":`...)
	b = appendJSONString(b, l.color)
	b = append(b, `,"description":`...)
	b = appendJSONString(b, l.description)
	b = append(b, `,"default":false}`...)
	return b
}

func issueJSON(r *repo, iss *issue) []byte {
	var b []byte
	b = append(b, `{"id":`...)
	b = strconv.AppendInt(b, int64(iss.id), 10)
	b = append(b, `,"node_id":`...)
	b = appendJSONString(b, iss.nodeID)
	b = append(b, `,"number":`...)
	b = strconv.AppendInt(b, int64(iss.number), 10)
	b = append(b, `,"title":`...)
	b = appendJSONString(b, iss.title)
	b = append(b, `,"body":`...)
	b = appendJSONString(b, iss.body)
	b = append(b, `,"state":`...)
	b = appendJSONString(b, iss.state)
	b = append(b, `,"user":`...)
	b = append(b, userJSON(viewerLogin)...)
	b = append(b, `,"labels":[`...)
	for i, l := range iss.labels {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, labelJSON(r, l)...)
	}
	b = append(b, `],"assignees":[],"milestone":null,"locked":false,"comments":`...)
	b = strconv.AppendInt(b, int64(len(iss.comments)), 10)
	b = append(b, `,"created_at":"`+fixedDate+`","updated_at":"`+fixedDate+`","closed_at":`...)
	if iss.state == "closed" {
		b = appendJSONString(b, fixedDate)
	} else {
		b = append(b, "null"...)
	}
	b = append(b, `,"html_url":`...)
	b = appendJSONString(b, "https://example.invalid/"+r.owner+"/"+r.name+"/issues/"+intStr(iss.number))
	b = append(b, `,"url":`...)
	b = appendJSONString(b, "/api/v3/repos/"+r.owner+"/"+r.name+"/issues/"+intStr(iss.number))
	b = append(b, '}')
	return b
}

// cachedIssueJSON returns the cached serialization when valid. The cache
// fields are guarded by issueCacheMu (reads happen under the store read
// lock, writes must not race concurrent readers).
var issueCacheMu sync.Mutex

func cachedIssueJSON(r *repo, iss *issue) []byte {
	issueCacheMu.Lock()
	if iss.cacheValid {
		b := iss.cached
		issueCacheMu.Unlock()
		return b
	}
	issueCacheMu.Unlock()
	b := issueJSON(r, iss)
	issueCacheMu.Lock()
	if !iss.cacheValid {
		iss.cached = b
		iss.cacheValid = true
	}
	issueCacheMu.Unlock()
	return b
}

func (iss *issue) invalidate() {
	issueCacheMu.Lock()
	iss.cacheValid = false
	iss.cached = nil
	issueCacheMu.Unlock()
}

func commentJSON(r *repo, iss *issue, c *comment) []byte {
	var b []byte
	b = append(b, `{"id":`...)
	b = strconv.AppendInt(b, int64(c.id), 10)
	b = append(b, `,"node_id":`...)
	b = appendJSONString(b, c.nodeID)
	b = append(b, `,"body":`...)
	b = appendJSONString(b, c.body)
	b = append(b, `,"user":`...)
	b = append(b, userJSON(viewerLogin)...)
	b = append(b, `,"created_at":"`+fixedDate+`","updated_at":"`+fixedDate+`","html_url":`...)
	b = appendJSONString(b, "https://example.invalid/"+r.owner+"/"+r.name+"/issues/"+intStr(iss.number)+"#issuecomment-"+intStr(c.id))
	b = append(b, `,"url":`...)
	b = appendJSONString(b, "/api/v3/repos/"+r.owner+"/"+r.name+"/issues/comments/"+intStr(c.id))
	b = append(b, '}')
	return b
}

func prJSON(r *repo, pr *pull) []byte {
	headSha := branchTip(r, pr.head)
	if headSha == "" {
		headSha = zeroOID
	}
	baseSha := branchTip(r, pr.base)
	if baseSha == "" {
		baseSha = zeroOID
	}
	var b []byte
	b = append(b, `{"id":`...)
	b = strconv.AppendInt(b, int64(pr.id), 10)
	b = append(b, `,"node_id":`...)
	b = appendJSONString(b, pr.nodeID)
	b = append(b, `,"number":`...)
	b = strconv.AppendInt(b, int64(pr.number), 10)
	b = append(b, `,"title":`...)
	b = appendJSONString(b, pr.title)
	b = append(b, `,"body":`...)
	b = appendJSONString(b, pr.body)
	b = append(b, `,"state":`...)
	b = appendJSONString(b, pr.state)
	b = append(b, `,"user":`...)
	b = append(b, userJSON(viewerLogin)...)
	b = append(b, `,"head":{"label":`...)
	b = appendJSONString(b, r.owner+":"+pr.head)
	b = append(b, `,"ref":`...)
	b = appendJSONString(b, pr.head)
	b = append(b, `,"sha":`...)
	b = appendJSONString(b, headSha)
	b = append(b, `},"base":{"label":`...)
	b = appendJSONString(b, r.owner+":"+pr.base)
	b = append(b, `,"ref":`...)
	b = appendJSONString(b, pr.base)
	b = append(b, `,"sha":`...)
	b = appendJSONString(b, baseSha)
	b = append(b, `},"merged":`...)
	if pr.merged {
		b = append(b, "true"...)
	} else {
		b = append(b, "false"...)
	}
	b = append(b, `,"merged_at":`...)
	if pr.merged {
		b = appendJSONString(b, fixedDate)
	} else {
		b = append(b, "null"...)
	}
	b = append(b, `,"merge_commit_sha":`...)
	if pr.mergeCommitSha != "" {
		b = appendJSONString(b, pr.mergeCommitSha)
	} else {
		b = append(b, "null"...)
	}
	b = append(b, `,"mergeable":`...)
	if prMergeable(r, pr) == "MERGEABLE" {
		b = append(b, "true"...)
	} else {
		b = append(b, "false"...)
	}
	b = append(b, `,"draft":false,"locked":false,"created_at":"`+fixedDate+`","updated_at":"`+fixedDate+`","closed_at":`...)
	if pr.state == "closed" {
		b = appendJSONString(b, fixedDate)
	} else {
		b = append(b, "null"...)
	}
	b = append(b, `,"html_url":`...)
	b = appendJSONString(b, "https://example.invalid/"+r.owner+"/"+r.name+"/pull/"+intStr(pr.number))
	b = append(b, `,"url":`...)
	b = appendJSONString(b, "/api/v3/repos/"+r.owner+"/"+r.name+"/pulls/"+intStr(pr.number))
	b = append(b, '}')
	return b
}

// --- pagination ---

type pageParams struct {
	perPage int
	page    int
}

func parsePageParams(q url.Values) pageParams {
	perPage := 30
	if s := q.Get("per_page"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n != 0 {
			perPage = n
		}
	}
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	page := 1
	if s := q.Get("page"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n != 0 {
			page = n
		}
	}
	if page < 1 {
		page = 1
	}
	return pageParams{perPage: perPage, page: page}
}

// paginate writes a JSON array slice with a Link header when more pages exist.
func paginateJSON(items [][]byte, q url.Values, host, path string) (body []byte, link string) {
	pp := parsePageParams(q)
	total := len(items)
	pages := (total + pp.perPage - 1) / pp.perPage
	if pages < 1 {
		pages = 1
	}
	start := (pp.page - 1) * pp.perPage
	if start > total {
		start = total
	}
	end := start + pp.perPage
	if end > total {
		end = total
	}
	var b []byte
	b = append(b, '[')
	for i := start; i < end; i++ {
		if i > start {
			b = append(b, ',')
		}
		b = append(b, items[i]...)
	}
	b = append(b, ']')
	if pp.page < pages {
		nextQ := cloneQuery(q)
		nextQ.Set("page", intStr(pp.page+1))
		nextQ.Set("per_page", intStr(pp.perPage))
		lastQ := cloneQuery(q)
		lastQ.Set("page", intStr(pages))
		lastQ.Set("per_page", intStr(pp.perPage))
		nextURL := "http://" + host + path + "?" + nextQ.Encode()
		lastURL := "http://" + host + path + "?" + lastQ.Encode()
		link = "<" + nextURL + `>; rel="next", <` + lastURL + `>; rel="last"`
	}
	return b, link
}

func cloneQuery(q url.Values) url.Values {
	out := make(url.Values, len(q))
	for k, v := range q {
		cp := make([]string, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// --- handler ---

// restResult is what handleRest returns; the dispatcher writes it.
type restResult struct {
	status  int
	body    []byte
	headers [][2]string
	noBody  bool // 204: headers only
}

func rJSON(status int, body []byte, headers ...[2]string) restResult {
	return restResult{status: status, body: body, headers: headers}
}

func rErr(status int, msg string, headers ...[2]string) restResult {
	return restResult{status: status, body: []byte(`{"message":` + jsonQuote(msg) + "}\n"), headers: headers}
}

func handleRest(method string, segments []string, q url.Values, host, path string, body JVal) restResult {
	seg := func(i int) string {
		if i < len(segments) {
			return segments[i]
		}
		return ""
	}
	// GET /api/v3/
	if len(segments) == 0 {
		if method != "GET" {
			return rErr(404, "Not Found")
		}
		return rJSON(200, []byte(`{"current_user_url":"/api/v3/user","rate_limit_url":"/api/v3/rate_limit"}`), [2]string{"x-oauth-scopes", "repo, read:org"})
	}
	// GET /api/v3/meta
	if len(segments) == 1 && seg(0) == "meta" {
		if method != "GET" {
			return rErr(404, "Not Found")
		}
		return rJSON(200, []byte(`{"installed_version":"1.0.0","verifiable_password_authentication":false}`))
	}
	// GET /api/v3/user
	if len(segments) == 1 && seg(0) == "user" && method == "GET" {
		var b []byte
		b = append(b, `{"login":`...)
		b = appendJSONString(b, viewerLogin)
		b = append(b, `,"id":`...)
		b = strconv.AppendInt(b, int64(viewerID), 10)
		b = append(b, `,"type":"User","site_admin":false,"name":`...)
		b = appendJSONString(b, viewerName)
		b = append(b, `,"email":`...)
		b = appendJSONString(b, viewerEmail)
		b = append(b, `,"html_url":`...)
		b = appendJSONString(b, "https://example.invalid/"+viewerLogin)
		b = append(b, '}')
		return rJSON(200, b)
	}
	// /api/v3/user/repos
	if len(segments) == 2 && seg(0) == "user" && seg(1) == "repos" {
		if method == "GET" {
			db.mu.RLock()
			var all [][]byte
			for _, r := range db.repos {
				if r.owner == viewerLogin {
					all = append(all, repoJSON(r))
				}
			}
			db.mu.RUnlock()
			// repos map iteration order is random; sort by id like the Node forge
			sortRepoJSON(all)
			bodyBytes, link := paginateJSON(all, q, host, path)
			var hdrs [][2]string
			if link != "" {
				hdrs = append(hdrs, [2]string{"link", link})
			}
			return rJSON(200, bodyBytes, hdrs...)
		}
		if method == "POST" {
			name := jStr(mustGet(body, "name"))
			if name == "" {
				return rErr(422, "name is required")
			}
			db.mu.Lock()
			defer db.mu.Unlock()
			if _, ok := db.repos[repoKey(viewerLogin, name)]; ok {
				return rErr(422, "Repository already exists")
			}
			desc := jStr(mustGet(body, "description"))
			priv := jBool(mustGet(body, "private"))
			r := db.createRepo(viewerLogin, name, desc, priv)
			return rJSON(201, repoJSON(r))
		}
		return rErr(404, "Not Found")
	}
	// /api/v3/repos/{owner}/{repo}[...]
	if seg(0) == "repos" && len(segments) >= 3 {
		owner := seg(1)
		name := seg(2)
		tail := segments[3:]
		// Reads take the read lock; mutations take the write lock. We take the
		// write lock for the whole repo subtree when the method mutates, else
		// the read lock. (Simpler and race-free; contention is negligible.)
		mutating := method == "POST" || method == "PATCH" || method == "PUT" || method == "DELETE"
		if mutating {
			db.mu.Lock()
			defer db.mu.Unlock()
		} else {
			db.mu.RLock()
			defer db.mu.RUnlock()
		}
		r := db.getRepo(owner, name)
		if r == nil {
			return rErr(404, "Not Found")
		}
		if len(tail) == 0 {
			if method == "GET" {
				return rJSON(200, repoJSON(r))
			}
			if method == "PATCH" {
				if v, ok := jGet(body, "description"); ok {
					if s, ok := v.(string); ok {
						r.description = s
					}
				}
				if v, ok := jGet(body, "private"); ok {
					if b, ok := v.(bool); ok {
						r.private = b
					}
				}
				if v, ok := jGet(body, "default_branch"); ok {
					if s, ok := v.(string); ok && s != "" {
						r.defaultBranch = s
					}
				}
				return rJSON(200, repoJSON(r))
			}
			if method == "DELETE" {
				delete(db.repos, repoKey(owner, name))
				return restResult{status: 204, noBody: true}
			}
			return rErr(404, "Not Found")
		}
		head := tail[0]
		// ---- branches ----
		if head == "branches" && method == "GET" && len(tail) == 1 {
			var refs []string
			for ref := range r.refs {
				if strings.HasPrefix(ref, "refs/heads/") {
					refs = append(refs, ref)
				}
			}
			sortStrings(refs)
			var b []byte
			b = append(b, '[')
			for i, ref := range refs {
				if i > 0 {
					b = append(b, ',')
				}
				branch := ref[len("refs/heads/"):]
				sha := r.refs[ref]
				b = append(b, `{"name":`...)
				b = appendJSONString(b, branch)
				b = append(b, `,"commit":{"sha":`...)
				b = appendJSONString(b, sha)
				b = append(b, `,"url":`...)
				b = appendJSONString(b, "/api/v3/repos/"+owner+"/"+name+"/commits/"+sha)
				b = append(b, `},"protected":false}`...)
			}
			b = append(b, ']')
			return rJSON(200, b)
		}
		if head == "branches" && method == "GET" && len(tail) == 2 {
			tip := branchTip(r, tail[1])
			if tip == "" {
				return rErr(404, "Branch not found")
			}
			var b []byte
			b = append(b, `{"name":`...)
			b = appendJSONString(b, tail[1])
			b = append(b, `,"commit":{"sha":`...)
			b = appendJSONString(b, tip)
			b = append(b, `,"url":`...)
			b = appendJSONString(b, "/api/v3/repos/"+owner+"/"+name+"/commits/"+tip)
			b = append(b, `},"protected":false}`...)
			return rJSON(200, b)
		}
		// ---- git refs ----
		if head == "git" && len(tail) > 1 && tail[1] == "refs" {
			if method == "POST" && len(tail) == 2 {
				ref := jStr(mustGet(body, "ref"))
				sha := jStr(mustGet(body, "sha"))
				if !strings.HasPrefix(ref, "refs/") || !isOID(sha) {
					return rErr(422, "Invalid ref or sha")
				}
				obj, ok := r.objects[sha]
				if !ok {
					return rErr(422, "Invalid ref or sha")
				}
				if _, exists := r.refs[ref]; exists {
					return rErr(422, "Reference already exists")
				}
				r.setRef(ref, sha)
				var b []byte
				b = append(b, `{"ref":`...)
				b = appendJSONString(b, ref)
				b = append(b, `,"node_id":`...)
				b = appendJSONString(b, globalID("ref", owner+"/"+name+":"+ref))
				b = append(b, `,"url":`...)
				b = appendJSONString(b, "/api/v3/repos/"+owner+"/"+name+"/git/refs/"+strings.TrimPrefix(ref, "refs/"))
				b = append(b, `,"object":{"sha":`...)
				b = appendJSONString(b, sha)
				b = append(b, `,"type":`...)
				b = appendJSONString(b, obj.typ)
				b = append(b, `,"url":`...)
				b = appendJSONString(b, "/api/v3/repos/"+owner+"/"+name+"/git/"+obj.typ+"s/"+sha)
				b = append(b, `}}`...)
				return rJSON(201, b)
			}
			if method == "GET" {
				wanted := strings.Join(tail[2:], "/")
				if wanted == "" {
					var refs []string
					for ref := range r.refs {
						refs = append(refs, ref)
					}
					sortStrings(refs)
					var b []byte
					b = append(b, '[')
					for i, ref := range refs {
						if i > 0 {
							b = append(b, ',')
						}
						sha := r.refs[ref]
						typ := "commit"
						if o := r.objects[sha]; o != nil {
							typ = o.typ
						}
						b = append(b, `{"ref":`...)
						b = appendJSONString(b, ref)
						b = append(b, `,"url":`...)
						b = appendJSONString(b, "/api/v3/repos/"+owner+"/"+name+"/git/refs/"+strings.TrimPrefix(ref, "refs/"))
						b = append(b, `,"object":{"sha":`...)
						b = appendJSONString(b, sha)
						b = append(b, `,"type":`...)
						b = appendJSONString(b, typ)
						b = append(b, `,"url":""}}`...)
					}
					b = append(b, ']')
					return rJSON(200, b)
				}
				full := "refs/" + wanted
				sha, ok := r.refs[full]
				if !ok {
					return rErr(404, "Not Found")
				}
				typ := "commit"
				if o := r.objects[sha]; o != nil {
					typ = o.typ
				}
				var b []byte
				b = append(b, `{"ref":`...)
				b = appendJSONString(b, full)
				b = append(b, `,"node_id":`...)
				b = appendJSONString(b, globalID("ref", owner+"/"+name+":"+full))
				b = append(b, `,"url":`...)
				b = appendJSONString(b, "/api/v3/repos/"+owner+"/"+name+"/git/refs/"+wanted)
				b = append(b, `,"object":{"sha":`...)
				b = appendJSONString(b, sha)
				b = append(b, `,"type":`...)
				b = appendJSONString(b, typ)
				b = append(b, `,"url":""}}`...)
				return rJSON(200, b)
			}
			return rErr(404, "Not Found")
		}
		// ---- commits ----
		if head == "commits" && method == "GET" {
			refParam := q.Get("sha")
			if refParam == "" {
				refParam = r.defaultBranch
			}
			tip := resolveRef(r, refParam)
			var items [][]byte
			if tip != "" {
				seen := make(map[string]bool)
				var stack []string
				var order []string
				cur := tip
				for cur != "" && !seen[cur] {
					seen[cur] = true
					obj := r.objects[cur]
					if obj == nil || obj.typ != "commit" {
						break
					}
					order = append(order, cur)
					c := parseCommit(obj.data)
					cur = ""
					if len(c.parents) > 0 {
						cur = c.parents[0]
					}
					if len(c.parents) > 1 {
						for _, p := range c.parents[1:] {
							stack = append(stack, p)
						}
					}
				}
				for len(stack) > 0 {
					oid := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					if seen[oid] {
						continue
					}
					seen[oid] = true
					obj := r.objects[oid]
					if obj == nil || obj.typ != "commit" {
						continue
					}
					order = append(order, oid)
					for _, p := range parseCommit(obj.data).parents {
						stack = append(stack, p)
					}
				}
				for _, sha := range order {
					c := parseCommit(r.objects[sha].data)
					msg := c.message
					if strings.HasSuffix(msg, "\n") {
						msg = msg[:len(msg)-1]
					}
					var b []byte
					b = append(b, `{"sha":`...)
					b = appendJSONString(b, sha)
					b = append(b, `,"node_id":`...)
					b = appendJSONString(b, globalID("commit", owner+"/"+name+":"+sha))
					b = append(b, `,"commit":{"message":`...)
					b = appendJSONString(b, msg)
					b = append(b, `,"tree":{"sha":`...)
					b = appendJSONString(b, c.tree)
					b = append(b, `},"author":{"name":`...)
					b = appendJSONString(b, viewerName)
					b = append(b, `,"email":`...)
					b = appendJSONString(b, viewerEmail)
					b = append(b, `,"date":"`+fixedDate+`"},"committer":{"name":`...)
					b = appendJSONString(b, viewerName)
					b = append(b, `,"email":`...)
					b = appendJSONString(b, viewerEmail)
					b = append(b, `,"date":"`+fixedDate+`"}},"author":`...)
					b = append(b, userJSON(viewerLogin)...)
					b = append(b, `,"committer":`...)
					b = append(b, userJSON(viewerLogin)...)
					b = append(b, `,"parents":[`...)
					for i, p := range c.parents {
						if i > 0 {
							b = append(b, ',')
						}
						b = append(b, `{"sha":`...)
						b = appendJSONString(b, p)
						b = append(b, '}')
					}
					b = append(b, `],"url":`...)
					b = appendJSONString(b, "/api/v3/repos/"+owner+"/"+name+"/commits/"+sha)
					b = append(b, `,"html_url":`...)
					b = appendJSONString(b, "https://example.invalid/"+owner+"/"+name+"/commit/"+sha)
					b = append(b, '}')
					items = append(items, b)
				}
			}
			bodyBytes, link := paginateJSON(items, q, host, path)
			var hdrs [][2]string
			if link != "" {
				hdrs = append(hdrs, [2]string{"link", link})
			}
			return rJSON(200, bodyBytes, hdrs...)
		}
		// ---- contents ----
		if head == "contents" {
			filePath := joinDecoded(tail[1:])
			refParam := q.Get("ref")
			branch := r.defaultBranch
			if refParam != "" {
				branch = shortBranch(refParam)
			}
			tip := branchTip(r, branch)
			if method == "GET" {
				if tip == "" {
					return rErr(404, "Not Found")
				}
				treeOID := parseCommit(r.objects[tip].data).tree
				flat := flattenTree(r, treeOID)
				if filePath == "" {
					return rJSON(200, rootListingJSON(r, treeOID))
				}
				if entry, ok := flat[filePath]; ok {
					blob := r.objects[entry.oid]
					var b []byte
					b = append(b, `{"type":"file","encoding":"base64","size":`...)
					b = strconv.AppendInt(b, int64(len(blob.data)), 10)
					b = append(b, `,"name":`...)
					b = appendJSONString(b, baseName(filePath))
					b = append(b, `,"path":`...)
					b = appendJSONString(b, filePath)
					b = append(b, `,"content":`...)
					b = appendJSONString(b, b64Std(blob.data))
					b = append(b, `,"sha":`...)
					b = appendJSONString(b, entry.oid)
					b = append(b, `,"url":`...)
					b = appendJSONString(b, "/api/v3/repos/"+owner+"/"+name+"/contents/"+filePath)
					b = append(b, `,"html_url":`...)
					b = appendJSONString(b, "https://example.invalid/"+owner+"/"+name+"/blob/"+branch+"/"+filePath)
					b = append(b, `,"download_url":null}`...)
					return rJSON(200, b)
				}
				prefix := filePath + "/"
				children := make(map[string][]byte)
				var childNames []string
				for pth, e := range flat {
					if !strings.HasPrefix(pth, prefix) {
						continue
					}
					rest := pth[len(prefix):]
					if slash := strings.IndexByte(rest, '/'); slash < 0 {
						if _, ok := children[rest]; !ok {
							childNames = append(childNames, rest)
						}
						children[rest] = dirEntryJSON("file", rest, pth, e.oid, len(r.objects[e.oid].data))
					} else {
						dir := rest[:slash]
						if _, ok := children[dir]; !ok {
							childNames = append(childNames, dir)
							dirTree := findSubtree(r, treeOID, filePath+"/"+dir)
							if dirTree == "" {
								dirTree = zeroOID
							}
							children[dir] = dirEntryJSON("dir", dir, filePath+"/"+dir, dirTree, 0)
						}
					}
				}
				if len(children) > 0 {
					sortStrings(childNames)
					var b []byte
					b = append(b, '[')
					for i, n := range childNames {
						if i > 0 {
							b = append(b, ',')
						}
						b = append(b, children[n]...)
					}
					b = append(b, ']')
					return rJSON(200, b)
				}
				return rErr(404, "Not Found")
			}
			if method == "PUT" {
				contentB64 := jStr(mustGet(body, "content"))
				message := jStr(mustGet(body, "message"))
				if message == "" {
					message = "update"
				}
				content := b64DecodeLenient(contentB64)
				targetBranch := jStr(mustGet(body, "branch"))
				if targetBranch == "" {
					targetBranch = r.defaultBranch
				}
				baseTip := branchTip(r, targetBranch)
				baseFlat := make(map[string]treeEntry)
				if baseTip != "" {
					baseFlat = flattenTree(r, parseCommit(r.objects[baseTip].data).tree)
				} else if targetBranch != r.defaultBranch {
					defTip := branchTip(r, r.defaultBranch)
					if defTip != "" {
						baseTip = defTip
						baseFlat = flattenTree(r, parseCommit(r.objects[defTip].data).tree)
					}
				}
				existing, isUpdate := baseFlat[filePath]
				if isUpdate {
					sha := jStr(mustGet(body, "sha"))
					if sha == "" || !strings.EqualFold(sha, existing.oid) {
						return rErr(409, "sha does not match the current file")
					}
				}
				blobOID := putObject(r, "blob", content)
				next := make(map[string]treeEntry, len(baseFlat)+1)
				for k, v := range baseFlat {
					next[k] = v
				}
				next[filePath] = treeEntry{mode: "100644", oid: blobOID}
				treeOID := buildTreeFromMap(r, next)
				var parents []string
				if baseTip != "" {
					parents = []string{baseTip}
				}
				commitSha := forgeCommit(r, treeOID, parents, message)
				r.setRef(branchRef(targetBranch), commitSha)
				var b []byte
				b = append(b, `{"content":{"name":`...)
				b = appendJSONString(b, baseName(filePath))
				b = append(b, `,"path":`...)
				b = appendJSONString(b, filePath)
				b = append(b, `,"sha":`...)
				b = appendJSONString(b, blobOID)
				b = append(b, `,"size":`...)
				b = strconv.AppendInt(b, int64(len(content)), 10)
				b = append(b, `,"type":"file","encoding":"base64","content":`...)
				b = appendJSONString(b, contentB64)
				b = append(b, `,"url":`...)
				b = appendJSONString(b, "/api/v3/repos/"+owner+"/"+name+"/contents/"+filePath)
				b = append(b, `},"commit":{"sha":`...)
				b = appendJSONString(b, commitSha)
				b = append(b, `,"message":`...)
				b = appendJSONString(b, message)
				b = append(b, `,"author":{"name":`...)
				b = appendJSONString(b, viewerName)
				b = append(b, `,"email":`...)
				b = appendJSONString(b, viewerEmail)
				b = append(b, `,"date":"`+fixedDate+`"},"committer":{"name":`...)
				b = appendJSONString(b, viewerName)
				b = append(b, `,"email":`...)
				b = appendJSONString(b, viewerEmail)
				b = append(b, `,"date":"`+fixedDate+`"}}}`...)
				status := 201
				if isUpdate {
					status = 200
				}
				return rJSON(status, b)
			}
			if method == "DELETE" {
				baseTip := branchTip(r, branch)
				if baseTip == "" {
					return rErr(404, "Not Found")
				}
				baseFlat := flattenTree(r, parseCommit(r.objects[baseTip].data).tree)
				existing, ok := baseFlat[filePath]
				if !ok {
					return rErr(404, "Not Found")
				}
				if sha := jStr(mustGet(body, "sha")); sha != "" && !strings.EqualFold(sha, existing.oid) {
					return rErr(409, "sha does not match the current file")
				}
				msg := jStr(mustGet(body, "message"))
				if msg == "" {
					msg = "delete"
				}
				next := make(map[string]treeEntry, len(baseFlat))
				for k, v := range baseFlat {
					next[k] = v
				}
				delete(next, filePath)
				treeOID := buildTreeFromMap(r, next)
				commitSha := forgeCommit(r, treeOID, []string{baseTip}, msg)
				r.setRef(branchRef(branch), commitSha)
				var b []byte
				b = append(b, `{"commit":{"sha":`...)
				b = appendJSONString(b, commitSha)
				b = append(b, `,"message":`...)
				b = appendJSONString(b, msg)
				b = append(b, `}}`...)
				return rJSON(200, b)
			}
			return rErr(404, "Not Found")
		}
		// ---- labels ----
		if head == "labels" {
			if method == "GET" && len(tail) == 1 {
				var b []byte
				b = append(b, '[')
				i := 0
				// labels map iteration order is random; Node preserves insertion
				// order, but the suite only reads the set. Sort by id for determinism.
				labels := sortedLabels(r)
				for _, l := range labels {
					if i > 0 {
						b = append(b, ',')
					}
					b = append(b, labelJSON(r, l)...)
					i++
				}
				b = append(b, ']')
				return rJSON(200, b)
			}
			if method == "POST" && len(tail) == 1 {
				name := jStr(mustGet(body, "name"))
				if name == "" {
					return rErr(422, "name is required")
				}
				l, ok := r.labels[name]
				if !ok {
					color := jStr(mustGet(body, "color"))
					if color == "" {
						color = "ededed"
					}
					l = &label{id: r.nextLabelID, name: name, color: color, description: jStr(mustGet(body, "description"))}
					r.nextLabelID++
					r.labels[name] = l
				}
				return rJSON(201, labelJSON(r, l))
			}
			return rErr(404, "Not Found")
		}
		// ---- issues ----
		if head == "issues" {
			if method == "POST" && len(tail) == 1 {
				title := jStr(mustGet(body, "title"))
				if title == "" {
					return rErr(422, "title is required")
				}
				labelNames := labelNamesFrom(body)
				for _, n := range labelNames {
					if _, ok := r.labels[n]; !ok {
						return rErr(422, "Label '"+n+"' does not exist")
					}
				}
				iss := makeIssue(r, title, jStr(mustGet(body, "body")), labelNames)
				return rJSON(201, issueJSON(r, iss))
			}
			if method == "GET" && len(tail) == 1 {
				state := q.Get("state")
				if state == "" {
					state = "open"
				}
				sortDir := q.Get("sort")
				if sortDir == "" {
					sortDir = "created"
				}
				direction := q.Get("direction")
				if direction == "" {
					direction = "desc"
				}
				var list []*issue
				for _, i := range r.issues {
					if state == "all" || i.state == state {
						list = append(list, i)
					}
				}
				if sortDir == "created" {
					if direction == "asc" {
						sortIssuesAsc(list)
					} else {
						sortIssuesDesc(list)
					}
				}
				var items [][]byte
				for _, i := range list {
					items = append(items, cachedIssueJSON(r, i))
				}
				bodyBytes, link := paginateJSON(items, q, host, path)
				var hdrs [][2]string
				if link != "" {
					hdrs = append(hdrs, [2]string{"link", link})
				}
				return rJSON(200, bodyBytes, hdrs...)
			}
			if len(tail) >= 2 && isDigits(tail[1]) {
				number, _ := strconv.Atoi(tail[1])
				iss, ok := r.issueByNumber[number]
				if !ok {
					return rErr(404, "Not Found")
				}
				if len(tail) == 2 {
					if method == "GET" {
						return rJSON(200, cachedIssueJSON(r, iss))
					}
					if method == "PATCH" {
						if v, ok := jGet(body, "state"); ok {
							if s, ok := v.(string); ok && (s == "open" || s == "closed") {
								iss.state = s
							}
						}
						if v, ok := jGet(body, "title"); ok {
							if s, ok := v.(string); ok {
								iss.title = s
							}
						}
						if v, ok := jGet(body, "body"); ok {
							if s, ok := v.(string); ok {
								iss.body = s
							}
						}
						iss.invalidate()
						if v, ok := jGet(body, "labels"); ok {
							if arr, ok := v.([]JVal); ok {
								var names []string
								for _, l := range arr {
									if s, ok := l.(string); ok {
										names = append(names, s)
									} else if m, ok := l.(map[string]JVal); ok {
										if s, ok := m["name"].(string); ok {
											names = append(names, s)
										}
									}
								}
								for _, n := range names {
									if _, ok := r.labels[n]; !ok {
										return rErr(422, "Label '"+n+"' does not exist")
									}
								}
								iss.labels = nil
								for _, n := range names {
									iss.labels = append(iss.labels, r.labels[n])
								}
							}
						}
						return rJSON(200, issueJSON(r, iss))
					}
					return rErr(404, "Not Found")
				}
				if tail[2] == "comments" {
					if method == "POST" {
						text := jStr(mustGet(body, "body"))
						id := r.nextCommentID
						r.nextCommentID++
						c := &comment{id: id, nodeID: globalID("comment", owner+"/"+name+"#"+intStr(number)+":"+intStr(r.nextCommentID)), body: text}
						iss.comments = append(iss.comments, c)
						iss.invalidate()
						return rJSON(201, commentJSON(r, iss, c))
					}
					if method == "GET" {
						var b []byte
						b = append(b, '[')
						for i, c := range iss.comments {
							if i > 0 {
								b = append(b, ',')
							}
							b = append(b, commentJSON(r, iss, c)...)
						}
						b = append(b, ']')
						return rJSON(200, b)
					}
					return rErr(404, "Not Found")
				}
				return rErr(404, "Not Found")
			}
			return rErr(404, "Not Found")
		}
		// ---- pulls ----
		if head == "pulls" {
			if method == "POST" && len(tail) == 1 {
				title := jStr(mustGet(body, "title"))
				headBranch := shortBranch(jStr(mustGet(body, "head")))
				baseBranch := shortBranch(jStr(mustGet(body, "base")))
				if baseBranch == "" {
					baseBranch = r.defaultBranch
				}
				if title == "" || headBranch == "" {
					return rErr(422, "title and head are required")
				}
				if branchTip(r, headBranch) == "" {
					return rErr(422, "head branch '"+headBranch+"' does not exist")
				}
				if branchTip(r, baseBranch) == "" {
					return rErr(422, "base branch '"+baseBranch+"' does not exist")
				}
				if headBranch == baseBranch {
					return rErr(422, "head and base are the same")
				}
				for _, p := range r.pulls {
					if p.head == headBranch && p.base == baseBranch && p.state == "open" {
						return rErr(422, "A pull request already exists for this head and base")
					}
				}
				pr := makePull(r, title, jStr(mustGet(body, "body")), headBranch, baseBranch)
				return rJSON(201, prJSON(r, pr))
			}
			if method == "GET" && len(tail) == 1 {
				state := q.Get("state")
				if state == "" {
					state = "open"
				}
				var list []*pull
				for _, p := range r.pulls {
					if state == "all" || p.state == state {
						list = append(list, p)
					}
				}
				sortPullsDesc(list)
				var items [][]byte
				for _, p := range list {
					items = append(items, prJSON(r, p))
				}
				bodyBytes, link := paginateJSON(items, q, host, path)
				var hdrs [][2]string
				if link != "" {
					hdrs = append(hdrs, [2]string{"link", link})
				}
				return rJSON(200, bodyBytes, hdrs...)
			}
			if len(tail) >= 2 && isDigits(tail[1]) {
				number, _ := strconv.Atoi(tail[1])
				pr, ok := r.prByNumber[number]
				if !ok {
					return rErr(404, "Not Found")
				}
				if len(tail) == 2 && method == "GET" {
					return rJSON(200, prJSON(r, pr))
				}
				if len(tail) == 3 && tail[2] == "files" && method == "GET" {
					files := pr.files
					if files == nil {
						files = computePrFiles(r, pr)
					}
					var b []byte
					b = append(b, '[')
					for i, f := range files {
						if i > 0 {
							b = append(b, ',')
						}
						b = append(b, `{"sha":"","filename":`...)
						b = appendJSONString(b, f.filename)
						b = append(b, `,"status":`...)
						b = appendJSONString(b, f.status)
						b = append(b, `,"additions":`...)
						b = strconv.AppendInt(b, int64(f.additions), 10)
						b = append(b, `,"deletions":`...)
						b = strconv.AppendInt(b, int64(f.deletions), 10)
						b = append(b, `,"changes":`...)
						b = strconv.AppendInt(b, int64(f.changes), 10)
						b = append(b, `,"blob_url":"","raw_url":"","contents_url":""}`...)
					}
					b = append(b, ']')
					return rJSON(200, b)
				}
				if len(tail) == 3 && tail[2] == "merge" && method == "PUT" {
					if pr.state != "open" {
						return rErr(405, "Pull request is already closed")
					}
					status, sha, msg := doMerge(r, pr)
					if status != 200 {
						return rErr(status, msg)
					}
					var b []byte
					b = append(b, `{"sha":`...)
					b = appendJSONString(b, sha)
					b = append(b, `,"merged":true,"message":"Pull Request successfully merged"}`...)
					return rJSON(200, b)
				}
				return rErr(404, "Not Found")
			}
			return rErr(404, "Not Found")
		}
		return rErr(404, "Not Found")
	}
	return rErr(404, "Not Found")
}

func mustGet(body JVal, key string) JVal {
	v, _ := jGet(body, key)
	return v
}

func labelNamesFrom(body JVal) []string {
	v, ok := jGet(body, "labels")
	if !ok {
		return nil
	}
	arr, ok := v.([]JVal)
	if !ok {
		return nil
	}
	var out []string
	for _, l := range arr {
		if s, ok := l.(string); ok {
			out = append(out, s)
		} else if m, ok := l.(map[string]JVal); ok {
			if s, ok := m["name"].(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func joinDecoded(segs []string) string {
	parts := make([]string, len(segs))
	for i, s := range segs {
		if d, err := url.PathUnescape(s); err == nil {
			parts[i] = d
		} else {
			parts[i] = s
		}
	}
	return strings.Join(parts, "/")
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func rootListingJSON(r *repo, treeOID string) []byte {
	obj := r.objects[treeOID]
	if obj == nil || obj.typ != "tree" {
		return []byte("[]")
	}
	entries, err := parseTree(obj.data)
	if err != nil {
		return []byte("[]")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	var b []byte
	b = append(b, '[')
	for i, e := range entries {
		if i > 0 {
			b = append(b, ',')
		}
		typ := "file"
		size := 0
		if e.mode == "40000" {
			typ = "dir"
		} else if o := r.objects[e.oid]; o != nil {
			size = len(o.data)
		}
		b = append(b, `{"type":`...)
		b = appendJSONString(b, typ)
		b = append(b, `,"name":`...)
		b = appendJSONString(b, e.name)
		b = append(b, `,"path":`...)
		b = appendJSONString(b, e.name)
		b = append(b, `,"sha":`...)
		b = appendJSONString(b, e.oid)
		b = append(b, `,"size":`...)
		b = strconv.AppendInt(b, int64(size), 10)
		b = append(b, '}')
	}
	b = append(b, ']')
	return b
}

func dirEntryJSON(typ, name, path, sha string, size int) []byte {
	var b []byte
	b = append(b, `{"type":`...)
	b = appendJSONString(b, typ)
	b = append(b, `,"name":`...)
	b = appendJSONString(b, name)
	b = append(b, `,"path":`...)
	b = appendJSONString(b, path)
	b = append(b, `,"sha":`...)
	b = appendJSONString(b, sha)
	b = append(b, `,"size":`...)
	b = strconv.AppendInt(b, int64(size), 10)
	b = append(b, '}')
	return b
}

func findSubtree(r *repo, treeOID, path string) string {
	parts := strings.Split(path, "/")
	cur := treeOID
	for _, part := range parts {
		obj := r.objects[cur]
		if obj == nil || obj.typ != "tree" {
			return ""
		}
		entries, err := parseTree(obj.data)
		if err != nil {
			return ""
		}
		found := ""
		for _, e := range entries {
			if e.name == part {
				found = e.oid
				break
			}
		}
		if found == "" {
			return ""
		}
		cur = found
	}
	return cur
}

func sortedLabels(r *repo) []*label {
	out := make([]*label, 0, len(r.labels))
	for _, l := range r.labels {
		out = append(out, l)
	}
	sortSliceLabels(out)
	return out
}

func sortSliceLabels(ls []*label) {
	for i := 1; i < len(ls); i++ {
		for j := i; j > 0 && ls[j-1].id > ls[j].id; j-- {
			ls[j-1], ls[j] = ls[j], ls[j-1]
		}
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func sortIssuesAsc(list []*issue) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j-1].number > list[j].number; j-- {
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
}

func sortIssuesDesc(list []*issue) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j-1].number < list[j].number; j-- {
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
}

func sortPullsDesc(list []*pull) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j-1].number < list[j].number; j-- {
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
}

func sortRepoJSON(all [][]byte) {
	// repos were collected in map order; the Node forge sorts by repo id.
	// repoJSON starts with {"id":N — sort by that numeric prefix.
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && repoJSONID(all[j-1]) > repoJSONID(all[j]); j-- {
			all[j-1], all[j] = all[j], all[j-1]
		}
	}
}

func repoJSONID(b []byte) int {
	// b starts with {"id":
	i := 6
	n := 0
	for i < len(b) && b[i] >= '0' && b[i] <= '9' {
		n = n*10 + int(b[i]-'0')
		i++
	}
	return n
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
