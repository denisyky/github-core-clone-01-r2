// In-memory store: repos, objects, refs, issues, pull requests, and the
// deterministic commit/id counters. Faithful port of the Node forge store.

package main

import (
	"bytes"
	"encoding/base64"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

const fixedDate = "2025-01-01T00:00:00Z"
const commitEpoch = int64(1735689600)

type label struct {
	id          int
	name        string
	color       string
	description string
}

type comment struct {
	id     int
	nodeID string
	body   string
}

type issue struct {
	number   int
	id       int
	nodeID   string
	title    string
	body     string
	state    string // "open" | "closed"
	labels   []*label
	comments []*comment

	// cached serialized JSON (read workload); cleared on any mutation.
	cached     []byte
	cacheValid bool
}

type fileDiff struct {
	filename  string
	status    string // added | modified | removed
	additions int
	deletions int
	changes   int
}

type pull struct {
	number         int
	id             int
	nodeID         string
	title          string
	body           string
	head           string
	base           string
	state          string // "open" | "closed"
	merged         bool
	mergeCommitSha string
	files          []fileDiff
}

type repo struct {
	id            int
	name          string
	owner         string
	description   string
	private       bool
	defaultBranch string
	objects       map[string]*gitObject
	refs          map[string]string
	labels        map[string]*label
	nextLabelID   int
	issues        []*issue
	issueByNumber map[int]*issue
	pulls         []*pull
	prByNumber    map[int]*pull
	nextNumber    int
	nextCommentID int

	// advertisement cache (git.go); bumped on any ref change.
	advVer   atomic.Uint64
	advMu    sync.Mutex
	advCache map[string]advEntry // key: "v2-up" | "v2" | "v0-up" | "v0-recv"
}

type advEntry struct {
	ver  uint64
	body []byte
}

type store struct {
	mu              sync.RWMutex
	repos           map[string]*repo
	nextRepoID      int
	commitCounter   int64
	globalIDCounter int64
}

var db = &store{repos: make(map[string]*repo), nextRepoID: 1}

func repoKey(owner, name string) string { return owner + "/" + name }

func globalID(kind, payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + ":" + payload))
}

func (s *store) createRepo(owner, name, description string, priv bool) *repo {
	id := s.nextRepoID
	s.nextRepoID++
	r := &repo{
		id: id, name: name, owner: owner,
		description: description, private: priv,
		defaultBranch: "main",
		objects:       make(map[string]*gitObject),
		refs:          make(map[string]string),
		labels:        make(map[string]*label),
		nextLabelID:   1,
		issueByNumber: make(map[int]*issue),
		prByNumber:    make(map[int]*pull),
		nextNumber:    1,
		nextCommentID: 1,
		advCache:      make(map[string]advEntry),
	}
	s.repos[repoKey(owner, name)] = r
	return r
}

// getRepo returns nil unless the owner is the viewer login.
func (s *store) getRepo(owner, name string) *repo {
	if owner != viewerLogin {
		return nil
	}
	return s.repos[repoKey(owner, name)]
}

func (r *repo) bump() {
	r.advVer.Add(1)
}

func (r *repo) setRef(ref, oid string) {
	r.refs[ref] = oid
	r.bump()
}

func (r *repo) delRef(ref string) {
	delete(r.refs, ref)
	r.bump()
}

func putObject(r *repo, typ string, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := r.objects[oid]; !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		r.objects[oid] = &gitObject{typ: typ, data: cp}
	}
	return oid
}

func refOID(r *repo, ref string) string {
	if v, ok := r.refs[ref]; ok {
		return v
	}
	return ""
}

func branchRef(branch string) string { return "refs/heads/" + branch }

func branchTip(r *repo, branch string) string { return refOID(r, branchRef(branch)) }

func resolveRef(r *repo, ref string) string {
	if ref == "" {
		return ""
	}
	if isOID(ref) {
		if _, ok := r.objects[ref]; ok {
			return ref
		}
		return ""
	}
	if strings.HasPrefix(ref, "refs/") {
		return refOID(r, ref)
	}
	if h := branchTip(r, ref); h != "" {
		return h
	}
	return refOID(r, "refs/tags/"+ref)
}

func peelToCommit(r *repo, oid string) string {
	cur := oid
	seen := make(map[string]bool)
	for cur != "" && !seen[cur] {
		seen[cur] = true
		obj := r.objects[cur]
		if obj == nil {
			return ""
		}
		if obj.typ == "commit" {
			return cur
		}
		if obj.typ == "tag" {
			cur = parseTag(obj.data).object
			continue
		}
		return ""
	}
	return ""
}

