package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// Skills from dropped files, MCP servers from a pasted snippet, both
// bumping the base; the instances list across windows; restart answers
// with the count of running ones it took on.
func TestAgentFiles_SkillsMCPInstances(t *testing.T) {
	f := newWindowAgentFixture(t, "sleep 1;:")
	base := "/v1/agents/x-poster"
	rec := do(t, f.srv, "POST", base+"/skills", `{"name":"given","files":{"SKILL.md":"---\nname: post-thread\ndescription: Posts.\n---\n# go\n","refs/a.md":"x"}}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"post-thread"`) {
		t.Fatalf("add skill = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, f.srv, "GET", base+"/skills/post-thread", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "# go") {
		t.Fatalf("skill text = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, f.srv, "POST", base+"/skills", `{"url":"not a url at all with spaces"}`); rec.Code != 400 {
		t.Fatalf("a bad source = %d, want 400", rec.Code)
	}
	if rec := do(t, f.srv, "POST", base+"/mcp", `{"mcpServers":{"gh":{"command":"npx","args":["x"]}}}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"gh"`) {
		t.Fatalf("add mcp = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, f.srv, "GET", base+"/mcp", ""); !strings.Contains(rec.Body.String(), `"command":"npx"`) {
		t.Fatalf("list mcp: %s", rec.Body)
	}
	if rec := do(t, f.srv, "GET", "/v1/agents", ""); !strings.Contains(rec.Body.String(), `"version":"1.0.2"`) {
		t.Fatalf("two changes bump the base twice: %s", rec.Body)
	}
	if rec := do(t, f.srv, "DELETE", base+"/skills/post-thread", ""); rec.Code != 204 {
		t.Fatalf("delete skill = %d", rec.Code)
	}
	if rec := do(t, f.srv, "DELETE", base+"/mcp/gh", ""); rec.Code != 204 {
		t.Fatalf("delete mcp = %d", rec.Code)
	}
	if rec := do(t, f.srv, "GET", "/v1/agents/nope/skills", ""); rec.Code != 404 {
		t.Fatalf("unknown agent = %d", rec.Code)
	}

	// Not deployed anywhere yet: no instances, nothing to restart.
	if rec := do(t, f.srv, "GET", base+"/instances", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"instances":[]`) {
		t.Fatalf("instances before add = %d %s", rec.Code, rec.Body)
	}
	f.start(t, "x-poster", "")
	f.waitState(t, "running")
	rec = do(t, f.srv, "GET", base+"/instances", "")
	var got struct {
		Instances []agentDeployment `json:"instances"`
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Instances) != 1 || got.Instances[0].Window != "Chart Labs" || got.Instances[0].State != "running" || got.Instances[0].PaneVersion == "" {
		t.Fatalf("instances = %+v", got.Instances)
	}
	if rec := do(t, f.srv, "POST", base+"/instances/restart", ""); rec.Code != 202 || !strings.Contains(rec.Body.String(), `"restarting":1`) {
		t.Fatalf("restart = %d %s", rec.Code, rec.Body)
	}
}
