// MCP at /mcp: JSON-RPC 2.0 over Streamable HTTP, revision 2026-07-28.
// Ported from the Node forge; tools/list is byte-identical across calls.
package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

var mcpVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type mcpTool struct {
	name        string
	description string
	schema      map[string]any
}

var mcpTools = []mcpTool{
	{"create_repo", "Create a new repository owned by the authenticated user.", map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string", "description": "Repository name"}, "description": map[string]any{"type": "string"}, "private": map[string]any{"type": "boolean"}}, "required": []any{"name"}}},
	{"get_repo", "Get a repository by owner and name.", map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string"}, "repo": map[string]any{"type": "string"}}, "required": []any{"owner", "repo"}}},
	{"create_issue", "Create an issue in a repository.", map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string"}, "repo": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"}}, "required": []any{"owner", "repo", "title"}}},
	{"list_issues", "List issues in a repository.", map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string"}, "repo": map[string]any{"type": "string"}, "state": map[string]any{"type": "string"}}, "required": []any{"owner", "repo"}}},
	{"create_pull_request", "Create a pull request in a repository.", map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string"}, "repo": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"}, "head": map[string]any{"type": "string"}, "base": map[string]any{"type": "string"}}, "required": []any{"owner", "repo", "title", "head", "base"}}},
	{"merge_pull_request", "Merge a pull request in a repository.", map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string"}, "repo": map[string]any{"type": "string"}, "pull_number": map[string]any{"type": "integer"}}, "required": []any{"owner", "repo", "pull_number"}}},
}

func mcpToolsJSON() []any {
	out := make([]any, 0, len(mcpTools))
	for _, t := range mcpTools {
		out = append(out, map[string]any{"name": t.name, "description": t.description, "inputSchema": t.schema})
	}
	return out
}

func mcpOwnerRepo(args map[string]any) (owner, repo string) {
	owner = asStr(args, "owner")
	repo = asStr(args, "repo")
	if owner == "" || repo == "" {
		if full, ok := args["full_name"].(string); ok {
			if parts := strings.Split(full, "/"); len(parts) == 2 {
				if owner == "" {
					owner = parts[0]
				}
				if repo == "" {
					repo = parts[1]
				}
			}
		}
	}
	if owner == "" {
		owner = viewer
	}
	return owner, repo
}

func mcpText(obj any) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(marshalJSON(obj))}}, "structuredContent": obj}
}

func mcpErrorResult(message string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(marshalJSON(map[string]any{"error": message}))}}, "isError": true}
}

