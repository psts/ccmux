package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/manager"
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

// windowAgentFixture is a daemon with a harmless harness, one base agent
// ("x-poster" on it), and one project session in the shared window "Chart
// Labs". Tests drive the window's agent through it.
type windowAgentFixture struct {
	srv   *Server
	base  string
	winID string
	ws    wsResult
}

func newWindowAgentFixture(t *testing.T, harnessCmd string) *windowAgentFixture {
	t.Helper()
	srv, _, base := harnessStackServer(t)
	f := &windowAgentFixture{srv: srv, base: base, ws: createWS(t, base)}
	// A real (short-lived) process, not a shell builtin: tmux must report a
	// harness in the foreground for the start mark to clear, as it does for
	// every real harness; a builtin never leaves the shell.
	// "sleep 1;:" — sleep is the foreground (not a shell name, unlike sh),
	// and the trailing ":" swallows the prompt the launch line appends.
	f.put(t, "/v1/settings", `{"harnesses":[{"name":"noop","icon":"·","command":"`+harnessCmd+`"}]}`, 200)
	f.put(t, "/v1/agents/x-poster", `{"icon":"🐦","description":"Posts X threads.","harness":"noop","instructions":"# Role\n"}`, 200)
	// The project session joins a shared window; the agent joins THAT.
	f.put(t, "/v1/workspaces/"+f.ws.ID+"/group", `{"group":"Chart Labs"}`, 204)
	f.winID = windowID(t, base, "Chart Labs")
	return f
}

func (f *windowAgentFixture) put(t *testing.T, path, body string, want int) {
	t.Helper()
	req, _ := http.NewRequest("PUT", f.base+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("PUT %s = %d, want %d", path, resp.StatusCode, want)
	}
}

func (f *windowAgentFixture) list(t *testing.T) agentInstance {
	t.Helper()
	resp, err := http.Get(f.base + "/v1/windows/" + f.winID + "/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Agents []agentInstance `json:"agents"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list.Agents) != 1 {
		t.Fatalf("agents = %+v, want one", list.Agents)
	}
	return list.Agents[0]
}

// agentPane is what the add/wake route answers with.
type agentPane struct {
	ID, WorkspaceID, CWD, StartupCommand, Agent, AgentVersion string
}

