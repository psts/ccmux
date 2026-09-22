package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A pasted snippet may name a local server (command) or a remote one (url),
// in the bare shape or under mcpServers. A remote entry gets type http
// unless it says sse; a local one carries no type. Neither or both is an
// error that names the server.
func TestParseMCPSnippet_LocalAndRemote(t *testing.T) {
	m, err := parseMCPSnippet([]byte(`{"mcpServers":{
		"kb": {"command": "kb-mcp", "args": ["--live"], "env": {"KB_TOKEN": "${KB_TOKEN}"}},
		"door": {"url": "${X_MCP_URL}", "headers": {"authorization": "Bearer ${X_MCP_TOKEN}"}},
		"old": {"type": "sse", "url": "http://h/sse"}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	if kb := m["kb"]; kb.Command != "kb-mcp" || kb.Type != "" || kb.Remote() {
		t.Errorf("local entry = %+v", kb)
	}
	if door := m["door"]; door.Type != "http" || door.URL != "${X_MCP_URL}" || door.Headers["authorization"] == "" || !door.Remote() {
		t.Errorf("remote entry = %+v", door)
	}
	if old := m["old"]; old.Type != "sse" {
		t.Errorf("sse entry = %+v", old)
	}
	for _, bad := range []struct{ snippet, want string }{
		{`{"x": {"args": ["a"]}}`, "needs a command or a url"},
		{`{"x": {"command": "c", "url": "http://h"}}`, "not both"},
		{`{"x": {"type": "grpc", "url": "http://h"}}`, `type "grpc"`},
		{`{"x": {"command": "c", "type": "http"}}`, ""}, // a stray type on a local entry is dropped, not refused
	} {
		got, err := parseMCPSnippet([]byte(bad.snippet))
		if bad.want == "" {
			if err != nil || got["x"].Type != "" {
				t.Errorf("%s: err=%v type=%q, want accepted with no type", bad.snippet, err, got["x"].Type)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s: err=%v, want %q", bad.snippet, err, bad.want)
		}
	}
}

// AddMCP writes a remote entry to mcp.json in the neutral shape and the
// lens listing shows it back with its url; a local one still lists its
// command and never a type.
func TestAddMCP_RemoteRoundTrip(t *testing.T) {
	s := testStore(t)
	if _, err := s.Save(Definition{Name: "x-watch", Description: "d"}); err != nil {
		t.Fatal(err)
	}
	names, err := s.AddMCP("x-watch", []byte(`{"door": {"url": "${X_MCP_URL}"}, "kb": {"command": "kb-mcp"}}`))
	if err != nil || len(names) != 2 {
		t.Fatalf("add: %v %v", names, err)
	}
	raw, _ := os.ReadFile(filepath.Join(s.Dir("x-watch"), "mcp.json"))
	if !strings.Contains(string(raw), `"type": "http"`) || strings.Contains(string(raw), `"command": ""`) {
		t.Errorf("mcp.json:\n%s", raw)
	}
	list, err := s.MCPServers("x-watch")
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %v", list, err)
	}
	if list[0].Name != "door" || list[0].URL != "${X_MCP_URL}" || list[0].Type != "http" || list[0].Command != "" {
		t.Errorf("remote listed as %+v", list[0])
	}
	if list[1].Name != "kb" || list[1].Command != "kb-mcp" || list[1].URL != "" || list[1].Type != "" {
		t.Errorf("local listed as %+v", list[1])
	}
	// The wire shape the lenses decode is flat: the embedded entry's fields
	// sit beside name, and absent ones are left out.
	wire, _ := json.Marshal(list[0])
	if string(wire) != `{"name":"door","type":"http","url":"${X_MCP_URL}"}` {
		t.Errorf("wire shape = %s", wire)
	}
}
