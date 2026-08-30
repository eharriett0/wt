package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strings"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/todos"
)

// cmdMCP runs a stdio MCP (Model Context Protocol) server exposing wt's
// READ-ONLY surface as tools — wt_status / wt_check / wt_todos / wt_where — so a
// chat-style agent (Claude Desktop, Cursor, any MCP client) can ask "who else is
// touching this file?" without shelling out. It speaks newline-delimited
// JSON-RPC 2.0 over stdin/stdout (the MCP stdio transport) and single-sources
// every answer on the SAME collide.Scan / buildCheckReport / gradeStatusOverlaps
// pipeline the CLI uses, so the data can never drift from `wt status --json` etc.
// v1 is read-only by design: no tool mutates state.
func cmdMCP(_ []string) int {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // MCP messages are one-line; allow large args
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	writeResp := func(r rpcResponse) {
		b, err := json.Marshal(r)
		if err != nil {
			return
		}
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			writeResp(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
				Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		// A JSON-RPC notification has no id — no response (e.g.
		// notifications/initialized). A request (even id:null) gets a reply.
		if len(req.ID) == 0 {
			continue
		}
		writeResp(dispatchRPC(req))
	}
	return 0
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func dispatchRPC(req rpcRequest) rpcResponse {
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = mcpInitialize(req.Params)
	case "tools/list":
		resp.Result = map[string]any{"tools": mcpToolDescriptors()}
	case "tools/call":
		res, rerr := mcpToolsCall(req.Params)
		if rerr != nil {
			resp.Error = rerr
		} else {
			resp.Result = res
		}
	case "ping":
		resp.Result = map[string]any{}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

func mcpInitialize(params json.RawMessage) map[string]any {
	ver := "2025-06-18"
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 && json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		ver = p.ProtocolVersion // echo the client's version when it names one
	}
	return map[string]any{
		"protocolVersion": ver,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "wt", "version": version()},
		"instructions": "wt exposes multi-window git-coordination state (read-only). " +
			"Call wt_status to see every active window + graded file overlaps, " +
			"wt_check before editing paths to see if another live window is in them, " +
			"wt_todos for what each window is working on, wt_where to resolve an " +
			"issue/branch to its worktree path.",
	}
}

func mcpToolDescriptors() []map[string]any {
	obj := func(props map[string]any, required ...string) map[string]any {
		s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	return []map[string]any{
		{
			"name": "wt_status",
			"description": "Every active window in this repo (worktree, branch, claimed issue, files touched) " +
				"plus graded cross-window overlaps (HIGH = overlapping hunks). Read-only.",
			"inputSchema": obj(map[string]any{
				"blocking": map[string]any{"type": "boolean", "description": "return only HIGH-risk overlaps"},
			}),
		},
		{
			"name": "wt_check",
			"description": "Before editing: is any other live window touching these paths? Returns per-path " +
				"grading (HIGH overlapping hunks vs disjoint/advisory). Read-only.",
			"inputSchema": obj(map[string]any{
				"paths":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "repo-relative paths (or bare basenames) to check"},
				"include_stale": map[string]any{"type": "boolean", "description": "include merged/dormant windows"},
			}, "paths"),
		},
		{
			"name":        "wt_todos",
			"description": "What every window is working on — each window's recorded TODO list (active item + pending). Read-only.",
			"inputSchema": obj(map[string]any{}),
		},
		{
			"name":        "wt_where",
			"description": "Resolve an issue number or branch name to that window's worktree path. Read-only.",
			"inputSchema": obj(map[string]any{
				"target": map[string]any{"type": "string", "description": "issue number (e.g. 42 or #42) or branch name"},
			}, "target"),
		},
	}
}

func mcpToolsCall(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}
	text, isErr := runMCPTool(p.Name, p.Arguments)
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}, nil
}