func flattenTree(r *repo, treeOID string) map[string]treeEntry {
	out := make(map[string]treeEntry)
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		obj := r.objects[oid]
		if obj == nil || obj.typ != "tree" {
			return
		}
		entries, err := parseTree(obj.data)
		if err != nil {
			return
		}
		for _, e := range entries {
			path := e.name
			if prefix != "" {
				path = prefix + "/" + e.name
			}
			if e.mode == "40000" {
				walk(e.oid, path)
			} else {
				out[path] = e
			}
		}
	}
	walk(treeOID, "")
	return out
}

func buildTreeFromMap(r *repo, flat map[string]treeEntry) string {
	// Recursive builder over flat entries.
	var rec func(entries []flatEnt) string
	rec = func(entries []flatEnt) string {
		var treeEnts []treeEntry
		dirs := make(map[string][]flatEnt)
		var dirNames []string
		for _, fe := range entries {
			slash := strings.IndexByte(fe.path, '/')
			if slash < 0 {
				treeEnts = append(treeEnts, treeEntry{mode: fe.ent.mode, name: fe.path, oid: fe.ent.oid})
			} else {
				dir := fe.path[:slash]
				if _, ok := dirs[dir]; !ok {
					dirNames = append(dirNames, dir)
				}
				dirs[dir] = append(dirs[dir], flatEnt{path: fe.path[slash+1:], ent: fe.ent})
			}
		}
		for _, dir := range dirNames {
			treeEnts = append(treeEnts, treeEntry{mode: "40000", name: dir, oid: rec(dirs[dir])})
		}
		return putObject(r, "tree", buildTree(treeEnts))
	}
	var all []flatEnt
	for path, ent := range flat {
		all = append(all, flatEnt{path: path, ent: ent})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].path < all[j].path })
	return rec(all)
}

type flatEnt struct {
	path string
	ent  treeEntry
}

func forgeCommit(r *repo, tree string, parents []string, message string) string {
	timestamp := commitEpoch + db.commitCounter
	db.commitCounter++
	author := viewerName + " <" + viewerEmail + ">"
	data := buildCommitBytes(tree, parents, message, author, timestamp)
	return putObject(r, "commit", data)
}

func countLines(r *repo, oid string) int {
	if oid == "" {
		return 0
	}
	obj := r.objects[oid]
	if obj == nil || obj.typ != "blob" {
		return 0
	}
	text := string(obj.data)
	if text == "" {
		return 0
	}
	n := bytes.Count([]byte(text), []byte("\n"))
	if strings.HasSuffix(text, "\n") {
		return n
	}
	return n + 1
}

func diffFlat(oldMap, newMap map[string]treeEntry, r *repo) []fileDiff {
	var out []fileDiff
	for path, entry := range newMap {
		old, ok := oldMap[path]
		if !ok {
			add := countLines(r, entry.oid)
			out = append(out, fileDiff{filename: path, status: "added", additions: add, changes: add})
		} else if old.oid != entry.oid {
			add := countLines(r, entry.oid)
			del := countLines(r, old.oid)
			out = append(out, fileDiff{filename: path, status: "modified", additions: add, deletions: del, changes: add + del})
		}
	}
	for path, old := range oldMap {
		if _, ok := newMap[path]; !ok {
			del := countLines(r, old.oid)
			out = append(out, fileDiff{filename: path, status: "removed", deletions: del, changes: del})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].filename < out[j].filename })
	return out
}

// threeWayMerge returns (treeOID, conflictPath).
func threeWayMerge(r *repo, baseOID, headOID, mergeBaseOID string) (string, string) {
	conflict := threeWayConflict(r, baseOID, headOID, mergeBaseOID)
	if conflict != "" {
		return "", conflict
	}
	baseTree := parseCommit(r.objects[mergeBaseOID].data).tree
	headTree := parseCommit(r.objects[headOID].data).tree
	tipTree := parseCommit(r.objects[baseOID].data).tree
	baseFlat := flattenTree(r, baseTree)
	headFlat := flattenTree(r, headTree)
	tipFlat := flattenTree(r, tipTree)
	merged := make(map[string]treeEntry, len(tipFlat))
	for k, v := range tipFlat {
		merged[k] = v
	}
	allPaths := make([]string, 0, len(baseFlat)+len(headFlat)+len(tipFlat))
	seen := make(map[string]bool)
	for _, m := range []map[string]treeEntry{baseFlat, headFlat, tipFlat} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				allPaths = append(allPaths, k)
			}
		}
	}
	sort.Strings(allPaths)
	for _, path := range allPaths {
		b, bOk := baseFlat[path]
		h, hOk := headFlat[path]
		var bOid, hOid string
		if bOk {
			bOid = b.oid
		}
		if hOk {
			hOid = h.oid
		}
		if hOid != bOid {
			if !hOk {
				delete(merged, path)
			} else {
				merged[path] = headFlat[path]
			}
		}
	}
	return buildTreeFromMap(r, merged), ""
}

