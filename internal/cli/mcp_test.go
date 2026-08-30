package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMCPInitialize_EchoesClientVersion(t *testing.T) {
	r := mcpInitialize(json.RawMessage(`{"protocolVersion":"2024-11-05"}`))
	if r["protocolVersion"] != "2024-11-05" {
		t.Errorf("should echo client version, got %v", r["protocolVersion"])
	}
	si, _ := r["serverInfo"].(map[string]any)
	if si["name"] != "wt" {
		t.Errorf("serverInfo.name=%v want wt", si["name"])
	}
	if _, ok := r["capabilities"].(map[string]any)["tools"]; !ok {
		t.Errorf("must advertise tools capability: %v", r["capabilities"])
	}
}

func TestMCPInitialize_DefaultsWhenNoVersion(t *testing.T) {
	if r := mcpInitialize(nil); r["protocolVersion"] == "" {
		t.Error("must default a protocolVersion when the client sends none")
	}
	if r := mcpInitialize(json.RawMessage(`{}`)); r["protocolVersion"] == "" {
		t.Error("empty params must still yield a default protocolVersion")
	}
}

func TestMCPToolDescriptors(t *testing.T) {
	tools := mcpToolDescriptors()
	want := map[string]bool{"wt_status": false, "wt_check": false, "wt_todos": false, "wt_where": false}
	for _, tl := range tools {
		name, _ := tl["name"].(string)
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected tool %q", name)
			continue
		}
		want[name] = true
		if _, ok := tl["inputSchema"].(map[string]any); !ok {
			t.Errorf("%s missing inputSchema", name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("missing tool %q", name)
		}
	}
	// wt_check requires paths; wt_where requires target.
	for _, tl := range tools {
		sch := tl["inputSchema"].(map[string]any)
		req, _ := sch["required"].([]string)
		switch tl["name"] {
		case "wt_check":
			if len(req) != 1 || req[0] != "paths" {
				t.Errorf("wt_check required=%v want [paths]", req)
			}
		case "wt_where":
			if len(req) != 1 || req[0] != "target" {
				t.Errorf("wt_where required=%v want [target]", req)
			}
		}
	}
}

func TestDispatchRPC(t *testing.T) {
	// initialize → result carries protocolVersion, preserves id.
	resp := dispatchRPC(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("7"), Method: "initialize"})
	if string(resp.ID) != "7" || resp.Error != nil {
		t.Fatalf("initialize resp=%+v", resp)
	}
	if _, ok := resp.Result.(map[string]any)["protocolVersion"]; !ok {
		t.Errorf("initialize result missing protocolVersion")
	}

	// tools/list → result.tools present.
	resp = dispatchRPC(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("8"), Method: "tools/list"})
	if resp.Error != nil {
		t.Fatalf("tools/list err=%+v", resp.Error)
	}
	if _, ok := resp.Result.(map[string]any)["tools"]; !ok {
		t.Errorf("tools/list missing tools")
	}

	// ping → empty object, no error.
	resp = dispatchRPC(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("9"), Method: "ping"})
	if resp.Error != nil {
		t.Errorf("ping should not error: %+v", resp.Error)
	}

	// unknown method → -32601, id preserved.
	resp = dispatchRPC(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("10"), Method: "no/such"})
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Errorf("unknown method err=%+v want -32601", resp.Error)
	}
	if string(resp.ID) != "10" {
		t.Errorf("error response must preserve id, got %s", resp.ID)
	}
}

func TestRunMCPTool_UnknownTool(t *testing.T) {
	// config.Load fails outside a repo, or returns an unknown-tool error inside
	// one; either way this must be a graceful isError, never a panic.
	text, isErr := runMCPTool("wt_bogus", json.RawMessage(`{}`))
	if !isErr {
		t.Errorf("unknown tool must be isError, got %q", text)
	}
	if !strings.Contains(text, "unknown tool") && !strings.Contains(text, "not in a git repo") {
		t.Errorf("unexpected text: %q", text)
	}
}