func (f *windowAgentFixture) start(t *testing.T, name, prompt string) (int, agentPane) {
	t.Helper()
	resp, err := http.Post(f.base+"/v1/windows/"+f.winID+"/agents/"+name, "application/json",
		strings.NewReader(`{"prompt":`+strconv.Quote(prompt)+`,"createdBy":"tester"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var p agentPane
	json.NewDecoder(resp.Body).Decode(&p)
	return resp.StatusCode, p
}

func (f *windowAgentFixture) sleep(t *testing.T) int {
	t.Helper()
	req, _ := http.NewRequest("DELETE", f.base+"/v1/windows/"+f.winID+"/agents/x-poster", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (f *windowAgentFixture) waitState(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		if a := f.list(t); a.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never reached %q: %+v", want, f.list(t))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (f *windowAgentFixture) workspace(t *testing.T, id string) map[string]any {
	t.Helper()
	for _, w := range getJSONList(t, f.base+"/v1/workspaces") {
		if w["id"] == id {
			return w
		}
	}
	t.Fatalf("workspace %s not listed", id)
	return nil
}

// TestWindowAgents_AddWakeSleep drives an instance through its life: added
// with a prompt (a new session in the window, folder bootstrapped), asleep
// once the command exits, woken by a second start, archived and woken by a
// revive, put to sleep by DELETE (session and folder kept).
func TestWindowAgents_AddWakeSleep(t *testing.T) {
	f := newWindowAgentFixture(t, "sleep 1;:")
	if a := f.list(t); a.State != "absent" || a.Version != "1.0.0" {
		t.Fatalf("before add: %+v", a)
	}
	var pane agentPane
	t.Run("add", func(t *testing.T) {
		code, p := f.start(t, "x-poster", "hello there")
		pane = p
		if code != 201 || p.Agent != "x-poster" || p.AgentVersion != "1.0.0" || p.WorkspaceID == f.ws.ID {
			t.Fatalf("add = %d %+v", code, p)
		}
		if !strings.Contains(p.CWD, "/windows/chart-labs-") || !strings.HasSuffix(p.CWD, "/agents/x-poster") || strings.Contains(p.StartupCommand, "hello there") || !strings.HasPrefix(p.StartupCommand, "CLAUDE_PEERS_NAME=x-poster ") {
			t.Fatalf("pane cwd/command: %+v", p)
		}
		for _, file := range []string{"AGENTS.md", "opencode.jsonc", "memory/MEMORY.md", "log.md"} {
			if _, err := os.Stat(p.CWD + "/" + file); err != nil {
				t.Errorf("instance folder missing %s", file)
			}
		}
		ws := f.workspace(t, p.WorkspaceID)
		if ws["agent"] != "x-poster" || ws["name"] != "🐦 x-poster" || ws["group"] != "Chart Labs" {
			t.Fatalf("agent session: %+v", ws)
		}
	})
	t.Run("wake", func(t *testing.T) {
		f.waitState(t, "asleep") // the noop command exits at once
		if a := f.list(t); a.Pane != pane.ID || a.Workspace != pane.WorkspaceID {
			t.Fatalf("asleep instance: %+v", a)
		}
		if code, _ := f.start(t, "x-poster", "again"); code != 200 {
			t.Fatalf("wake = %d", code)
		}
		if n := len(getJSONList(t, f.base+"/v1/workspaces")); n != 2 {
			t.Fatalf("a wake must reuse the agent's session, got %d sessions", n)
		}
	})
	t.Run("archive and revive", func(t *testing.T) {
		resp, _ := http.Post(f.base+"/v1/workspaces/"+pane.WorkspaceID+"/archive?force=1", "application/json", strings.NewReader(`{}`))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("archive agent session = %d", resp.StatusCode)
		}
		if a := f.list(t); a.State != "asleep" {
			t.Fatalf("archived must read asleep: %+v", a)
		}
		// Revive carries the CURRENT launch; the persisted line still has no prompt.
		code, woken := f.start(t, "x-poster", "after archive")
		if code != 200 || woken.ID != pane.ID || strings.Contains(woken.StartupCommand, "after archive") {
			t.Fatalf("wake after archive = %d %+v", code, woken)
		}
		if ws := f.workspace(t, pane.WorkspaceID); ws["status"] != "live" {
			t.Fatalf("agent session not revived: %v", ws["status"])
		}
		// A second wake inside the start window is "already running", not a
		// second launch typed into the same pane.
		if code, _ := f.start(t, "x-poster", "again"); code != 409 {
			t.Fatalf("double wake after revive = %d, want 409", code)
		}
		f.waitState(t, "asleep")
	})
	t.Run("sleep is idempotent", func(t *testing.T) {
		if code := f.sleep(t); code != http.StatusNoContent {
			t.Fatalf("sleep = %d", code)
		}
		if a := f.list(t); a.State == "absent" || a.Workspace != pane.WorkspaceID {
			t.Fatalf("after sleep: %+v", a)
		}
		if _, err := os.Stat(pane.CWD + "/AGENTS.md"); err != nil {
			t.Error("instance folder must survive a sleep")
		}
	})
	t.Run("unknowns are 404", func(t *testing.T) {
		if code, _ := f.start(t, "nobody", ""); code != 404 {
			t.Fatalf("unknown agent = %d", code)
		}
		resp, _ := http.Post(f.base+"/v1/windows/nope/agents/x-poster", "application/json", strings.NewReader(`{}`))
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("unknown window = %d", resp.StatusCode)
		}
	})
}

// A running instance is put to sleep by DELETE: the pane drops to its shell,
// the session stays live. "cat" waits on stdin and quits on the ctrl-d the
// sleep sends, which is exactly the shape of a harness at its prompt.
func TestWindowAgents_SleepEndsARunningInstance(t *testing.T) {
	f := newWindowAgentFixture(t, "cat")
	code, pane := f.start(t, "x-poster", "")
	if code != 201 {
		t.Fatalf("add = %d", code)
	}
	f.waitState(t, "running")
	if code := f.sleep(t); code != http.StatusNoContent {
		t.Fatalf("sleep = %d", code)
	}
	f.waitState(t, "asleep")
	if ws := f.workspace(t, pane.WorkspaceID); ws["status"] != "live" {
		t.Fatalf("sleep must keep the session live, got %v", ws["status"])
	}
	if err := f.srv.mgr.SleepAgent(f.ws.Panes[0].ID); !errors.Is(err, manager.ErrNotAgent) {
		t.Fatalf("sleeping a plain pane = %v, want ErrNotAgent", err)
	}
}

// The agent gets the window's project folders, never its own or another
// agent's, and a wake resolves them through the pane's shared window.
func TestWindowAgents_ProjectFoldersOnTheLaunch(t *testing.T) {
	f := newWindowAgentFixture(t, "sleep 1;:")
	_, pane := f.start(t, "x-poster", "")
	win, _, msg := f.srv.windowByID(f.winID)
	if msg != "" {
		t.Fatal(msg)
	}
	if dirs := f.srv.windowRepos(win); len(dirs) != 1 || dirs[0] != "/tmp" {
		t.Fatalf("window repos = %v, want the project session only", dirs)
	}
	dirs, _, msg := f.srv.windowReposOfPane(pane.ID)
	if msg != "" || len(dirs) != 1 || dirs[0] != "/tmp" {
		t.Fatalf("repos of the agent's pane = %v %q", dirs, msg)
	}
}

// The bus adapter: a group that is not a shared window is refused with the
// reason; a window starts the agent there and a second contact is not an
// error (it is running, the bus delivers directly).
func TestWindowAgents_BusStartsInTheSendersWindow(t *testing.T) {
	f := newWindowAgentFixture(t, "cat")
	err := f.srv.startAgentForPeer("/home/u/somewhere", "x-poster", "hi")
	if err == nil || !strings.Contains(err.Error(), "joins a shared window") {
		t.Fatalf("ungrouped sender = %v", err)
	}
	if err := f.srv.startAgentForPeer("chart labs", "x-poster", "hi"); err != nil {
		t.Fatalf("start in window: %v", err)
	}
	f.waitState(t, "running")
	if err := f.srv.startAgentForPeer("Chart Labs", "x-poster", "again"); err != nil {
		t.Fatalf("a running instance is not a bus error: %v", err)
	}
	if a := f.list(t); a.State != "running" {
		t.Fatalf("after bus contact: %+v", a)
	}
}

// windowID finds a shared window by name through GET /v1/windows.
func windowID(t *testing.T, base, name string) string {
	t.Helper()
	for _, w := range getJSONList(t, base+"/v1/windows") {
		if w["name"] == name {
			return w["id"].(string)
		}
	}
	t.Fatalf("window %q not listed", name)
	return ""
}

func getJSONList(t *testing.T, url string) []map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s: %v", url, err)
	}
	return out
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

func TestPeerStartError_InFlightIsNotAFailure(t *testing.T) {
	if err := peerStartError(startOutcome{status: 201}); err != nil {
		t.Fatalf("started: %v", err)
	}
	if err := peerStartError(startOutcome{status: 409, msg: "agent x is already running", err: manager.ErrAgentRunning}); err != nil {
		t.Fatalf("in flight or running must be fine for the bus: %v", err)
	}
	if err := peerStartError(startOutcome{status: 409, msg: "cap reached", err: manager.ErrAgentCap}); err == nil {
		t.Fatal("the cap is a failure")
	}
	if err := peerStartError(startOutcome{status: 404, msg: "no such agent"}); err == nil || err.Error() != "no such agent" {
		t.Fatalf("other refusals carry their message: %v", err)
	}
}
