package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// wantCapabilities is what both handshake answers must declare, written out
// here on its own: tools, the channel, and the permission relay. Claude Code
// registers the session's push channel off exactly these keys.
func wantCapabilities() map[string]any {
	return map[string]any{
		"tools": map[string]any{},
		"experimental": map[string]any{
			"claude/channel":            map[string]any{},
			"claude/channel/permission": map[string]any{},
		},
	}
}

// The two handshake answers, whole. A legacy client gets initialize; a
// 2026-07-28 client probes server/discover first. Every key matters: the
// capabilities are how the channel is registered, the server name is baked
// into the launch flag, and the instructions are the bus rules.
func TestHandlers_HandshakeAnswersAreWhole(t *testing.T) {
	a := &app{mcp: newMCPServerIO(strings.NewReader(""), io.Discard)}
	a.installHandlers()
	info := map[string]any{"name": "claude-peers", "version": shimVersion}

	got, rpcErr := a.mcp.onRequest["initialize"](json.RawMessage(`{"protocolVersion":"2025-06-18"}`))
	want := map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    wantCapabilities(),
		"serverInfo":      info,
		"instructions":    serverInstructions,
	}
	if rpcErr != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("initialize (%v):\n got %v\nwant %v", rpcErr, got, want)
	}

	got, rpcErr = a.mcp.onRequest["server/discover"](json.RawMessage(`{}`))
	want = map[string]any{
		"resultType":        "complete",
		"supportedVersions": supportedProtocolVersions,
		"capabilities":      wantCapabilities(),
		"instructions":      serverInstructions,
		"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": info},
	}
	if rpcErr != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("server/discover (%v):\n got %v\nwant %v", rpcErr, got, want)
	}
}

// A request for a revision this server does not speak is answered with the
// newest it does, never echoed back.
func TestHandlers_InitializeAnswersAnUnknownRevisionWithItsNewest(t *testing.T) {
	a := &app{mcp: newMCPServerIO(strings.NewReader(""), io.Discard)}
	a.installHandlers()
	for _, params := range []string{`{"protocolVersion":"2026-07-28"}`, `{}`, `not json`} {
		got, rpcErr := a.mcp.onRequest["initialize"](json.RawMessage(params))
		if rpcErr != nil || got.(map[string]any)["protocolVersion"] != supportedProtocolVersions[0] {
			t.Fatalf("initialize %s: %v, %v; want protocolVersion %s", params, got, rpcErr, supportedProtocolVersions[0])
		}
	}
}

// tools/call hands the named tool its arguments, and refuses a body it cannot
// read with the JSON-RPC "invalid params" code instead of calling anything.
func TestHandlers_ToolCallRefusesABodyItCannotRead(t *testing.T) {
	a := &app{mcp: newMCPServerIO(strings.NewReader(""), io.Discard), id: "p1"}
	a.installHandlers()

	got, rpcErr := a.mcp.onRequest["tools/call"](json.RawMessage(`{"name":"no_such_tool","arguments":{}}`))
	if answer, _ := json.Marshal(got); rpcErr != nil || !bytes.Contains(answer, []byte("Unknown tool: no_such_tool")) {
		t.Fatalf("a readable call must reach the tool table: %s, %v", answer, rpcErr)
	}
	got, rpcErr = a.mcp.onRequest["tools/call"](json.RawMessage(`{"name":`))
	if got != nil || rpcErr == nil || rpcErr.Code != -32602 {
		t.Fatalf("an unreadable call: got %v, %v; want no result and code -32602", got, rpcErr)
	}
}

// Claude Code opened a tool-approval dialog: the shim hands it to the daemon
// for this session's peer, field for field, so a delegator can answer it.
// A session with no peer id yet, and a notice it cannot read, relay nothing.
func TestHandlers_PermissionRequestIsRelayedToTheDaemon(t *testing.T) {
	var mu sync.Mutex
	var posts []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["_path"] = r.URL.Path
		mu.Lock()
		posts = append(posts, body)
		mu.Unlock()
		fmt.Fprint(w, `{"relayed_to":2}`)
	}))
	defer srv.Close()
	a := &app{mcp: newMCPServerIO(strings.NewReader(""), io.Discard), daemon: newDaemonClient(srv.URL, "t"), id: "p1"}
	a.installHandlers()
	relay := a.mcp.onNotify["notifications/claude/channel/permission_request"]
	notice := `{"request_id":"abcde","tool_name":"Bash","description":"Run the tests","input_preview":"{\"command\":\"go test\"}"}`

	relay(json.RawMessage(notice))
	want := map[string]any{
		"_path": "/v1/peers/permission-request", "peer_id": "p1", "request_id": "abcde",
		"tool_name": "Bash", "description": "Run the tests", "input_preview": `{"command":"go test"}`,
	}
	if len(posts) != 1 || !reflect.DeepEqual(posts[0], want) {
		t.Fatalf("relayed %v, want exactly %v", posts, want)
	}

	relay(json.RawMessage(`{"request_id":`)) // unreadable
	a.id = ""
	relay(json.RawMessage(notice)) // not registered: there is no peer to relay for
	if len(posts) != 1 {
		t.Fatalf("an unreadable notice or an unregistered session relayed anyway: %v", posts[1:])
	}
}
