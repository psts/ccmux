package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/store"
)

func agentsServer(t *testing.T) *Server {
	s := settingsServer(t)
	s.SetAgents(agent.NewStore(filepath.Join(t.TempDir(), "agents")), 6)
	return s
}

func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAgents_CRUDAndValidation(t *testing.T) {
	s := agentsServer(t)
	if rec := do(t, s, "GET", "/v1/agents", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"agents":[]`) {
		t.Fatalf("empty list: %d %s", rec.Code, rec.Body)
	}
	rec := do(t, s, "PUT", "/v1/agents/x-poster", `{"description":"Posts X threads about releases.","instructions":"# Role\n"}`)
	if rec.Code != 200 {
		t.Fatalf("put = %d %s", rec.Code, rec.Body)
	}
	var d agent.Definition
	json.Unmarshal(rec.Body.Bytes(), &d)
	if d.Name != "x-poster" || d.Version != "1.0.0" || d.Harness != agent.DefaultHarness || d.Instructions != "# Role\n" {
		t.Fatalf("stored = %+v", d)
	}
	if rec := do(t, s, "PUT", "/v1/agents/x-poster", `{"description":""}`); rec.Code != 400 {
		t.Fatalf("empty description should 400, got %d", rec.Code)
	}
	if rec := do(t, s, "PUT", "/v1/agents/x-poster", `{"description":"d","harness":"no-such-harness"}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "harness") {
		t.Fatalf("unknown harness should 400 naming it, got %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "PUT", "/v1/agents/x-poster", `{"description":"d","harness":"claude"}`); rec.Code != 200 {
		t.Fatalf("builtin harness should be accepted, got %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "GET", "/v1/agents", ""); !strings.Contains(rec.Body.String(), `"name":"x-poster"`) {
		t.Fatalf("list after put: %s", rec.Body)
	}
	if rec := do(t, s, "DELETE", "/v1/agents/x-poster", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := do(t, s, "DELETE", "/v1/agents/x-poster", ""); rec.Code != 404 {
		t.Fatalf("second delete = %d", rec.Code)
	}
}

func TestAgents_UnavailableWithoutStore(t *testing.T) {
	s := settingsServer(t)
	for _, c := range []struct{ m, p string }{{"GET", "/v1/agents"}, {"PUT", "/v1/agents/a-b-c"}, {"DELETE", "/v1/agents/a-b-c"}} {
		if rec := do(t, s, c.m, c.p, `{"description":"d"}`); rec.Code != 503 {
			t.Errorf("%s %s = %d, want 503", c.m, c.p, rec.Code)
		}
	}
}

// TestWorkspaceAgents_AddWakeStop drives an instance through its life with a
// harmless harness: added with a prompt (pane spawned, folder bootstrapped),
// asleep once the command exits, woken by a second start, closed by DELETE.
func TestWorkspaceAgents_AddWakeStop(t *testing.T) {
	_, base := harnessStack(t)
	ws := createWS(t, base)
	put := func(path, body string) *http.Response {
		req, _ := http.NewRequest("PUT", base+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := put("/v1/settings", `{"harnesses":[{"name":"noop","icon":"·","command":": noop-harness"}]}`); resp.StatusCode != 200 {
		t.Fatalf("put harnesses: %d", resp.StatusCode)
	}
	if resp := put("/v1/agents/x-poster", `{"description":"Posts X threads.","harness":"noop","instructions":"# Role\n"}`); resp.StatusCode != 200 {
		t.Fatalf("put agent: %d", resp.StatusCode)
	}

	// Absent before it is added.
	var list struct {
		Agents []agentInstance `json:"agents"`
	}
	getList := func() []agentInstance {
		resp, err := http.Get(base + "/v1/workspaces/" + ws.ID + "/agents")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		json.NewDecoder(resp.Body).Decode(&list)
		return list.Agents
	}
	if l := getList(); len(l) != 1 || l[0].State != "absent" || l[0].Version != "1.0.0" {
		t.Fatalf("before add: %+v", l)
	}

	// Add with a prompt: 201, pane stamped, folder bootstrapped, prompt not persisted.
	resp, err := http.Post(base+"/v1/workspaces/"+ws.ID+"/agents/x-poster", "application/json",
		strings.NewReader(`{"prompt":"hello there","createdBy":"tester"}`))
	if err != nil {
		t.Fatal(err)
	}
	var pane struct {
		ID, CWD, StartupCommand, Agent, AgentVersion string
	}
	json.NewDecoder(resp.Body).Decode(&pane)
	resp.Body.Close()
	if resp.StatusCode != 201 || pane.Agent != "x-poster" || pane.AgentVersion != "1.0.0" {
		t.Fatalf("add = %d %+v", resp.StatusCode, pane)
	}
	if !strings.HasSuffix(pane.CWD, "/.ccmux/agents/x-poster") || strings.Contains(pane.StartupCommand, "hello there") || !strings.HasPrefix(pane.StartupCommand, "CLAUDE_PEERS_NAME=x-poster ") {
		t.Fatalf("pane cwd/command: %+v", pane)
	}
	for _, f := range []string{"AGENTS.md", "opencode.jsonc", "memory/MEMORY.md", "log.md"} {
		if _, err := os.Stat(pane.CWD + "/" + f); err != nil {
			t.Errorf("instance folder missing %s", f)
		}
	}

	// The noop command exits at once, so the instance is asleep; wake it.
	deadline := time.Now().Add(8 * time.Second)
	for {
		if l := getList(); l[0].State == "asleep" && l[0].Pane == pane.ID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never went asleep: %+v", getList())
		}
		time.Sleep(100 * time.Millisecond)
	}
	resp, _ = http.Post(base+"/v1/workspaces/"+ws.ID+"/agents/x-poster", "application/json", strings.NewReader(`{"prompt":"again"}`))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("wake = %d", resp.StatusCode)
	}

	// Stop closes the pane; the instance is absent again but its folder stays.
	req, _ := http.NewRequest("DELETE", base+"/v1/workspaces/"+ws.ID+"/agents/x-poster", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("stop = %d", resp.StatusCode)
	}
	if l := getList(); l[0].State != "absent" {
		t.Fatalf("after stop: %+v", l)
	}
	if _, err := os.Stat(pane.CWD + "/AGENTS.md"); err != nil {
		t.Error("instance folder must survive a stop")
	}
	// Unknown agent → 404.
	resp, _ = http.Post(base+"/v1/workspaces/"+ws.ID+"/agents/nobody", "application/json", strings.NewReader(`{}`))
	if resp.StatusCode != 404 {
		t.Fatalf("unknown agent = %d", resp.StatusCode)
	}
}

func TestAgents_PeersHooksWiredInEitherOrder(t *testing.T) {
	for _, agentsFirst := range []bool{true, false} {
		s := settingsServer(t)
		st := agent.NewStore(filepath.Join(t.TempDir(), "agents"))
		pst, err := store.Open(filepath.Join(t.TempDir(), "peers.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pst.Close() })
		svc := peers.NewService(pst, nullHook{}, testSecret)
		if agentsFirst {
			s.SetAgents(st, 6)
			s.EnablePeers(svc)
		} else {
			s.EnablePeers(svc)
			s.SetAgents(st, 6)
		}
		if svc.IsAgent == nil || svc.StartAgent == nil || svc.PushToPane == nil {
			t.Fatalf("agentsFirst=%v: bus hooks not wired", agentsFirst)
		}
	}
}

func TestAgents_PutMergesOverStoredDefinition(t *testing.T) {
	s := agentsServer(t)
	rec := do(t, s, "PUT", "/v1/agents/x-poster", `{"description":"d","permissions":{"bash":"deny","bashAllow":["git log *"]},"model":"anthropic/claude-opus-5","instructions":"# Role\n"}`)
	if rec.Code != 200 {
		t.Fatalf("first put = %d %s", rec.Code, rec.Body)
	}
	rec = do(t, s, "PUT", "/v1/agents/x-poster", `{"description":"d2"}`)
	if rec.Code != 200 {
		t.Fatalf("second put = %d %s", rec.Code, rec.Body)
	}
	var d agent.Definition
	json.Unmarshal(rec.Body.Bytes(), &d)
	if d.Description != "d2" || d.Permissions.Bash != "deny" || len(d.Permissions.BashAllow) != 1 || d.Model != "anthropic/claude-opus-5" || d.Instructions != "# Role\n" {
		t.Fatalf("a partial PUT must not reset the rest: %+v", d)
	}
	if d.Version != "1.0.1" || d.IdleExitMinutes != agent.DefaultIdleExitMinutes {
		t.Fatalf("version %s idle %d: a changed description bumps, a new agent got the idle default", d.Version, d.IdleExitMinutes)
	}
	rec = do(t, s, "PUT", "/v1/agents/x-poster", `{"idleExitMinutes":0}`)
	json.Unmarshal(rec.Body.Bytes(), &d)
	if d.IdleExitMinutes != 0 {
		t.Fatalf("idle 0 (never) must survive a save, got %d", d.IdleExitMinutes)
	}
}