// runMCPTool executes one read-only tool and returns (text, isError). Tool-level
// failures (not in a repo, bad args, scan error) come back as isError results —
// NOT JSON-RPC protocol errors — so the client surfaces them to the model.
func runMCPTool(name string, args json.RawMessage) (string, bool) {
	c, err := config.Load()
	if err != nil {
		return "not in a git repo (or wt config unavailable): " + err.Error(), true
	}
	switch name {
	case "wt_status":
		var a struct {
			Blocking bool `json:"blocking"`
		}
		_ = json.Unmarshal(args, &a)
		return mcpStatus(c, a.Blocking)
	case "wt_check":
		var a struct {
			Paths        []string `json:"paths"`
			IncludeStale bool     `json:"include_stale"`
		}
		if err := json.Unmarshal(args, &a); err != nil || len(a.Paths) == 0 {
			return "wt_check requires a non-empty `paths` array", true
		}
		return mcpCheck(c, a.Paths, a.IncludeStale)
	case "wt_todos":
		return mcpTodos(c)
	case "wt_where":
		var a struct {
			Target string `json:"target"`
		}
		if err := json.Unmarshal(args, &a); err != nil || strings.TrimSpace(a.Target) == "" {
			return "wt_where requires a `target` (issue number or branch)", true
		}
		return mcpWhere(c, a.Target)
	default:
		return "unknown tool: " + name, true
	}
}

func jsonText(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}

func mcpStatus(c *config.Config, blocking bool) (string, bool) {
	ws, err := collide.Scan(c)
	if err != nil {
		return "scan failed: " + err.Error(), true
	}
	ov := collide.Overlaps(ws)
	live := collide.ClassifyWindows(ws, c.Base, collide.OverlapWindowSet(ov), c.MaxAge)
	active, benign := collide.PartitionOverlaps(ov, live)
	graded := gradeStatusOverlaps(c, ws, active)
	p := buildStatusPayload(ws, graded, len(benign))
	if blocking {
		kept := p.Overlaps[:0]
		for _, o := range p.Overlaps {
			if o.Category == CatBlocking {
				kept = append(kept, o)
			}
		}
		p.Overlaps = kept
	}
	return jsonText(p), false
}

func mcpCheck(c *config.Config, paths []string, includeStale bool) (string, bool) {
	ws, err := collide.Scan(c)
	if err != nil {
		return "scan failed: " + err.Error(), true
	}
	root, _ := gitx.RepoRoot()
	entries := buildCheckReport(c, ws, root, paths, includeStale)
	return jsonText(buildCheckPayload(entries, includeStale)), false
}

func mcpWhere(c *config.Config, target string) (string, bool) {
	target = strings.TrimSpace(target)
	norm := strings.TrimPrefix(target, "#")
	ws, err := collide.Scan(c)
	if err != nil {
		return "scan failed: " + err.Error(), true
	}
	for _, w := range ws {
		if (w.Issue != "" && w.Issue == norm) || w.Branch == target || w.Branch == norm {
			return jsonText(map[string]any{"target": target, "worktree": w.Worktree,
				"branch": w.Branch, "issue": w.Issue}), false
		}
	}
	return "no worktree found for " + target + " — call wt_status to see active windows", true
}

// mcpTodoWindow is one window's TODO summary in the wt_todos result.
type mcpTodoWindow struct {
	Label      string   `json:"label"`
	Branch     string   `json:"branch"`
	Title      string   `json:"title,omitempty"`
	Active     string   `json:"active,omitempty"`
	Pending    []string `json:"pending,omitempty"`
	Done       int      `json:"done"`
	InProgress int      `json:"in_progress"`
	PendingN   int      `json:"pending_count"`
	Updated    string   `json:"updated,omitempty"`
	HasTodos   bool     `json:"has_todos"`
}

func mcpTodos(c *config.Config) (string, bool) {
	ws, err := collide.Scan(c)
	if err != nil {
		return "scan failed: " + err.Error(), true
	}
	out := make([]mcpTodoWindow, 0, len(ws))
	for _, w := range ws {
		tw := mcpTodoWindow{Label: w.Label(), Branch: w.Branch, Title: w.Title}
		rec, _ := todos.ForWorktree(w.Worktree)
		if rec != nil && len(rec.Todos) > 0 {
			tw.HasTodos = true
			tw.Updated = rec.Updated
			pending, inProgress, done := rec.Counts()
			tw.PendingN, tw.InProgress, tw.Done = pending, inProgress, done
			if a, ok := rec.Active(); ok {
				tw.Active = a.ActiveForm
			}
			for _, t := range rec.Todos {
				if t.Status == "pending" {
					tw.Pending = append(tw.Pending, t.Content)
				}
			}
		}
		out = append(out, tw)
	}
	return jsonText(map[string]any{"windows": out}), false
}