func handleMCP(req *request) *response {
	var msg map[string]any
	if len(req.body) > 0 {
		dec := json.NewDecoder(bytes.NewReader(req.body))
		dec.UseNumber()
		if err := dec.Decode(&msg); err != nil {
			return jsonResp(400, mustJSON(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "Parse error"}}))
		}
	}
	if msg == nil {
		msg = map[string]any{}
	}
	if !authOk(req) {
		return jsonResp(401, mustJSON(map[string]any{"jsonrpc": "2.0", "id": idOrNull(msg), "error": map[string]any{"code": -32000, "message": "Unauthorized"}}))
	}
	id := idOrNull(msg)
	method := asStr(msg, "method")
	params, _ := msg["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	rpcResult := func(result any) *response {
		return jsonResp(200, mustJSON(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}))
	}
	rpcError := func(code int, message string, data any) *response {
		errObj := map[string]any{"code": code, "message": message}
		if data != nil {
			errObj["data"] = data
		}
		return jsonResp(200, mustJSON(map[string]any{"jsonrpc": "2.0", "id": id, "error": errObj}))
	}
	// Notification (no id): accept silently.
	if _, hasID := msg["id"]; !hasID || msg["id"] == nil {
		if method == "notifications/initialized" || strings.HasPrefix(method, "notifications/") {
			return &response{status: 202, body: []byte{}}
		}
	}
	switch method {
	case "server/discover":
		return rpcResult(map[string]any{
			"protocolVersion":   "2026-07-28",
			"supportedVersions": mcpVersions,
			"serverInfo":        map[string]any{"name": "github-forge", "version": "1.0.0"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "github-forge", "version": "1.0.0"}},
		})
	case "initialize":
		requested := asStr(params, "protocolVersion")
		if requested == "" {
			requested = "2026-07-28"
		}
		supported := false
		for _, v := range mcpVersions {
			if v == requested {
				supported = true
			}
		}
		if !supported {
			return rpcError(-32022, "Unsupported protocol version", map[string]any{"supported": mcpVersions})
		}
		return rpcResult(map[string]any{
			"protocolVersion": requested,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "github-forge", "version": "1.0.0"},
		})
	case "ping":
		return rpcResult(map[string]any{})
	case "tools/list":
		return rpcResult(map[string]any{"tools": mcpToolsJSON()})
	case "tools/call":
		rawName := asStr(params, "name")
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		norm := strings.ToLower(rawName)
		base := norm
		if strings.Contains(norm, "__") {
			parts := strings.Split(norm, "__")
			base = parts[len(parts)-1]
		}
		var tool *mcpTool
		for i := range mcpTools {
			if mcpTools[i].name == base {
				tool = &mcpTools[i]
				break
			}
		}
		if tool == nil {
			return rpcError(-32601, "Unknown tool: "+rawName, nil)
		}
		for _, reqArg := range tool.schema["required"].([]any) {
			name := reqArg.(string)
			_, present := args[name]
			if !present && name == "pull_number" {
				_, present = args["number"]
				if !present {
					_, present = args["pr_number"]
				}
			}
			if !present {
				return rpcResult(mcpErrorResult("Missing required argument: " + name))
			}
		}
		var result any
		var errResult map[string]any
		switch tool.name {
		case "create_repo":
			name := asStr(args, "name")
			repo := getRepo(viewer, name)
			if repo == nil {
				repo = createRepo(name, asBool(args, "private"), asStr(args, "description"))
			}
			result = repoJSON(repo)
		case "get_repo":
			owner, rn := mcpOwnerRepo(args)
			var repo *Repo
			if owner == viewer {
				repo = getRepo(owner, rn)
			}
			if repo == nil {
				return rpcResult(mcpErrorResult("Repository not found: " + owner + "/" + rn))
			}
			result = repoJSON(repo)
		case "create_issue":
			owner, rn := mcpOwnerRepo(args)
			var repo *Repo
			if owner == viewer {
				repo = getRepo(owner, rn)
			}
			if repo == nil {
				return rpcResult(mcpErrorResult("Repository not found: " + owner + "/" + rn))
			}
			num := repo.nextNumber
			repo.nextNumber++
			id := nextGlobalID
			nextGlobalID++
			seq := nextGlobalID
			nextGlobalID++
			issue := &Issue{id: id, number: num, title: asStr(args, "title"), body: asStr(args, "body"), state: "open", labels: []Label{}, seq: seq, comments: []Comment{}}
			repo.issues[num] = issue
			result = issueJSON(repo, issue)
		case "list_issues":
			owner, rn := mcpOwnerRepo(args)
			var repo *Repo
			if owner == viewer {
				repo = getRepo(owner, rn)
			}
			if repo == nil {
				return rpcResult(mcpErrorResult("Repository not found: " + owner + "/" + rn))
			}
			var list []*Issue
			for _, i := range repo.issues {
				list = append(list, i)
			}
			if state, ok := args["state"].(string); ok && state != "all" {
				var filtered []*Issue
				for _, i := range list {
					if i.state == state {
						filtered = append(filtered, i)
					}
				}
				list = filtered
			}
			items := make([]any, 0, len(list))
			for _, i := range list {
				items = append(items, issueJSON(repo, i))
			}
			result = map[string]any{"issues": items, "count": len(list)}
		case "create_pull_request":
			owner, rn := mcpOwnerRepo(args)
			var repo *Repo
			if owner == viewer {
				repo = getRepo(owner, rn)
			}
			if repo == nil {
				return rpcResult(mcpErrorResult("Repository not found: " + owner + "/" + rn))
			}
			head := asStr(args, "head")
			base := asStr(args, "base")
			num := repo.nextNumber
			repo.nextNumber++
			id := nextGlobalID
			nextGlobalID++
			seq := nextGlobalID
			nextGlobalID++
			pr := &PR{id: id, number: num, title: asStr(args, "title"), body: asStr(args, "body"), state: "open", merged: false, headRef: head, baseRef: base, headSha: branchTip(repo, head), baseSha: branchTip(repo, base), seq: seq, mergeSha: "", mergedAt: ""}
			repo.prs[num] = pr
			result = prJSON(repo, pr)
		case "merge_pull_request":
			owner, rn := mcpOwnerRepo(args)
			var repo *Repo
			if owner == viewer {
				repo = getRepo(owner, rn)
			}
			if repo == nil {
				return rpcResult(mcpErrorResult("Repository not found: " + owner + "/" + rn))
			}
			num := toIntOrZero(args["pull_number"])
			if _, ok := args["pull_number"]; !ok {
				num = toIntOrZero(args["number"])
				if _, ok := args["number"]; !ok {
					num = toIntOrZero(args["pr_number"])
				}
			}
			pr := repo.prs[num]
			if pr == nil {
				return rpcResult(mcpErrorResult("Pull request not found: " + itoa(num)))
			}
			if !pr.merged {
				if _, conflict := doMerge(repo, pr); conflict != "" {
					return rpcResult(mcpErrorResult(conflict))
				}
			}
			result = map[string]any{"sha": pr.mergeSha, "merged": true, "message": "Pull Request successfully merged", "number": pr.number, "pull_number": pr.number}
		}
		if errResult != nil {
			return rpcResult(errResult)
		}
		return rpcResult(mcpText(result))
	default:
		return rpcError(-32601, "Method not found: "+method, nil)
	}
}

func idOrNull(msg map[string]any) any {
	if v, ok := msg["id"]; ok {
		return v
	}
	return nil
}
