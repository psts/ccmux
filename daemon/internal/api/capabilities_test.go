package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCapabilities_Routes(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // no plugins installed
	s := agentsServer(t)
	if rec := do(t, s, "GET", "/v1/capabilities", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"capabilities":[]`) {
		t.Fatalf("empty catalog: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "PUT", "/v1/capabilities/door", `{"kind":"mcp","server":{"url":"http://d/mcp"}}`); rec.Code != 200 {
		t.Fatalf("put = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "PUT", "/v1/capabilities/bad", `{"kind":"mcp"}`); rec.Code != 400 {
		t.Fatalf("an mcp entry without a server should 400, got %d", rec.Code)
	}
	if rec := do(t, s, "PUT", "/v1/agents/radar", `{"description":"d","harness":"claude","isolated":true,"capabilities":["door"],"toolDeny":["AskUserQuestion"]}`); rec.Code != 200 {
		t.Fatalf("agent put = %d %s", rec.Code, rec.Body)
	}
	rec := do(t, s, "GET", "/v1/capabilities", "")
	var got struct {
		Capabilities []struct {
			ID     string   `json:"id"`
			Skills []string `json:"skills"`
		} `json:"capabilities"`
		Installed []string `json:"installed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Capabilities) != 1 || got.Capabilities[0].ID != "door" || got.Capabilities[0].Skills == nil || got.Installed == nil {
		t.Fatalf("list = %s (%v)", rec.Body, err)
	}
	if rec := do(t, s, "DELETE", "/v1/capabilities/door", ""); rec.Code != 409 || !strings.Contains(rec.Body.String(), "radar") {
		t.Fatalf("an entry in use should 409 naming the agent, got %d %s", rec.Code, rec.Body)
	}
	do(t, s, "PUT", "/v1/agents/radar", `{"capabilities":[]}`)
	if rec := do(t, s, "DELETE", "/v1/capabilities/door", ""); rec.Code != 200 {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "POST", "/v1/capabilities/import", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"proposals":{}`) {
		t.Fatalf("import on an empty host = %d %s", rec.Code, rec.Body)
	}
}
