package main

import (
	"encoding/json"
	"slices"
	"strings"
)

var mcpVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}
var mcpInfo = mapJSON{"name": "makefun-git-forge", "version": "1.0.0"}
var mcpCaps = mapJSON{"tools": mapJSON{"listChanged": false}}

type mcpTool struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Input       mapJSON `json:"inputSchema"`
	required    []string
	props       map[string]string
}

func makeTool(name, description, required string, props map[string]string) mcpTool {
	properties := mapJSON{}
	for name, t := range props {
		schema := mapJSON{"type": t}
		if t == "integer" {
			schema["minimum"] = 1
		}
		properties[name] = schema
	}
	r := strings.Fields(required)
	return mcpTool{name, description, mapJSON{"type": "object", "properties": properties, "required": r}, r, props}
}

var mcpTools = []mcpTool{
	makeTool("create_repo", "Create a repository owned by the authenticated user.", "name", map[string]string{"name": "string", "description": "string", "private": "boolean", "auto_init": "boolean"}),
	makeTool("get_repo", "Read repository metadata.", "owner repo", map[string]string{"owner": "string", "repo": "string"}),
	makeTool("create_issue", "Open an issue in a repository.", "owner repo title", map[string]string{"owner": "string", "repo": "string", "title": "string", "body": "string"}),
	makeTool("list_issues", "List issues and filter by state.", "owner repo", map[string]string{"owner": "string", "repo": "string", "state": "string", "page": "integer", "per_page": "integer"}),
	makeTool("create_pull_request", "Open a pull request from an existing branch.", "owner repo head base title", map[string]string{"owner": "string", "repo": "string", "head": "string", "base": "string", "title": "string", "body": "string"}),
	makeTool("merge_pull_request", "Merge or squash a pull request into its base.", "owner repo pull_number", map[string]string{"owner": "string", "repo": "string", "pull_number": "integer", "merge_method": "string", "commit_title": "string", "commit_message": "string"}),
}

func rpcError(id any, code int, message string) mapJSON {
	return mapJSON{"jsonrpc": "2.0", "id": id, "error": mapJSON{"code": code, "message": message}}
}
func rpcResult(id, data any) mapJSON { return mapJSON{"jsonrpc": "2.0", "id": id, "result": data} }
func mcpContent(data string, isError bool) mapJSON {
	return mapJSON{"content": []mapJSON{{"type": "text", "text": data}}, "isError": isError}
}
func (s *store) callMcp(id any, name string, args mapJSON, origin string) (result mapJSON) {
	index := slices.IndexFunc(mcpTools, func(t mcpTool) bool { return t.Name == name })
	if index < 0 {
		return rpcError(id, -32602, "Unknown tool")
	}
	spec := mcpTools[index]
	for _, k := range spec.required {
		if _, ok := args[k]; !ok {
			return rpcError(id, -32602, "Missing tool argument: "+k)
		}
	}
	for key, v := range args {
		valid := true
		switch spec.props[key] {
		case "string":
			_, valid = v.(string)
		case "boolean":
			_, valid = v.(bool)
		case "integer":
			valid = intVal(v) > 0
		}
		if !valid {
			return rpcError(id, -32602, "Invalid tool argument: "+key)
		}
	}
	defer func() {
		if fail := recover(); fail != nil {
			if problem, ok := fail.(apiError); ok {
				result = rpcResult(id, mcpContent(problem.message, true))
				return
			}
			panic(fail)
		}
	}()
	respond := func(data any) mapJSON { return rpcResult(id, mcpContent(string(encoded(data)), false)) }
	if name == "create_repo" {
		return respond(s.createRepo(args).view(origin))
	}
	r := s.find(stringVal(args["owner"]), stringVal(args["repo"]))
	if r == nil {
		reject(404, "Not Found")
	}
	switch name {
	case "get_repo":
		return respond(r.view(origin))
	case "create_issue":
		return respond(s.createIssue(r, args))
	case "list_issues":
		rows := r.issueList(strings.ToLower(optional(args["state"], "open")), false)
		size, page := intVal(args["per_page"]), intVal(args["page"])
		if size == 0 {
			size = 30
		}
		if page == 0 {
			page = 1
		}
		start, end := pageBounds(len(rows), min(size, 100), page)
		return respond(rows[start:end])
	case "create_pull_request":
		return respond(s.createPull(r, args).view())
	case "merge_pull_request":
		p := r.pulls[intVal(args["pull_number"])]
		if p == nil {
			reject(404, "Not Found")
		}
		return respond(s.mergePull(p, args))
	}
	return rpcError(id, -32601, "Method not found")
}
func (s *store) mcp(method, origin string, body []byte) httpReply {
	if method == "DELETE" {
		return jsonReply(204, nil)
	}
	if method != "POST" {
		r := jsonReply(405, mapJSON{"message": "Method Not Allowed"})
		r.allow = "POST, DELETE"
		return r
	}
	var input any
	if json.Unmarshal(body, &input) != nil {
		return jsonReply(400, rpcError(nil, -32700, "Parse error"))
	}
	rpc := objectVal(input)
	rpcMethod := stringVal(rpc["method"])
	if rpc["jsonrpc"] != "2.0" || rpcMethod == "" {
		return jsonReply(200, rpcError(nil, -32600, "Invalid Request"))
	}
	id, found := rpc["id"]
	switch id.(type) {
	case nil, float64, string:
	default:
		return jsonReply(200, rpcError(nil, -32600, "Invalid Request"))
	}
	if !found {
		return jsonReply(202, nil)
	}
	params := objectVal(rpc["params"])
	result := func(v any) httpReply { return jsonReply(200, rpcResult(id, v)) }
	switch rpcMethod {
	case "server/discover":
		return result(mapJSON{"supportedVersions": mcpVersions, "serverInfo": mcpInfo, "capabilities": mcpCaps, "_meta": mapJSON{"io.modelcontextprotocol/serverInfo": mcpInfo}})
	case "initialize":
		v := stringVal(params["protocolVersion"])
		if !slices.Contains(mcpVersions, v) {
			v = mcpVersions[1]
		}
		return result(mapJSON{"protocolVersion": v, "serverInfo": mcpInfo, "capabilities": mcpCaps})
	case "ping":
		return result(mapJSON{})
	case "tools/list":
		return result(mapJSON{"tools": mcpTools})
	case "tools/call":
		return jsonReply(200, s.callMcp(id, stringVal(params["name"]), objectVal(params["arguments"]), origin))
	}
	return jsonReply(200, rpcError(id, -32601, "Method not found"))
}
