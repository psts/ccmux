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
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude-code"}}}`)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	result := frames[0]["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion = %v, want the 2025-06-18 the client asked for", result["protocolVersion"])
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
	want := []string{"list_peers", "send_message", "delegate", "update_task", "set_summary", "check_messages"}
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

// Negotiation, not a pin and not an echo. A revision the server speaks is agreed
// to as asked; anything else gets the newest it does speak, leaving the client to
// accept the older revision or terminate. Echoing an unknown version back — the
// original behavior — claimed support for whatever was asked for, which matters
// most for a revision like 2026-07-28 whose stateless core removes the
// server-initiated notifications this server's delivery path depends on.
func TestMCP_InitializeNegotiatesProtocolVersion(t *testing.T) {
	cases := map[string]string{
		"2025-11-25":    "2025-11-25", // current spec, and what Claude Code prefers
		"2025-06-18":    "2025-06-18", // also spoken; agreed as asked
		"2026-07-28":    "2025-11-25", // not spoken -> newest we do
		"2025-03-26":    "2025-11-25", // allowed batching, which Serve cannot parse
		"2024-11-05":    "2025-11-25",
		"not-a-version": "2025-11-25",
		"":              "2025-11-25", // absent field invents no claim
	}
	for offered, want := range cases {
		frames := runFrames(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+offered+
			`","capabilities":{},"clientInfo":{"name":"claude-code"}}}`)
		if len(frames) != 1 {
			t.Fatalf("offered %q: got %d frames, want 1", offered, len(frames))
		}
		if got := frames[0]["result"].(map[string]any)["protocolVersion"]; got != want {
			t.Errorf("client offered %q, server answered %v; want %s", offered, got, want)
		}
	}
}

// The newest supported revision leads the list, because that is what an unknown
// request falls back to.
func TestSupportedProtocolVersions_NewestFirst(t *testing.T) {
	if len(supportedProtocolVersions) < 1 {
		t.Fatal("no supported protocol versions declared")
	}
	for i := 1; i < len(supportedProtocolVersions); i++ {
		if supportedProtocolVersions[i-1] <= supportedProtocolVersions[i] {
			t.Errorf("versions are not newest-first: %q before %q",
				supportedProtocolVersions[i-1], supportedProtocolVersions[i])
		}
	}
	if got := negotiateProtocolVersion("nonsense"); got != supportedProtocolVersions[0] {
		t.Errorf("unknown request fell back to %q, want the newest %q", got, supportedProtocolVersions[0])
	}
}

// A 2026-07-28 client may probe with server/discover before falling back to the
// legacy handshake. The answer must be well-formed and honest: only the legacy
// revisions this server actually implements, so the probe resolves to a clean
// legacy fallback instead of a method-not-found (or worse, a claimed era whose
// semantics this server does not speak).
func TestMCP_ServerDiscoverAdvertisesLegacyOnly(t *testing.T) {
	frames := runFrames(t,
		`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	result, ok := frames[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("server/discover errored: %v — a modern client's probe must get a result", frames[0]["error"])
	}
	if result["resultType"] != "complete" {
		t.Fatalf("resultType = %v, want complete", result["resultType"])
	}
	var versions []string
	for _, v := range result["supportedVersions"].([]any) {
		versions = append(versions, v.(string))
	}
	if strings.Join(versions, ",") != strings.Join(supportedProtocolVersions, ",") {
		t.Fatalf("supportedVersions = %v, want exactly %v (advertise only what the era code implements)",
			versions, supportedProtocolVersions)
	}
	caps := result["capabilities"].(map[string]any)
	exp := caps["experimental"].(map[string]any)
	if _, ok := exp["claude/channel"]; !ok {
		t.Fatal("discover response missing claude/channel — push delivery would silently die on the discover path")
	}
	if _, ok := result["instructions"].(string); !ok {
		t.Fatal("discover response missing instructions")
	}
}

// 2025-06-18 removed JSON-RPC batching, and this server's pin asserts that
// revision. Every frame it writes must therefore be a single JSON object, never
// an array — the claim is only true while that holds.
func TestMCP_NeverWritesABatch(t *testing.T) {
	frames := runFrames(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping","params":{}}`,
		`{"jsonrpc":"2.0","id":4,"method":"no/such/method","params":{}}`)
	if len(frames) != 4 {
		t.Fatalf("got %d frames, want one object per request", len(frames))
	}
	for i, f := range frames {
		if f["jsonrpc"] != "2.0" {
			t.Errorf("frame %d is not a lone JSON-RPC object: %v", i, f)
		}
	}
}
