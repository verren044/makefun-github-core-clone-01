// mcp.go — MCP (Model Context Protocol) over Streamable HTTP at /mcp.
// Faithful port of v1.
package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

var mcpVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

func mcpTool(name, desc string, props map[string]any, required []string) map[string]any {
	return map[string]any{
		"name": name, "description": desc,
		"inputSchema": map[string]any{
			"type": "object", "properties": props, "required": required,
		},
	}
}

func mcpProp(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

var mcpTools = []any{
	mcpTool("create_repo", "Create a new repository owned by the authenticated user.", map[string]any{
		"name": mcpProp("Repository name"), "description": mcpProp("Repository description"),
		"private": map[string]any{"type": "boolean", "description": "Whether the repository is private"},
		"full_name": mcpProp("owner/repo"),
	}, []string{"name"}),
	mcpTool("get_repo", "Get a repository by owner and name.", map[string]any{
		"owner": mcpProp("Repository owner"), "repo": mcpProp("Repository name"),
		"full_name": mcpProp("owner/repo"),
	}, []string{"owner", "repo"}),
	mcpTool("create_issue", "Create an issue in a repository.", map[string]any{
		"owner": mcpProp("Repository owner"), "repo": mcpProp("Repository name"),
		"full_name": mcpProp("owner/repo"), "title": mcpProp("Issue title"), "body": mcpProp("Issue body"),
	}, []string{"owner", "repo", "title"}),
	mcpTool("list_issues", "List issues in a repository, optionally filtered by state.", map[string]any{
		"owner": mcpProp("Repository owner"), "repo": mcpProp("Repository name"),
		"full_name": mcpProp("owner/repo"), "state": mcpProp("Issue state: open, closed or all"),
	}, []string{"owner", "repo"}),
	mcpTool("create_pull_request", "Create a pull request from a head branch onto a base branch.", map[string]any{
		"owner": mcpProp("Repository owner"), "repo": mcpProp("Repository name"),
		"full_name": mcpProp("owner/repo"), "title": mcpProp("Pull request title"),
		"body": mcpProp("Pull request body"), "head": mcpProp("Head branch name"), "base": mcpProp("Base branch name"),
	}, []string{"owner", "repo", "title", "head", "base"}),
	mcpTool("merge_pull_request", "Merge a pull request with a squash commit.", map[string]any{
		"owner": mcpProp("Repository owner"), "repo": mcpProp("Repository name"),
		"full_name": mcpProp("owner/repo"),
		"pull_number": map[string]any{"type": "integer", "description": "Pull request number"},
	}, []string{"owner", "repo", "pull_number"}),
}

// sortedJSON re-serializes b with sorted keys (Python's
// json.dumps(payload, sort_keys=True), as mcp_text_result uses).
func sortedJSON(b []byte) []byte {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return b
	}
	out, err := json.Marshal(v)
	if err != nil {
		return b
	}
	return out
}

func mcpTextResult(payloadJSON []byte) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": string(sortedJSON(payloadJSON))}},
		"isError": false,
	}
}

func mcpSplitFullName(args map[string]any) (string, string) {
	owner, _ := args["owner"].(string)
	repo, _ := args["repo"].(string)
	full, _ := args["full_name"].(string)
	if full == "" {
		full, _ = args["fullName"].(string)
	}
	if (owner == "" || repo == "") && strings.Contains(full, "/") {
		parts := strings.SplitN(full, "/", 2)
		if owner == "" {
			owner = parts[0]
		}
		if repo == "" {
			repo = parts[1]
		}
	}
	return owner, repo
}

type mcpError struct {
	code int
	msg  string
}

func (e *mcpError) Error() string { return e.msg }

