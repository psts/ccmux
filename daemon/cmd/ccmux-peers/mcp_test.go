package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// runFrames feeds newline-delimited JSON-RPC frames through a fully-wired app
// and returns the decoded output frames.
func runFrames(t *testing.T, frames ...string) []map[string]any {
	t.Helper()
	in := strings.NewReader(strings.Join(frames, "\n") + "\n")
	var out bytes.Buffer
	a := &app{mcp: newMCPServerIO(in, &out), channelMode: true}
	a.daemon = newDaemonClient("http://127.0.0.1:1", "") // unreachable — tools error fast
	a.installHandlers()
	a.mcp.Serve()

	var decoded []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("bad output frame %q: %v", line, err)
		}
		decoded = append(decoded, frame)
	}
	return decoded
}

func TestMCP_InitializeDeclaresChannelCapabilities(t *testing.T) {
	frames := runFrames(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"claude-code"}}}`)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	result := frames[0]["result"].(map[string]any)
	if result["protocolVersion"] != "2025-03-26" {
		t.Fatalf("protocolVersion = %v, want echo of client's", result["protocolVersion"])
	}
	caps := result["capabilities"].(map[string]any)
	exp := caps["experimental"].(map[string]any)
	if _, ok := exp["claude/channel"]; !ok {
		t.Fatal("missing claude/channel capability — channel push would silently not work")
	}
	if _, ok := exp["claude/channel/permission"]; !ok {
		t.Fatal("missing claude/channel/permission capability")
	}
	info := result["serverInfo"].(map[string]any)
	if info["name"] != "claude-peers" {
		t.Fatalf(`serverInfo.name = %v, want "claude-peers" (baked into the launch flag)`, info["name"])
	}
	instr := result["instructions"].(string)
	for _, marker := range []string{"[claude-peers permission relay]", "send_message", "Do NOT use the built-in SendMessage"} {
		if !strings.Contains(instr, marker) {
			t.Fatalf("instructions missing %q", marker)
		}
	}
}

func TestMCP_ToolsListIsVerbatimSurface(t *testing.T) {
	frames := runFrames(t, `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)
	tools := frames[0]["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, tl := range tools {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	want := []string{"list_peers", "send_message", "set_summary", "check_messages"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", names, want)
	}
}

func TestMCP_UnknownMethodAndUnregisteredTool(t *testing.T) {
	frames := runFrames(t,
		`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_peers","arguments":{}}}`)
	if errObj := frames[0]["error"].(map[string]any); errObj["code"].(float64) != -32601 {
		t.Fatalf("unknown method error = %v, want -32601", errObj)
	}
	// Not registered yet → the verbatim broker-era error text.
	result := frames[1]["result"].(map[string]any)
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if text != "Not registered with broker yet" || result["isError"] != true {
		t.Fatalf("unregistered tool call = %q (isError=%v)", text, result["isError"])
	}
}

func TestDispatchEvent_EmitsChannelAndPermissionNotifications(t *testing.T) {
	var out bytes.Buffer
	a := &app{mcp: newMCPServerIO(strings.NewReader(""), &out), channelMode: true}

	if !a.dispatchEvent(wireEvent{Type: "message", Seq: 3, FromID: "abc", FromName: "backend",
		FromSummary: "s", FromCWD: "/w/backend", Text: "hello", SentAt: "2026-07-15T12:00:00.000Z"}) {
		t.Fatal("message dispatch reported failure")
	}
	if !a.dispatchEvent(wireEvent{Type: "permission_verdict", Seq: 4, RequestID: "abcde", Behavior: "allow", FromID: "abc"}) {
		t.Fatal("verdict dispatch reported failure")
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d notifications, want 2", len(lines))
	}
	var msg, verdict map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &msg); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &verdict); err != nil {
		t.Fatal(err)
	}

	if msg["method"] != "notifications/claude/channel" {
		t.Fatalf("message method = %v", msg["method"])
	}
	params := msg["params"].(map[string]any)
	if params["content"] != "hello" {
		t.Fatalf("content = %v", params["content"])
	}
	meta := params["meta"].(map[string]any)
	if meta["from_id"] != "abc" || meta["from_name"] != "backend" || meta["sent_at"] != "2026-07-15T12:00:00.000Z" {
		t.Fatalf("meta = %v", meta)
	}

	if verdict["method"] != "notifications/claude/channel/permission" {
		t.Fatalf("verdict method = %v", verdict["method"])
	}
	vp := verdict["params"].(map[string]any)
	if vp["request_id"] != "abcde" || vp["behavior"] != "allow" {
		t.Fatalf("verdict params = %v", vp)
	}
}
