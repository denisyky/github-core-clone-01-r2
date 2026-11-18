// GraphQL v4: hand-written parser + executor for the documents gh and
// github-mcp-server send. Faithful port of the Node forge.

package main

import (
	"errors"
	"strconv"
	"strings"
)

var gqlFields = map[string][]string{
	"Query":                  {"viewer", "repository", "node", "search", "__type", "__schema"},
	"User":                   {"login", "id", "name", "email"},
	"Repository":             {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "url", "description", "private"},
	"Issue":                  {"id", "number", "title", "body", "state", "createdAt", "updatedAt", "url", "author", "labels", "comments"},
	"PullRequest":            {"id", "number", "title", "body", "state", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "mergeable", "merged", "files", "createdAt", "updatedAt", "url", "author"},
	"Ref":                    {"name", "target"},
	"IssueConnection":        {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestConnection":  {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestFileConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"PageInfo":               {"hasNextPage", "hasPreviousPage", "endCursor", "startCursor"},
	"IssueEdge":              {"node", "cursor"},
	"PullRequestEdge":        {"node", "cursor"},
	"PullRequestFile":        {"path", "additions", "deletions", "changeType"},
	"Comment":                {"id", "body", "createdAt"},
	"CommentEdge":            {"node", "cursor"},
	"AddCommentPayload":      {"commentEdge", "clientMutationId"},
	"CreateIssuePayload":     {"issue", "clientMutationId"},
	"CloseIssuePayload":      {"issue", "clientMutationId"},
	"CreatePullRequestPayload": {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":  {"pullRequest", "clientMutationId"},
	"SearchType":             {},
	"__Type":                 {"name", "kind", "fields", "enumValues", "description"},
	"__Field":                {"name", "description", "type", "args"},
	"__EnumValue":            {"name", "description"},
	"__Schema":               {"queryType", "mutationType", "types", "directives"},
}

var gqlEnumSearchType = []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "CODE", "COMMIT"}

var gqlTypeKind = map[string]string{
	"Query": "OBJECT", "User": "OBJECT", "Repository": "OBJECT", "Issue": "OBJECT",
	"PullRequest": "OBJECT", "Ref": "OBJECT", "IssueConnection": "OBJECT",
	"PullRequestConnection": "OBJECT", "PageInfo": "OBJECT", "SearchType": "ENUM",
}

// --- tokenizer ---

type gqlTok struct {
	t string // "name" | "string" | "int" | "eof" | punctuation char | "..."
	v string
	n int64
}

func gqlTokenize(src string) ([]gqlTok, error) {
	var toks []gqlTok
	i := 0
	for i < len(src) {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if strings.HasPrefix(src[i:], "...") {
			toks = append(toks, gqlTok{t: "...", v: "..."})
			i += 3
			continue
		}
		if strings.IndexByte("{}():!$=[]@", c) >= 0 {
			toks = append(toks, gqlTok{t: string(c), v: string(c)})
			i++
			continue
		}
		if c == '"' {
			j := i + 1
			var out strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					out.WriteByte(src[j+1])
					j += 2
				} else {
					out.WriteByte(src[j])
					j++
				}
			}
			toks = append(toks, gqlTok{t: "string", v: out.String()})
			i = j + 1
			continue
		}
		if (c >= '0' && c <= '9') || (c == '-' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9') {
			j := i
			if src[j] == '-' {
				j++
			}
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			n, _ := strconv.ParseInt(src[i:j], 10, 64)
			toks = append(toks, gqlTok{t: "int", v: src[i:j], n: n})
			i = j
			continue
		}
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' {
			j := i
			for j < len(src) && ((src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= 'a' && src[j] <= 'z') || (src[j] >= '0' && src[j] <= '9') || src[j] == '_') {
				j++
			}
			toks = append(toks, gqlTok{t: "name", v: src[i:j]})
			i = j
			continue
		}
		return nil, errors.New("unexpected character " + strconv.Quote(string(c)))
	}
	toks = append(toks, gqlTok{t: "eof"})
	return toks, nil
}

// --- parser ---

// gqlVal is a parsed argument value node.
type gqlVal struct {
	isVar bool
	name  string
	lit   any // string | int64 | bool | nil | []gqlVal | map[string]gqlVal
}

type gqlField struct {
	alias     string
	name      string
	args      map[string]gqlVal
	selection []*gqlField
	fragment  bool
	typeName  string // inline fragment type condition
	spread    string // named fragment spread (ignored, no named fragments)
}

type gqlOp struct {
	kind     string // query | mutation | subscription
	name     string
	varDefs  map[string]gqlVarDef
	selection []*gqlField
}

type gqlVarDef struct {
	typeName string
	required bool
	def      *gqlVal
	hasDef   bool
}

type gqlParser struct {
	toks []gqlTok
	pos  int
}