// mcpCall dispatches one tools/call. Returns (result, isError, error).
func mcpCall(name string, args map[string]any) (map[string]any, error) {
	base := strings.ToLower(name)
	if i := strings.Index(base, "__"); i >= 0 {
		base = base[i+2:]
	}
	known := false
	for _, t := range []string{"create_repo", "get_repo", "create_issue", "list_issues", "create_pull_request", "merge_pull_request"} {
		if base == t {
			known = true
		}
	}
	if !known {
		return nil, &mcpError{-32601, "Unknown tool: " + name}
	}
	if args == nil {
		args = map[string]any{}
	}
	if base == "create_repo" {
		if strVal(args, "name", "") == "" {
			return nil, &mcpError{-32602, "Missing required argument: name"}
		}
		repo, err := createRepo(strVal(args, "name", ""), args["description"], args["private"])
		if err != nil {
			return nil, err
		}
		return mcpTextResult(repoRestJSON(repo)), nil
	}
	owner, repoName := mcpSplitFullName(args)
	if owner == "" || repoName == "" {
		return nil, &mcpError{-32602, "Missing required argument: owner/repo"}
	}
	if owner != viewerLogin {
		return nil, &mcpError{-32602, "Repository not found"}
	}
	repo, ok := store.repos[[2]string{owner, repoName}]
	if base == "get_repo" {
		if !ok {
			return nil, &mcpError{-32602, "Repository not found"}
		}
		return mcpTextResult(repoRestJSON(repo)), nil
	}
	if !ok {
		return nil, &mcpError{-32602, "Repository not found"}
	}
	switch base {
	case "create_issue":
		if strVal(args, "title", "") == "" {
			return nil, &mcpError{-32602, "Missing required argument: title"}
		}
		num := repo.NextIssue
		repo.NextIssue++
		issue := newIssue(repo, num, strVal(args, "title", ""), strVal(args, "body", ""), []string{})
		repo.Issues[num] = issue
		repo.IssueOrder = append(repo.IssueOrder, num)
		return mcpTextResult(issueRestJSON(issue)), nil
	case "list_issues":
		state := strings.ToLower(strVal(args, "state", "open"))
		var items []*Issue
		for _, n := range repo.IssueOrder {
			items = append(items, repo.Issues[n])
		}
		if state == "open" || state == "closed" {
			filtered := items[:0]
			for _, i := range items {
				if i.State == state {
					filtered = append(filtered, i)
				}
			}
			items = filtered
		}
		var buf []byte
		buf = append(buf, '[')
		for i, it := range items {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = append(buf, issueRestJSON(it)...)
		}
		buf = append(buf, ']')
		return mcpTextResult(buf), nil
	case "create_pull_request":
		for _, req := range []string{"title", "head", "base"} {
			if strVal(args, req, "") == "" {
				return nil, &mcpError{-32602, "Missing required argument: " + req}
			}
		}
		headSHA := repo.branchTip(strVal(args, "head", ""))
		if headSHA == "" {
			return nil, &mcpError{-32602, "Head branch does not exist"}
		}
		num := repo.NextPull
		repo.NextPull++
		pr := newPull(repo, num, strVal(args, "title", ""), strVal(args, "body", ""),
			strVal(args, "head", ""), strVal(args, "base", ""), headSHA, repo.branchTip(strVal(args, "base", "")))
		repo.Pulls[num] = pr
		repo.PullOrder = append(repo.PullOrder, num)
		return mcpTextResult(pullRestJSON(pr)), nil
	case "merge_pull_request":
		if args["pull_number"] == nil {
			return nil, &mcpError{-32602, "Missing required argument: pull_number"}
		}
		// Python int(): float truncates, digit strings parse, anything
		// else raises -> -32602 "pull_number must be an integer".
		num := 0
		switch v := args["pull_number"].(type) {
		case float64:
			num = int(v)
		case string:
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, &mcpError{-32602, "pull_number must be an integer"}
			}
			num = n
		default:
			return nil, &mcpError{-32602, "pull_number must be an integer"}
		}
		pr, ok := repo.Pulls[num]
		if !ok {
			return nil, &mcpError{-32602, "Pull request not found"}
		}
		payload, err := mergePull(repo, pr, map[string]any{"merge_method": "squash"})
		if err != nil {
			return nil, err
		}
		return mcpTextResult(payload.body), nil
	}
	return nil, &mcpError{-32601, "Unknown tool: " + name}
}

// handleMCP handles a parsed JSON-RPC body. Returns (status, payload,
// isNotification). A notification returns (202, nil, true).
func handleMCP(doc any) (int, any, bool) {
	m, ok := doc.(map[string]any)
	if !ok {
		return 400, map[string]any{"jsonrpc": "2.0", "id": nil,
			"error": map[string]any{"code": -32700, "message": "Parse error"}}, false
	}
	reqID := m["id"]
	method, _ := m["method"].(string)
	params, _ := m["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	_, hasID := m["id"]
	isNotification := !hasID
	if method == "notifications/initialized" || strings.HasPrefix(method, "notifications/") {
		return 202, nil, true
	}
	switch method {
	case "initialize":
		proto := strVal(params, "protocolVersion", "2025-11-25")
		result := map[string]any{
			"protocolVersion": proto,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "forge", "version": version},
		}
		return 200, map[string]any{"jsonrpc": "2.0", "id": reqID, "result": result}, false
	case "server/discover":
		result := map[string]any{
			"supportedVersions": mcpVersions,
			"serverInfo":        map[string]any{"name": "forge", "version": version},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"_meta": map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "forge", "version": version}},
		}
		return 200, map[string]any{"jsonrpc": "2.0", "id": reqID, "result": result}, false
	case "ping":
		return 200, map[string]any{"jsonrpc": "2.0", "id": reqID, "result": map[string]any{}}, false
	case "tools/list":
		return 200, map[string]any{"jsonrpc": "2.0", "id": reqID, "result": map[string]any{"tools": mcpTools}}, false
	case "tools/call":
		tname, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		result, err := mcpCall(tname, args)
		if err != nil {
			if me, ok := err.(*mcpError); ok {
				return 200, map[string]any{"jsonrpc": "2.0", "id": reqID,
					"error": map[string]any{"code": me.code, "message": me.msg}}, false
			}
			if ae, ok := err.(*apiError); ok {
				return 200, map[string]any{"jsonrpc": "2.0", "id": reqID,
					"result": map[string]any{
						"content": []any{map[string]any{"type": "text", "text": ae.msg}},
						"isError": true}}, false
			}
			return 200, map[string]any{"jsonrpc": "2.0", "id": reqID,
				"error": map[string]any{"code": -32603, "message": err.Error()}}, false
		}
		return 200, map[string]any{"jsonrpc": "2.0", "id": reqID, "result": result}, false
	}
	if isNotification {
		return 202, nil, true
	}
	return 200, map[string]any{"jsonrpc": "2.0", "id": reqID,
		"error": map[string]any{"code": -32601, "message": "Method not found: " + method}}, false
}
