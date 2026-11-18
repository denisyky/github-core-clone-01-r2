// MCP /mcp: JSON-RPC 2.0 Streamable HTTP, revision 2026-07-28.
// Faithful port of the Node forge's MCP handlers.

package main

import (
	"strconv"
	"strings"
)

var mcpVersions = []string{"2026-07-28", "2025-11-25"}

type mcpToolDef struct {
	name        string
	description string
	properties  [][3]string // name, type, description (in order)
	required    []string
}

// toolsListJSON is built once at startup; two tools/list calls answer the
// same bytes, byte for byte.
var mcpToolsListJSON []byte

var mcpTools = []mcpToolDef{
	{"create_repo", "Create a new repository for the authenticated user.",
		[][3]string{
			{"name", "string", "Repository name"},
			{"description", "string", "Repository description"},
			{"private", "boolean", "Whether the repository is private"},
			{"owner", "string", "Owner login (defaults to the authenticated user)"},
		}, []string{"name"}},
	{"get_repo", "Get a repository by owner and name.",
		[][3]string{
			{"owner", "string", "Owner login"},
			{"repo", "string", "Repository name"},
		}, []string{"owner", "repo"}},
	{"create_issue", "Create an issue in a repository.",
		[][3]string{
			{"owner", "string", "Owner login"},
			{"repo", "string", "Repository name"},
			{"title", "string", "Issue title"},
			{"body", "string", "Issue body"},
		}, []string{"owner", "repo", "title"}},
	{"list_issues", "List issues in a repository.",
		[][3]string{
			{"owner", "string", "Owner login"},
			{"repo", "string", "Repository name"},
			{"state", "string", "Issue state filter: open, closed, or all"},
		}, []string{"owner", "repo"}},
	{"get_issue", "Get a single issue by number.",
		[][3]string{
			{"owner", "string", "Owner login"},
			{"repo", "string", "Repository name"},
			{"issue_number", "integer", "Issue number"},
		}, []string{"owner", "repo", "issue_number"}},
	{"create_pull_request", "Create a pull request between two branches.",
		[][3]string{
			{"owner", "string", "Owner login"},
			{"repo", "string", "Repository name"},
			{"title", "string", "Pull request title"},
			{"body", "string", "Pull request body"},
			{"head", "string", "Head branch name"},
			{"base", "string", "Base branch name"},
		}, []string{"owner", "repo", "title", "head", "base"}},
	{"merge_pull_request", "Merge an open pull request.",
		[][3]string{
			{"owner", "string", "Owner login"},
			{"repo", "string", "Repository name"},
			{"pull_number", "integer", "Pull request number"},
		}, []string{"owner", "repo", "pull_number"}},
	{"list_pull_requests", "List pull requests in a repository.",
		[][3]string{
			{"owner", "string", "Owner login"},
			{"repo", "string", "Repository name"},
			{"state", "string", "Pull request state filter: open, closed, or all"},
		}, []string{"owner", "repo"}},
}