func (p *gqlParser) peek() gqlTok  { return p.toks[p.pos] }
func (p *gqlParser) next() gqlTok  { t := p.toks[p.pos]; p.pos++; return t }
func (p *gqlParser) expect(t string) (gqlTok, error) {
	tok := p.next()
	if tok.t != t {
		return tok, errors.New("expected " + t + ", got " + tok.t + " " + tok.v)
	}
	return tok, nil
}

func gqlParse(src string) ([]*gqlOp, error) {
	toks, err := gqlTokenize(src)
	if err != nil {
		return nil, err
	}
	p := &gqlParser{toks: toks}
	var ops []*gqlOp
	for p.peek().t != "eof" {
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func (p *gqlParser) parseValue() (gqlVal, error) {
	tok := p.peek()
	switch tok.t {
	case "$":
		p.next()
		name, err := p.expect("name")
		if err != nil {
			return gqlVal{}, err
		}
		return gqlVal{isVar: true, name: name.v}, nil
	case "int":
		p.next()
		return gqlVal{lit: tok.n}, nil
	case "string":
		p.next()
		return gqlVal{lit: tok.v}, nil
	case "name":
		p.next()
		switch tok.v {
		case "true":
			return gqlVal{lit: true}, nil
		case "false":
			return gqlVal{lit: false}, nil
		case "null":
			return gqlVal{lit: nil}, nil
		}
		return gqlVal{lit: tok.v}, nil
	case "[":
		p.next()
		var items []gqlVal
		for p.peek().t != "]" {
			v, err := p.parseValue()
			if err != nil {
				return gqlVal{}, err
			}
			items = append(items, v)
		}
		if _, err := p.expect("]"); err != nil {
			return gqlVal{}, err
		}
		return gqlVal{lit: items}, nil
	case "{":
		p.next()
		obj := make(map[string]gqlVal)
		for p.peek().t != "}" {
			key, err := p.expect("name")
			if err != nil {
				return gqlVal{}, err
			}
			if _, err := p.expect(":"); err != nil {
				return gqlVal{}, err
			}
			v, err := p.parseValue()
			if err != nil {
				return gqlVal{}, err
			}
			obj[key.v] = v
		}
		if _, err := p.expect("}"); err != nil {
			return gqlVal{}, err
		}
		return gqlVal{lit: obj}, nil
	}
	return gqlVal{}, errors.New("unexpected value token " + tok.t)
}

func (p *gqlParser) parseArgs() (map[string]gqlVal, error) {
	args := make(map[string]gqlVal)
	if p.peek().t != "(" {
		return args, nil
	}
	p.next()
	for p.peek().t != ")" {
		key, err := p.expect("name")
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[key.v] = v
	}
	if _, err := p.expect(")"); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlField, error) {
	if _, err := p.expect("{"); err != nil {
		return nil, err
	}
	var fields []*gqlField
	for p.peek().t != "}" {
		if p.peek().t == "..." {
			p.next()
			if p.peek().t == "name" && p.peek().v == "on" {
				p.next()
				typeName, err := p.expect("name")
				if err != nil {
					return nil, err
				}
				sub, err := p.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				fields = append(fields, &gqlField{fragment: true, typeName: typeName.v, selection: sub})
			} else {
				fragName, err := p.expect("name")
				if err != nil {
					return nil, err
				}
				fields = append(fields, &gqlField{fragment: true, spread: fragName.v})
			}
			continue
		}
		name, err := p.expect("name")
		if err != nil {
			return nil, err
		}
		alias := ""
		fname := name.v
		if p.peek().t == ":" {
			p.next()
			alias = fname
			realName, err := p.expect("name")
			if err != nil {
				return nil, err
			}
			fname = realName.v
		}
		if alias == "" {
			alias = fname
		}
		args, err := p.parseArgs()
		if err != nil {
			return nil, err
		}
		var selection []*gqlField
		if p.peek().t == "{" {
			selection, err = p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
		}
		fields = append(fields, &gqlField{alias: alias, name: fname, args: args, selection: selection})
	}
	if _, err := p.expect("}"); err != nil {
		return nil, err
	}
	return fields, nil
}

func (p *gqlParser) parseOperation() (*gqlOp, error) {
	op := &gqlOp{kind: "query", varDefs: make(map[string]gqlVarDef)}
	if p.peek().t == "name" && (p.peek().v == "query" || p.peek().v == "mutation" || p.peek().v == "subscription") {
		op.kind = p.next().v
		if p.peek().t == "name" {
			op.name = p.next().v
		}
	}
	if p.peek().t == "(" {
		p.next()
		for p.peek().t != ")" {
			if p.peek().t == "$" {
				p.next()
			}
			vname, err := p.expect("name")
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(":"); err != nil {
				return nil, err
			}
			var typeName string
			if p.peek().t == "[" {
				p.next()
				inner, err := p.expect("name")
				if err != nil {
					return nil, err
				}
				typeName = "[" + inner.v + "]"
				if _, err := p.expect("]"); err != nil {
					return nil, err
				}
			} else {
				tn, err := p.expect("name")
				if err != nil {
					return nil, err
				}
				typeName = tn.v
			}
			def := gqlVarDef{typeName: typeName}
			if p.peek().t == "!" {
				p.next()
				def.required = true
			}
			if p.peek().t == "=" {
				p.next()
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				def.def = &v
				def.hasDef = true
			}
			op.varDefs[vname.v] = def
		}
		if _, err := p.expect(")"); err != nil {
			return nil, err
		}
	}
	for p.peek().t == "@" {
		p.next()
		if _, err := p.expect("name"); err != nil {
			return nil, err
		}
		if _, err := p.parseArgs(); err != nil {
			return nil, err
		}
	}
	sel, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.selection = sel
	return op, nil
}

// --- value resolution ---

func gqlResolveValue(node gqlVal, variables map[string]JVal) JVal {
	if node.isVar {
		if v, ok := variables[node.name]; ok {
			return v
		}
		return nil
	}
	switch lit := node.lit.(type) {
	case []gqlVal:
		out := make([]JVal, len(lit))
		for i, e := range lit {
			out[i] = gqlResolveValue(e, variables)
		}
		return out
	case map[string]gqlVal:
		out := make(map[string]JVal, len(lit))
		for k, v := range lit {
			out[k] = gqlResolveValue(v, variables)
		}
		return out
	default:
		return lit
	}
}

func gqlArgs(f *gqlField, variables map[string]JVal) map[string]JVal {
	out := make(map[string]JVal, len(f.args))
	for k, v := range f.args {
		out[k] = gqlResolveValue(v, variables)
	}
	return out
}

// --- executor ---

// gqlNode wraps a domain value with its GraphQL type.
type gqlNode struct {
	typename string
	value    any // *repo | *issue | *pull | map | string etc.
	repo     *repo
	iss      *issue
	pr       *pull
}

func issueStateGQL(i *issue) string {
	if i.state == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func prStateGQL(pr *pull) string {
	if pr.merged {
		return "MERGED"
	}
	if pr.state == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func gqlIssueNodes(r *repo, args map[string]JVal) []*issue {
	list := append([]*issue{}, r.issues...)
	if states, ok := args["states"].([]JVal); ok && len(states) > 0 {
		want := make(map[string]bool)
		for _, s := range states {
			want[strings.ToLower(jStr(s))] = true
		}
		var filtered []*issue
		for _, i := range list {
			if want[i.state] {
				filtered = append(filtered, i)
			}
		}
		list = filtered
	}
	if ob, ok := args["orderBy"].(map[string]JVal); ok {
		if field, _ := ob["field"].(string); field == "CREATED_AT" {
			dir := 1
			if d, _ := ob["direction"].(string); d == "ASC" {
				dir = 1
			} else {
				dir = -1
			}
			for i := 1; i < len(list); i++ {
				for j := i; j > 0 && dir*cmpInt(list[j-1].number, list[j].number) > 0; j-- {
					list[j-1], list[j] = list[j], list[j-1]
				}
			}
			return list
		}
	}
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j-1].number > list[j].number; j-- {
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
	return list
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

type gqlConn struct {
	nodes      []*gqlNode
	pageInfo   map[string]JVal
	totalCount int
}

func gqlConnection(nodes []*gqlNode, numbers []int, args map[string]JVal) gqlConn {
	afterIdx := -1
	if after, ok := args["after"].(string); ok && after != "" {
		if decoded, err := b64URLDecode(after); err == nil {
			if strings.HasPrefix(decoded, "cursor:") {
				if n, err := strconv.Atoi(decoded[len("cursor:"):]); err == nil {
					for i, num := range numbers {
						if num == n {
							afterIdx = i
							break
						}
					}
				}
			}
		}
	}
	rest := nodes
	restNums := numbers
	if afterIdx >= 0 {
		rest = nodes[afterIdx+1:]
		restNums = numbers[afterIdx+1:]
	}
	_ = restNums
	first := len(rest)
	if f, ok := args["first"]; ok {
		if n, ok := jInt(f); ok {
			first = int(n)
		}
	}
	if first > len(rest) {
		first = len(rest)
	}
	if first < 0 {
		first = 0
	}
	page := rest[:first]
	hasNext := len(rest) > len(page)
	var endCursor, startCursor JVal
	if len(page) > 0 {
		endCursor = b64URLEncode("cursor:" + intStr(numbers[afterIdx+1+len(page)-1]))
		startCursor = b64URLEncode("cursor:" + intStr(numbers[afterIdx+1]))
	}
	return gqlConn{
		nodes: page,
		pageInfo: map[string]JVal{
			"hasNextPage":     hasNext,
			"hasPreviousPage": afterIdx >= 0,
			"endCursor":       endCursor,
			"startCursor":     startCursor,
		},
		totalCount: len(nodes),
	}
}

func gqlResolveGlobalID(id string) (kind, payload string, ok bool) {
	if id == "" {
		return "", "", false
	}
	text, err := b64URLDecode(id)
	if err != nil {
		return "", "", false
	}
	colon := strings.IndexByte(text, ':')
	if colon < 0 {
		return "", "", false
	}
	kind = text[:colon]
	payload = text[colon+1:]
	if kind != "repo" && kind != "issue" && kind != "pr" && kind != "comment" {
		return "", "", false
	}
	return kind, payload, true
}

// execFields executes fields against a node, writing into out (ordered JSON
// object builder). opErrors collects unknown-field errors.
func execFields(node *gqlNode, fields []*gqlField, out *orderedObj, opErrors *[]string, variables map[string]JVal) {
	for _, field := range fields {
		if field.fragment {
			if field.spread != "" {
				continue
			}
			if field.typeName != "" && field.typeName != node.typename {
				continue
			}
			execFields(node, field.selection, out, opErrors, variables)
			continue
		}
		if field.name == "__typename" {
			out.set(field.alias, node.typename)
			continue
		}
		allowed := gqlFields[node.typename]
		found := false
		for _, a := range allowed {
			if a == field.name {
				found = true
				break
			}
		}
		if !found {
			*opErrors = append(*opErrors, "Cannot query field \""+field.name+"\" on type \""+node.typename+"\".")
			continue
		}
		args := gqlArgs(field, variables)
		value := gqlResolveField(node, field, args, field.selection, opErrors, variables)
		if value == nil {
			out.set(field.alias, nil)
		} else {
			out.set(field.alias, value)
		}
	}
}

func gqlResolveField(node *gqlNode, field *gqlField, args map[string]JVal, selection []*gqlField, opErrors *[]string, variables map[string]JVal) any {
	t := node.typename
	resolveSub := func(child *gqlNode) any {
		if selection == nil {
			return nil
		}
		out := newOrderedObj()
		execFields(child, selection, out, opErrors, variables)
		return out
	}
	switch t {
	case "Query":
		switch field.name {
		case "viewer":
			return resolveSub(&gqlNode{typename: "User", value: viewerLogin})
		case "repository":
			r := db.getRepo(jStr(args["owner"]), jStr(args["name"]))
			if r == nil {
				return nil
			}
			return resolveSub(&gqlNode{typename: "Repository", value: r, repo: r})
		case "node":
			kind, payload, ok := gqlResolveGlobalID(jStr(args["id"]))
			if !ok {
				return nil
			}
			switch kind {
			case "repo":
				parts := strings.SplitN(payload, "/", 2)
				if len(parts) != 2 {
					return nil
				}
				r := db.getRepo(parts[0], parts[1])
				if r == nil {
					return nil
				}
				return resolveSub(&gqlNode{typename: "Repository", value: r, repo: r})
			case "issue", "pr":
				m := splitGlobalIssue(payload)
				if !m.ok {
					return nil
				}
				r := db.getRepo(m.owner, m.name)
				if r == nil {
					return nil
				}
				if kind == "issue" {
					iss := r.issueByNumber[m.num]
					if iss == nil {
						return nil
					}
					return resolveSub(&gqlNode{typename: "Issue", value: iss, repo: r, iss: iss})
				}
				pr := r.prByNumber[m.num]
				if pr == nil {
					return nil
				}
				return resolveSub(&gqlNode{typename: "PullRequest", value: pr, repo: r, pr: pr})
			}
			return nil
		case "__type":
			name := jStr(args["name"])
			kind, ok := gqlTypeKind[name]
			if !ok {
				kind = "OBJECT"
			}
			var fields []map[string]any
			for _, n := range gqlFields[name] {
				fields = append(fields, map[string]any{"name": n})
			}
			var enumValues []map[string]any
			if name == "SearchType" {
				for _, n := range gqlEnumSearchType {
					enumValues = append(enumValues, map[string]any{"name": n})
				}
			}
			tn := &gqlNode{typename: "__Type", value: map[string]any{"name": name, "kind": kind, "fields": fields, "enumValues": enumValues}}
			if selection == nil {
				return map[string]any{"name": name, "kind": kind}
			}
			return resolveSub(tn)
		case "search":
			return map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}
		case "__schema":
			return resolveSub(&gqlNode{typename: "__Schema", value: nil})
		}
		return nil
	case "__Schema":
		switch field.name {
		case "queryType":
			var fields []map[string]any
			for _, n := range gqlFields["Query"] {
				fields = append(fields, map[string]any{"name": n})
			}
			return resolveSub(&gqlNode{typename: "__Type", value: map[string]any{"name": "Query", "kind": "OBJECT", "fields": fields, "enumValues": []map[string]any{}}})
		case "mutationType":
			return resolveSub(&gqlNode{typename: "__Type", value: map[string]any{"name": "Mutation", "kind": "OBJECT", "fields": []map[string]any{}, "enumValues": []map[string]any{}}})
		case "types":
			return []any{}
		case "directives":
			return []any{}
		}
		return nil
	case "__Type":
		v := node.value.(map[string]any)
		switch field.name {
		case "fields":
			var out []any
			for _, f := range v["fields"].([]map[string]any) {
				out = append(out, resolveSub(&gqlNode{typename: "__Field", value: f}))
			}
			return out
		case "enumValues":
			var out []any
			for _, e := range v["enumValues"].([]map[string]any) {
				out = append(out, resolveSub(&gqlNode{typename: "__EnumValue", value: e}))
			}
			return out
		case "name":
			return v["name"]
		case "kind":
			return v["kind"]
		}
		return nil
	case "__Field", "__EnumValue":
		v := node.value.(map[string]any)
		if field.name == "name" {
			return v["name"]
		}
		return nil
	case "User":
		switch field.name {
		case "login":
			return node.value.(string)
		case "id":
			return globalID("user", node.value.(string))
		case "name":
			if node.value.(string) == viewerLogin {
				return viewerName
			}
			return node.value.(string)
		case "email":
			if node.value.(string) == viewerLogin {
				return viewerEmail
			}
			return ""
		}
		return nil
	case "Repository":
		r := node.value.(*repo)
		switch field.name {
		case "id":
			return globalID("repo", r.owner+"/"+r.name)
		case "databaseId":
			return int64(r.id)
		case "name":
			return r.name
		case "nameWithOwner":
			return r.owner + "/" + r.name
		case "owner":
			return resolveSub(&gqlNode{typename: "User", value: r.owner})
		case "defaultBranchRef":
			return resolveSub(&gqlNode{typename: "Ref", value: map[string]any{"name": r.defaultBranch}})
		case "viewerPermission":
			return "ADMIN"
		case "hasIssuesEnabled":
			return true
		case "description":
			return r.description
		case "private":
			return r.private
		case "url":
			return "https://example.invalid/" + r.owner + "/" + r.name
		case "issue":
			n, _ := jInt(args["number"])
			iss := r.issueByNumber[int(n)]
			if iss == nil {
				return nil
			}
			return resolveSub(&gqlNode{typename: "Issue", value: iss, repo: r, iss: iss})
		case "issues":
			list := gqlIssueNodes(r, args)
			var nodes []*gqlNode
			var numbers []int
			for _, iss := range list {
				nodes = append(nodes, &gqlNode{typename: "Issue", value: iss, repo: r, iss: iss})
				numbers = append(numbers, iss.number)
			}
			conn := gqlConnection(nodes, numbers, args)
			return resolveConnection(conn, selection, opErrors, variables)
		case "pullRequests":
			list := append([]*pull{}, r.pulls...)
			if states, ok := args["states"].([]JVal); ok && len(states) > 0 {
				want := make(map[string]bool)
				for _, s := range states {
					want[strings.ToUpper(jStr(s))] = true
				}
				var filtered []*pull
				for _, pr := range list {
					if want[prStateGQL(pr)] {
						filtered = append(filtered, pr)
					}
				}
				list = filtered
			}
			for i := 1; i < len(list); i++ {
				for j := i; j > 0 && list[j-1].number < list[j].number; j-- {
					list[j-1], list[j] = list[j], list[j-1]
				}
			}
			var nodes []*gqlNode
			var numbers []int
			for _, pr := range list {
				nodes = append(nodes, &gqlNode{typename: "PullRequest", value: pr, repo: r, pr: pr})
				numbers = append(numbers, pr.number)
			}
			conn := gqlConnection(nodes, numbers, args)
			return resolveConnection(conn, selection, opErrors, variables)
		case "pullRequest":
			n, _ := jInt(args["number"])
			pr := r.prByNumber[int(n)]
			if pr == nil {
				return nil
			}
			return resolveSub(&gqlNode{typename: "PullRequest", value: pr, repo: r, pr: pr})
		}
		return nil
	case "Issue":
		iss := node.value.(*issue)
		r := node.repo
		switch field.name {
		case "id":
			return iss.nodeID
		case "number":
			return int64(iss.number)
		case "title":
			return iss.title
		case "body":
			return iss.body
		case "state":
			return issueStateGQL(iss)
		case "createdAt", "updatedAt":
			return fixedDate
		case "url":
			return "https://example.invalid/" + r.owner + "/" + r.name + "/issues/" + intStr(iss.number)
		case "author":
			return resolveSub(&gqlNode{typename: "User", value: viewerLogin})
		case "labels":
			var nodes []any
			for _, l := range iss.labels {
				nodes = append(nodes, map[string]any{"name": l.name})
			}
			if nodes == nil {
				nodes = []any{}
			}
			return map[string]any{"nodes": nodes}
		case "comments":
			var nodes []any
			for _, c := range iss.comments {
				nodes = append(nodes, &gqlNode{typename: "Comment", value: c})
			}
			if nodes == nil {
				nodes = []any{}
			}
			return map[string]any{"nodes": nodes}
		}
		return nil
	case "PullRequest":
		pr := node.value.(*pull)
		r := node.repo
		switch field.name {
		case "id":
			return pr.nodeID
		case "number":
			return int64(pr.number)
		case "title":
			return pr.title
		case "body":
			return pr.body
		case "state":
			return prStateGQL(pr)
		case "headRefName":
			return pr.head
		case "baseRefName":
			return pr.base
		case "headRefOid":
			if tip := branchTip(r, pr.head); tip != "" {
				return tip
			}
			return zeroOID
		case "baseRefOid":
			if tip := branchTip(r, pr.base); tip != "" {
				return tip
			}
			return zeroOID
		case "mergeable":
			return prMergeable(r, pr)
		case "merged":
			return pr.merged
		case "createdAt", "updatedAt":
			return fixedDate
		case "url":
			return "https://example.invalid/" + r.owner + "/" + r.name + "/pull/" + intStr(pr.number)
		case "author":
			return resolveSub(&gqlNode{typename: "User", value: viewerLogin})
		case "files":
			files := pr.files
			if files == nil {
				files = computePrFiles(r, pr)
			}
			var nodes []*gqlNode
			var numbers []int
			for i, f := range files {
				nodes = append(nodes, &gqlNode{typename: "PullRequestFile", value: f})
				numbers = append(numbers, i)
			}
			conn := gqlConn{
				nodes:      nodes,
				pageInfo:   map[string]JVal{"hasNextPage": false, "hasPreviousPage": false, "endCursor": nil, "startCursor": nil},
				totalCount: len(nodes),
			}
			_ = numbers
			return resolveConnection(conn, selection, opErrors, variables)
		}
		return nil
	case "Ref":
		v := node.value.(map[string]any)
		switch field.name {
		case "name":
			return v["name"]
		case "target":
			return nil
		}
		return nil
	case "PullRequestFile":
		f := node.value.(fileDiff)
		switch field.name {
		case "path":
			return f.filename
		case "additions":
			return int64(f.additions)
		case "deletions":
			return int64(f.deletions)
		case "changeType":
			switch f.status {
			case "added":
				return "ADDED"
			case "removed":
				return "DELETED"
			default:
				return "MODIFIED"
			}
		}
		return nil
	case "Comment":
		c := node.value.(*comment)
		switch field.name {
		case "id":
			return c.nodeID
		case "body":
			return c.body
		case "createdAt":
			return fixedDate
		}
		return nil
	}
	return nil
}

// splitGlobalIssue parses "owner/name#123".
type globalIssueRef struct {
	owner string
	name  string
	num   int
	ok    bool
}

func splitGlobalIssue(payload string) globalIssueRef {
	hash := strings.LastIndexByte(payload, '#')
	if hash < 0 {
		return globalIssueRef{}
	}
	ownerRepo := payload[:hash]
	num, err := strconv.Atoi(payload[hash+1:])
	if err != nil {
		return globalIssueRef{}
	}
	slash := strings.IndexByte(ownerRepo, '/')
	if slash < 0 {
		return globalIssueRef{}
	}
	return globalIssueRef{owner: ownerRepo[:slash], name: ownerRepo[slash+1:], num: num, ok: true}
}

func resolveConnection(conn gqlConn, selection []*gqlField, opErrors *[]string, variables map[string]JVal) any {
	if selection == nil {
		nulls := make([]any, len(conn.nodes))
		return map[string]any{"nodes": nulls}
	}
	out := newOrderedObj()
	for _, field := range selection {
		if field.fragment {
			continue
		}
		switch field.name {
		case "nodes":
			var arr []any
			for _, n := range conn.nodes {
				o := newOrderedObj()
				execFields(n, field.selection, o, opErrors, variables)
				arr = append(arr, o)
			}
			if arr == nil {
				arr = []any{}
			}
			out.set(field.alias, arr)
		case "edges":
			var arr []any
			for _, n := range conn.nodes {
				o := newOrderedObj()
				for _, ef := range field.selection {
					if ef.name == "node" {
						no := newOrderedObj()
						execFields(n, ef.selection, no, opErrors, variables)
						o.set(ef.alias, no)
					} else if ef.name == "cursor" {
						num := 0
						if iss, ok := n.value.(*issue); ok {
							num = iss.number
						} else if pr, ok := n.value.(*pull); ok {
							num = pr.number
						}
						o.set(ef.alias, b64URLEncode("cursor:"+intStr(num)))
					}
				}
				arr = append(arr, o)
			}
			if arr == nil {
				arr = []any{}
			}
			out.set(field.alias, arr)
		case "pageInfo":
			o := newOrderedObj()
			for _, pf := range field.selection {
				o.set(pf.alias, conn.pageInfo[pf.name])
			}
			out.set(field.alias, o)
		case "totalCount":
			out.set(field.alias, int64(conn.totalCount))
		default:
			out.set(field.alias, nil)
		}
	}
	return out
}

// --- mutations ---

func execMutationFields(fields []*gqlField, out *orderedObj, opErrors *[]string, variables map[string]JVal) {
	for _, field := range fields {
		if field.fragment {
			continue
		}
		args := gqlArgs(field, variables)
		input, _ := args["input"].(map[string]JVal)
		if input == nil {
			input = map[string]JVal{}
		}
		wrap := func(payload map[string]*gqlNode, commentEdge *gqlNode) any {
			if field.selection == nil {
				return newOrderedObj()
			}
			o := newOrderedObj()
			for _, sf := range field.selection {
				if sf.fragment {
					continue
				}
				switch sf.name {
				case "issue", "pullRequest":
					target := payload[sf.name]
					if target == nil {
						o.set(sf.alias, nil)
						continue
					}
					so := newOrderedObj()
					execFields(target, sf.selection, so, opErrors, variables)
					o.set(sf.alias, so)
				case "commentEdge":
					if commentEdge == nil {
						o.set(sf.alias, nil)
						continue
					}
					eo := newOrderedObj()
					for _, ef := range sf.selection {
						if ef.name == "node" {
							no := newOrderedObj()
							execFields(commentEdge, ef.selection, no, opErrors, variables)
							eo.set(ef.alias, no)
						}
					}
					o.set(sf.alias, eo)
				case "clientMutationId":
					o.set(sf.alias, input["clientMutationId"])
				default:
					o.set(sf.alias, nil)
				}
			}
			return o
		}
		errMsg := ""
		var payload map[string]*gqlNode
		var commentEdge *gqlNode
		switch field.name {
		case "createIssue":
			kind, pl, ok := gqlResolveGlobalID(jStr(input["repositoryId"]))
			if !ok || kind != "repo" {
				errMsg = "unknown repository"
				break
			}
			parts := strings.SplitN(pl, "/", 2)
			if len(parts) != 2 {
				errMsg = "unknown repository"
				break
			}
			r := db.getRepo(parts[0], parts[1])
			if r == nil {
				errMsg = "unknown repository"
				break
			}
			iss := makeIssue(r, jStr(input["title"]), jStr(input["body"]), nil)
			payload = map[string]*gqlNode{"issue": {typename: "Issue", value: iss, repo: r, iss: iss}}
		case "closeIssue":
			kind, pl, ok := gqlResolveGlobalID(jStr(input["issueId"]))
			if !ok || kind != "issue" {
				errMsg = "unknown issue"
				break
			}
			m := splitGlobalIssue(pl)
			if !m.ok {
				errMsg = "unknown issue"
				break
			}
			r := db.getRepo(m.owner, m.name)
			var iss *issue
			if r != nil {
				iss = r.issueByNumber[m.num]
			}
			if iss == nil {
				errMsg = "unknown issue"
				break
			}
			iss.state = "closed"
			iss.invalidate()
			payload = map[string]*gqlNode{"issue": {typename: "Issue", value: iss, repo: r, iss: iss}}
		case "createPullRequest":
			kind, pl, ok := gqlResolveGlobalID(jStr(input["repositoryId"]))
			if !ok || kind != "repo" {
				errMsg = "unknown repository"
				break
			}
			parts := strings.SplitN(pl, "/", 2)
			if len(parts) != 2 {
				errMsg = "unknown repository"
				break
			}
			r := db.getRepo(parts[0], parts[1])
			if r == nil {
				errMsg = "unknown repository"
				break
			}
			head := shortBranch(jStr(input["headRefName"]))
			base := shortBranch(jStr(input["baseRefName"]))
			if base == "" {
				base = r.defaultBranch
			}
			if branchTip(r, head) == "" {
				errMsg = "head branch does not exist"
				break
			}
			var pr *pull
			for _, p := range r.pulls {
				if p.head == head && p.base == base && p.state == "open" {
					pr = p
					break
				}
			}
			if pr == nil {
				pr = makePull(r, jStr(input["title"]), jStr(input["body"]), head, base)
			}
			payload = map[string]*gqlNode{"pullRequest": {typename: "PullRequest", value: pr, repo: r, pr: pr}}
		case "mergePullRequest":
			kind, pl, ok := gqlResolveGlobalID(jStr(input["pullRequestId"]))
			if !ok || kind != "pr" {
				errMsg = "unknown pull request"
				break
			}
			m := splitGlobalIssue(pl)
			if !m.ok {
				errMsg = "unknown pull request"
				break
			}
			r := db.getRepo(m.owner, m.name)
			var pr *pull
			if r != nil {
				pr = r.prByNumber[m.num]
			}
			if pr == nil {
				errMsg = "unknown pull request"
				break
			}
			if pr.state == "open" {
				status, _, msg := doMerge(r, pr)
				if status != 200 {
					errMsg = msg
					break
				}
			}
			payload = map[string]*gqlNode{"pullRequest": {typename: "PullRequest", value: pr, repo: r, pr: pr}}
		case "addComment":
			kind, pl, ok := gqlResolveGlobalID(jStr(input["subjectId"]))
			if !ok || kind != "issue" {
				errMsg = "unknown subject"
				break
			}
			m := splitGlobalIssue(pl)
			if !m.ok {
				errMsg = "unknown issue"
				break
			}
			r := db.getRepo(m.owner, m.name)
			var iss *issue
			if r != nil {
				iss = r.issueByNumber[m.num]
			}
			if iss == nil {
				errMsg = "unknown issue"
				break
			}
			id := r.nextCommentID
			r.nextCommentID++
			c := &comment{id: id, nodeID: globalID("comment", m.owner+"/"+m.name+"#"+intStr(m.num)+":"+intStr(r.nextCommentID)), body: jStr(input["body"])}
			iss.comments = append(iss.comments, c)
			iss.invalidate()
			commentEdge = &gqlNode{typename: "Comment", value: c}
		default:
			*opErrors = append(*opErrors, "Cannot query field \""+field.name+"\" on type \"Mutation\".")
			out.set(field.alias, nil)
			continue
		}
		if errMsg != "" {
			*opErrors = append(*opErrors, errMsg)
			out.set(field.alias, nil)
			continue
		}
		out.set(field.alias, wrap(payload, commentEdge))
	}
}

// --- ordered JSON object (preserves insertion order) ---

type orderedObj struct {
	keys []string
	vals map[string]any
}

func newOrderedObj() *orderedObj {
	return &orderedObj{vals: make(map[string]any)}
}

func (o *orderedObj) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *orderedObj) bytes() []byte {
	var b []byte
	b = append(b, '{')
	for i, k := range o.keys {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendJSONString(b, k)
		b = append(b, ':')
		b = appendGQLVal(b, o.vals[k])
	}
	b = append(b, '}')
	return b
}

// appendGQLVal serializes executor values: nil, bool, string, int64,
// []any, map[string]any, *orderedObj, *gqlNode (should not occur), JVal.
func appendGQLVal(dst []byte, v any) []byte {
	switch n := v.(type) {
	case nil:
		return append(dst, "null"...)
	case *orderedObj:
		return append(dst, n.bytes()...)
	case map[string]any:
		dst = append(dst, '{')
		i := 0
		// map iteration order is random; GraphQL object fields here are
		// fixed-shape payloads (search/__type no-selection) — order does not
		// matter to the suite, which parses JSON.
		for k, e := range n {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONString(dst, k)
			dst = append(dst, ':')
			dst = appendGQLVal(dst, e)
			i++
		}
		return append(dst, '}')
	case []any:
		dst = append(dst, '[')
		for i, e := range n {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendGQLVal(dst, e)
		}
		return append(dst, ']')
	case string:
		return appendJSONString(dst, n)
	case bool:
		if n {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case int64:
		return strconv.AppendInt(dst, n, 10)
	case int:
		return strconv.AppendInt(dst, int64(n), 10)
	case float64:
		return strconv.AppendFloat(dst, n, 'g', -1, 64)
	case JVal:
		return appendJSONVal(dst, n)
	}
	return append(dst, "null"...)
}

// gqlExecute runs one document; returns one result per operation.
func gqlExecute(doc string, variables map[string]JVal) ([]*orderedObj, [][]string, error) {
	ops, err := gqlParse(doc)
	if err != nil {
		return nil, nil, err
	}
	var results []*orderedObj
	var allErrors [][]string
	for _, op := range ops {
		data := newOrderedObj()
		var opErrors []string
		if op.kind == "mutation" {
			execMutationFields(op.selection, data, &opErrors, variables)
		} else {
			root := &gqlNode{typename: "Query"}
			execFields(root, op.selection, data, &opErrors, variables)
		}
		results = append(results, data)
		allErrors = append(allErrors, opErrors)
	}
	return results, allErrors, nil
}