// threeWayConflict reports the first conflicting path (sorted order, so the
// result is deterministic) without building any tree. Used by prMergeable,
// which runs under a read lock and must not mutate the object store.
func threeWayConflict(r *repo, baseOID, headOID, mergeBaseOID string) string {
	baseTree := parseCommit(r.objects[mergeBaseOID].data).tree
	headTree := parseCommit(r.objects[headOID].data).tree
	tipTree := parseCommit(r.objects[baseOID].data).tree
	baseFlat := flattenTree(r, baseTree)
	headFlat := flattenTree(r, headTree)
	tipFlat := flattenTree(r, tipTree)
	allPaths := make([]string, 0, len(baseFlat)+len(headFlat)+len(tipFlat))
	seen := make(map[string]bool)
	for _, m := range []map[string]treeEntry{baseFlat, headFlat, tipFlat} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				allPaths = append(allPaths, k)
			}
		}
	}
	sort.Strings(allPaths)
	for _, path := range allPaths {
		b, bOk := baseFlat[path]
		h, hOk := headFlat[path]
		t, tOk := tipFlat[path]
		var bOid, hOid, tOid string
		if bOk {
			bOid = b.oid
		}
		if hOk {
			hOid = h.oid
		}
		if tOk {
			tOid = t.oid
		}
		headChanged := hOid != bOid
		tipChanged := tOid != bOid
		if headChanged && tipChanged && hOid != tOid {
			return path
		}
	}
	return ""
}

func prMergeable(r *repo, pr *pull) string {
	headTip := branchTip(r, pr.head)
	baseTip := branchTip(r, pr.base)
	if headTip == "" || baseTip == "" {
		return "UNKNOWN"
	}
	base := mergeBase(headTip, baseTip, func(oid string) *gitObject { return r.objects[oid] })
	if base == "" {
		return "UNKNOWN"
	}
	if conflict := threeWayConflict(r, baseTip, headTip, base); conflict != "" {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

func computePrFiles(r *repo, pr *pull) []fileDiff {
	headTip := branchTip(r, pr.head)
	baseTip := branchTip(r, pr.base)
	if headTip == "" || baseTip == "" {
		return nil
	}
	base := mergeBase(headTip, baseTip, func(oid string) *gitObject { return r.objects[oid] })
	if base == "" {
		return nil
	}
	baseTree := parseCommit(r.objects[base].data).tree
	headTree := parseCommit(r.objects[headTip].data).tree
	return diffFlat(flattenTree(r, baseTree), flattenTree(r, headTree), r)
}

func makeIssue(r *repo, title, body string, labelNames []string) *issue {
	number := r.nextNumber
	r.nextNumber++
	db.globalIDCounter++
	id := int(db.globalIDCounter) + 100000
	iss := &issue{
		number: number, id: id,
		nodeID: globalID("issue", r.owner+"/"+r.name+"#"+intStr(number)),
		title:  title, body: body, state: "open",
	}
	for _, name := range labelNames {
		if l, ok := r.labels[name]; ok {
			iss.labels = append(iss.labels, l)
		}
	}
	r.issues = append(r.issues, iss)
	r.issueByNumber[number] = iss
	return iss
}

func makePull(r *repo, title, body, head, base string) *pull {
	number := r.nextNumber
	r.nextNumber++
	db.globalIDCounter++
	id := int(db.globalIDCounter) + 100000
	pr := &pull{
		number: number, id: id,
		nodeID: globalID("pr", r.owner+"/"+r.name+"#"+intStr(number)),
		title: title, body: body, head: head, base: base, state: "open",
	}
	pr.files = computePrFiles(r, pr)
	r.pulls = append(r.pulls, pr)
	r.prByNumber[number] = pr
	return pr
}

// doMerge returns (status, sha, message).
func doMerge(r *repo, pr *pull) (int, string, string) {
	headTip := branchTip(r, pr.head)
	baseTip := branchTip(r, pr.base)
	if headTip == "" || baseTip == "" {
		return 409, "", "missing branch tip"
	}
	base := mergeBase(headTip, baseTip, func(oid string) *gitObject { return r.objects[oid] })
	if base == "" {
		return 409, "", "no merge base"
	}
	tree, conflict := threeWayMerge(r, baseTip, headTip, base)
	if conflict != "" {
		return 405, "", "merge conflict on " + conflict
	}
	sha := forgeCommit(r, tree, []string{baseTip}, "Merge pull request #"+intStr(pr.number)+" from "+pr.head+"\n\n"+pr.title)
	r.setRef(branchRef(pr.base), sha)
	pr.state = "closed"
	pr.merged = true
	pr.mergeCommitSha = sha
	return 200, sha, ""
}

func shortBranch(ref string) string {
	if ref == "" {
		return ""
	}
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}