func buildToolsListJSON() []byte {
	var b []byte
	b = append(b, `{"tools":[`...)
	for i, t := range mcpTools {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"name":`...)
		b = appendJSONString(b, t.name)
		b = append(b, `,"description":`...)
		b = appendJSONString(b, t.description)
		b = append(b, `,"inputSchema":{"type":"object","properties":{`...)
		for j, p := range t.properties {
			if j > 0 {
				b = append(b, ',')
			}
			b = appendJSONString(b, p[0])
			b = append(b, `:{"type":`...)
			b = appendJSONString(b, p[1])
			b = append(b, `,"description":`...)
			b = appendJSONString(b, p[2])
			b = append(b, '}')
		}
		b = append(b, `},"required":[`...)
		for j, r := range t.required {
			if j > 0 {
				b = append(b, ',')
			}
			b = appendJSONString(b, r)
		}
		b = append(b, `]}}`...)
	}
	b = append(b, `]}`...)
	return b
}

func mcpResult(id JVal, result []byte) []byte {
	var b []byte
	b = append(b, `{"jsonrpc":"2.0","id":`...)
	b = appendJSONVal(b, id)
	b = append(b, `,"result":`...)
	b = append(b, result...)
	b = append(b, '}')
	return b
}

func mcpError(id JVal, code int, message string) []byte {
	var b []byte
	b = append(b, `{"jsonrpc":"2.0","id":`...)
	b = appendJSONVal(b, id)
	b = append(b, `,"error":{"code":`...)
	b = strconv.AppendInt(b, int64(code), 10)
	b = append(b, `,"message":`...)
	b = appendJSONString(b, message)
	b = append(b, `}}`...)
	return b
}

func mcpToolText(obj []byte) []byte {
	var b []byte
	b = append(b, `{"content":[{"type":"text","text":`...)
	b = appendJSONString(b, string(obj))
	b = append(b, `}],"structuredContent":`...)
	b = append(b, obj...)
	b = append(b, '}')
	return b
}

func mcpToolError(message string) []byte {
	var b []byte
	b = append(b, `{"content":[{"type":"text","text":`...)
	b = appendJSONString(b, `{"error":`+jsonQuote(message)+`}`)
	b = append(b, `}],"isError":true}`...)
	return b
}

// mcpCallTool returns (resultJSON, rpcErrorCode, rpcErrorMessage).
// rpcErrorCode != 0 means a JSON-RPC error response.
func mcpCallTool(name string, args map[string]JVal) ([]byte, int, string) {
	canonical := strings.ToLower(strings.TrimPrefix(name, "github__"))
	var tool *mcpToolDef
	for i := range mcpTools {
		if mcpTools[i].name == canonical {
			tool = &mcpTools[i]
			break
		}
	}
	if tool == nil {
		return nil, -32602, "Unknown tool: " + name
	}
	if args == nil {
		args = map[string]JVal{}
	}
	var missing []string
	for _, k := range tool.required {
		v, ok := args[k]
		if !ok || v == nil {
			missing = append(missing, k)
			continue
		}
		if s, isStr := v.(string); isStr && s == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return mcpToolError("Missing required argument(s): " + strings.Join(missing, ", ")), 0, ""
	}
	owner := jStr(args["owner"])
	if owner == "" {
		owner = viewerLogin
	}
	repoName := jStr(args["repo"])
	if repoName == "" {
		repoName = jStr(args["repository"])
	}
	switch canonical {
	case "create_repo":
		repoOwner := jStr(args["owner"])
		if repoOwner == "" {
			repoOwner = viewerLogin
		}
		db.mu.Lock()
		defer db.mu.Unlock()
		if _, ok := db.repos[repoKey(repoOwner, jStr(args["name"]))]; ok {
			return mcpToolError("Repository " + repoOwner + "/" + jStr(args["name"]) + " already exists"), 0, ""
		}
		r := db.createRepo(repoOwner, jStr(args["name"]), jStr(args["description"]), jBool(args["private"]))
		return mcpToolText(mcpRepoObj(r)), 0, ""
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	r := db.getRepo(owner, repoName)
	if r == nil {
		return mcpToolError("Repository " + owner + "/" + repoName + " not found"), 0, ""
	}
	switch canonical {
	case "get_repo":
		return mcpToolText(mcpRepoObj(r)), 0, ""
	case "create_issue":
		iss := makeIssue(r, jStr(args["title"]), jStr(args["body"]), nil)
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
		b = append(b, `,"url":`...)
		b = appendJSONString(b, "/api/v3/repos/"+r.owner+"/"+r.name+"/issues/"+intStr(iss.number))
		b = append(b, '}')
		return mcpToolText(b), 0, ""
	case "list_issues":
		state := jStr(args["state"])
		if state == "" {
			state = "open"
		}
		var b []byte
		b = append(b, `{"issues":[`...)
		n := 0
		for _, iss := range r.issues {
			if state != "all" && iss.state != state {
				continue
			}
			if n > 0 {
				b = append(b, ',')
			}
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
			b = append(b, '}')
			n++
		}
		b = append(b, `],"total_count":`...)
		b = strconv.AppendInt(b, int64(n), 10)
		b = append(b, '}')
		return mcpToolText(b), 0, ""
	case "get_issue":
		num, _ := jInt(args["issue_number"])
		iss := r.issueByNumber[int(num)]
		if iss == nil {
			return mcpToolError("Issue #" + intStr(int(num)) + " not found"), 0, ""
		}
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
		b = append(b, '}')
		return mcpToolText(b), 0, ""
	case "create_pull_request":
		head := shortBranch(jStr(args["head"]))
		base := shortBranch(jStr(args["base"]))
		if base == "" {
			base = r.defaultBranch
		}
		if branchTip(r, head) == "" {
			return mcpToolError("head branch '" + head + "' does not exist"), 0, ""
		}
		for _, p := range r.pulls {
			if p.head == head && p.base == base && p.state == "open" {
				return mcpToolError("A pull request already exists for this head and base"), 0, ""
			}
		}
		pr := makePull(r, jStr(args["title"]), jStr(args["body"]), head, base)
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
		b = append(b, `,"head":{"ref":`...)
		b = appendJSONString(b, pr.head)
		b = append(b, `,"sha":`...)
		b = appendJSONString(b, branchTip(r, pr.head))
		b = append(b, `},"base":{"ref":`...)
		b = appendJSONString(b, pr.base)
		b = append(b, `,"sha":`...)
		b = appendJSONString(b, branchTip(r, pr.base))
		b = append(b, `},"merged":false}`...)
		return mcpToolText(b), 0, ""
	case "merge_pull_request":
		num, _ := jInt(args["pull_number"])
		pr := r.prByNumber[int(num)]
		if pr == nil {
			return mcpToolError("Pull request #" + intStr(int(num)) + " not found"), 0, ""
		}
		if pr.state != "open" {
			return mcpToolError("Pull request is already closed"), 0, ""
		}
		status, sha, msg := doMerge(r, pr)
		if status != 200 {
			return mcpToolError(msg), 0, ""
		}
		var b []byte
		b = append(b, `{"merged":true,"sha":`...)
		b = appendJSONString(b, sha)
		b = append(b, `,"message":"Pull Request successfully merged"}`...)
		return mcpToolText(b), 0, ""
	case "list_pull_requests":
		state := jStr(args["state"])
		if state == "" {
			state = "open"
		}
		var b []byte
		b = append(b, `{"pull_requests":[`...)
		n := 0
		for _, pr := range r.pulls {
			if state != "all" && pr.state != state {
				continue
			}
			if n > 0 {
				b = append(b, ',')
			}
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
			b = append(b, `,"merged":`...)
			if pr.merged {
				b = append(b, "true"...)
			} else {
				b = append(b, "false"...)
			}
			b = append(b, `,"head":{"ref":`...)
			b = appendJSONString(b, pr.head)
			b = append(b, `},"base":{"ref":`...)
			b = appendJSONString(b, pr.base)
			b = append(b, `}}`...)
			n++
		}
		b = append(b, `],"total_count":`...)
		b = strconv.AppendInt(b, int64(n), 10)
		b = append(b, '}')
		return mcpToolText(b), 0, ""
	}
	return nil, -32602, "Unknown tool: " + name
}

func mcpRepoObj(r *repo) []byte {
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
	b = appendJSONString(b, r.owner)
	b = append(b, `,"description":`...)
	b = appendJSONString(b, r.description)
	b = append(b, `,"private":`...)
	if r.private {
		b = append(b, "true"...)
	} else {
		b = append(b, "false"...)
	}
	b = append(b, `,"default_branch":`...)
	b = appendJSONString(b, r.defaultBranch)
	b = append(b, '}')
	return b
}

// handleMcp serves one POST /mcp request. Returns (status, body).
func handleMcp(rawBody []byte) (int, []byte) {
	msg, err := parseJSON(rawBody)
	if err != nil {
		return 400, mcpError(nil, -32700, "Parse error")
	}
	m, ok := msg.(map[string]JVal)
	if !ok {
		return 400, mcpError(nil, -32700, "Parse error")
	}
	// Notification: method present, no id.
	if _, hasMethod := m["method"]; hasMethod {
		if _, hasID := m["id"]; !hasID {
			return 202, nil
		}
	}
	id, _ := m["id"]
	method := jStr(m["method"])
	params, _ := m["params"].(map[string]JVal)
	if params == nil {
		params = map[string]JVal{}
	}
	switch method {
	case "server/discover":
		var result []byte
		result = append(result, `{"serverInfo":{"name":"github-forge","version":"1.0.0"},"capabilities":{"tools":{}},"supportedVersions":["2026-07-28","2025-11-25"],"protocolVersion":"2026-07-28","_meta":{"io.modelcontextprotocol/serverInfo":{"name":"github-forge","version":"1.0.0"}}}`...)
		return 200, mcpResult(id, result)
	case "initialize":
		requested := params["protocolVersion"]
		var negotiated string
		var candidates []string
		if arr, ok := requested.([]JVal); ok {
			for _, v := range arr {
				candidates = append(candidates, jStr(v))
			}
		} else if s, ok := requested.(string); ok {
			candidates = []string{s}
		}
		for _, c := range candidates {
			for _, v := range mcpVersions {
				if c == v {
					negotiated = c
					break
				}
			}
			if negotiated != "" {
				break
			}
		}
		if negotiated == "" {
			negotiated = "2026-07-28"
		}
		var result []byte
		result = append(result, `{"protocolVersion":`...)
		result = appendJSONString(result, negotiated)
		result = append(result, `,"capabilities":{"tools":{}},"serverInfo":{"name":"github-forge","version":"1.0.0"}}`...)
		return 200, mcpResult(id, result)
	case "ping":
		return 200, mcpResult(id, []byte("{}"))
	case "tools/list":
		return 200, mcpResult(id, mcpToolsListJSON)
	case "tools/call":
		args, _ := m["params"].(map[string]JVal)
		_ = args
		pname := jStr(params["name"])
		pargs, _ := params["arguments"].(map[string]JVal)
		res, code, msg := mcpCallTool(pname, pargs)
		if code != 0 {
			return 200, mcpError(id, code, msg)
		}
		return 200, mcpResult(id, res)
	case "resources/list":
		return 200, mcpResult(id, []byte(`{"resources":[]}`))
	case "prompts/list":
		return 200, mcpResult(id, []byte(`{"prompts":[]}`))
	default:
		return 200, mcpError(id, -32601, "Method not found: "+method)
	}
}
