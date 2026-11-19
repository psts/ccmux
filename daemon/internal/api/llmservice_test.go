package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/llmserve"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
)

// With the proxy in its own service, the daemon's pane routes stay mounted
// for the panes that still carry the daemon's address, and hand what arrives
// to the service instead of serving it. The loopback guard stays in front.
func TestLLMPaneRoutesForwardToTheService(t *testing.T) {
	s := llmServer(t)
	var forwarded []string
	s.SetLLMForward(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = append(forwarded, r.Method+" "+r.URL.EscapedPath())
	}))
	h := s.Handler()

	for _, c := range []struct{ method, path string }{
		{"POST", "/llm/pane/p1/v1/messages"},
		{"GET", "/llm/pane/p1/api/hello"},
		{"HEAD", "/llm/pane/p1/api/hello"},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
		req.RemoteAddr = "127.0.0.1:9999"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s %s = %d", c.method, c.path, rec.Code)
		}
	}
	want := []string{"POST /llm/pane/p1/v1/messages", "GET /llm/pane/p1/api/hello", "HEAD /llm/pane/p1/api/hello"}
	if !reflect.DeepEqual(forwarded, want) {
		t.Fatalf("forwarded %v, want %v", forwarded, want)
	}

	req := httptest.NewRequest("POST", "/llm/pane/p1/v1/messages", strings.NewReader("{}"))
	req.RemoteAddr = "100.64.1.2:9999" // tailnet, not loopback
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || len(forwarded) != len(want) {
		t.Fatalf("a tailnet caller got %d and %d requests were forwarded; want 403 and none", rec.Code, len(forwarded)-len(want))
	}
}

// GET /v1/health says where the LLM proxy runs once that is wired, and
// keeps one "version" key: build-daemon.sh reads the daemon's version with a
// greedy sed that takes the LAST one in the answer.
func TestHealthCarriesTheLLMProxy(t *testing.T) {
	s := llmServer(t)
	get := func() (map[string]any, string) {
		rec := httptest.NewRecorder()
		s.health(rec, httptest.NewRequest("GET", "/v1/health", nil))
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out, rec.Body.String()
	}
	if out, _ := get(); out["llmProxy"] != nil {
		t.Fatalf("health names an llm proxy nobody wired: %v", out["llmProxy"])
	}
	s.SetLLMProxyHealth(func() map[string]any {
		return map[string]any{"mode": "service", "url": "http://127.0.0.1:7901", "serving": true, "codeHash": "abc", "wantHash": "abc"}
	})
	out, raw := get()
	proxy, _ := out["llmProxy"].(map[string]any)
	if proxy["mode"] != "service" || proxy["url"] != "http://127.0.0.1:7901" {
		t.Fatalf("llmProxy = %v", out["llmProxy"])
	}
	if n := strings.Count(raw, `"version":`); n != 1 {
		t.Fatalf("health JSON has %d \"version\" keys, want exactly one: %s", n, raw)
	}
}

// ROUTING EQUIVALENCE against the real manager. The proxy used to resolve a
// pane's harness from the manager's memory, inside the daemon. As its own
// service it resolves it from the database. For every pane a real manager
// holds, tmux panes and runner sessions alike, the two must build the same
// failover order.
func TestLLMServiceRoutesPanesAsTheDaemonDoes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reg.db")
	srv, mgr, base, st := harnessStackAt(t, dbPath)
	p := seedRoutedPanes(t, srv, mgr, base, st)

	ids := []string{p.shell, p.claude, p.picky, p.overridden, "runner-live", "runner-done", "runner-orphan", "never-existed"}
	want, wantFail, err := srv.llm.PaneOrders(ids) // in the daemon, from the manager's memory
	if err != nil {
		t.Fatal(err)
	}
	reader, err := llmserve.OpenReader(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, gotFail, err := llmserve.NewService(reader).PaneOrders(ids) // in the service, from the database
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotFail, wantFail) {
		t.Fatalf("the service and the daemon disagree on who answers:\nservice %v (failures %v)\ndaemon  %v (failures %v)", got, gotFail, want, wantFail)
	}
	// The comparison covers every tier, not eight copies of the default.
	for pane, order := range map[string][]string{
		p.shell:       {"router"},
		p.claude:      {"sub-1", "router", "sub-2"},
		p.picky:       {"sub-2", "sub-1", "router"},
		p.overridden:  {"sub-2", "sub-1", "router"},
		"runner-live": {"sub-1", "router", "sub-2"},
		"runner-done": {"router"},
	} {
		if !reflect.DeepEqual(want[pane], order) {
			t.Errorf("pane %s: the daemon's own order is %v, want %v", pane, want[pane], order)
		}
	}
}

// routedPanes are the tmux panes seedRoutedPanes made, by what they run.
type routedPanes struct{ shell, claude, picky, overridden string }

// seedRoutedPanes configures accounts and harnesses through the API, opens
// one tmux pane of each kind in a real workspace, and adds three runner
// sessions (live, ended, and one whose workspace is gone) under the pane ids
// runner-live, runner-done and runner-orphan.
func seedRoutedPanes(t *testing.T, srv *Server, mgr *manager.Manager, base string, st *store.SQLite) routedPanes {
	t.Helper()
	// Two subscriptions, a keyed gateway and a codex account; a stand-in for
	// claude (the real command must not start in a test) and a harness with
	// its own account order.
	putSettingsOK(t, base, `{"llmAccounts":[
		{"name":"sub-1","kind":"claude","baseURL":"http://127.0.0.1:1","apiKey":"t1"},
		{"name":"router","kind":"openai","baseURL":"http://127.0.0.1:1","apiKey":"k"},
		{"name":"sub-2","kind":"claude","baseURL":"http://127.0.0.1:1","apiKey":"t2"},
		{"name":"gpt","kind":"codex","baseURL":"http://127.0.0.1:1"}],
		"llmRoute":"router"}`)
	putSettingsOK(t, base, `{"harnesses":[
		{"name":"claude","icon":"c","command":": claude-stand-in"},
		{"name":"picky","icon":"p","command":": picky","accountKinds":["openai","claude"],"accountOrder":["sub-2"]}]}`)

	ws := createWS(t, base) // its first pane is a bare shell
	p := routedPanes{
		shell: ws.Panes[0].ID, claude: spawnHarnessPane(t, base, ws.ID, "claude"),
		picky: spawnHarnessPane(t, base, ws.ID, "picky"), overridden: spawnHarnessPane(t, base, ws.ID, "claude"),
	}
	if err := srv.llm.SetPaneRoute(p.overridden, "sub-2"); err != nil {
		t.Fatal(err)
	}
	// The manager builds a runner session's pane from its row (SyncRunnerPane).
	for _, sess := range []store.RunnerSession{
		{ID: "s-live", Cwd: "/tmp", Wanted: store.SessionRunning, PaneID: "runner-live", WorkspaceID: ws.ID},
		{ID: "s-done", Cwd: "/tmp", Wanted: store.SessionDone, PaneID: "runner-done", WorkspaceID: ws.ID},
		{ID: "s-orphan", Cwd: "/tmp", Wanted: store.SessionRunning, PaneID: "runner-orphan", WorkspaceID: "ws-deleted"},
	} {
		if err := st.AddRunnerSession(sess); err != nil {
			t.Fatal(err)
		}
		mgr.SyncRunnerPane(sess)
	}
	return p
}

func putSettingsOK(t *testing.T, base, body string) {
	t.Helper()
	req, _ := http.NewRequest("PUT", base+"/v1/settings", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("put settings = %d", resp.StatusCode)
	}
}

// spawnHarnessPane opens a pane running the named harness and returns its id.
func spawnHarnessPane(t *testing.T, base, wsID, harness string) string {
	t.Helper()
	resp, err := http.Post(base+"/v1/workspaces/"+wsID+"/panes", "application/json",
		strings.NewReader(`{"harness":"`+harness+`","createdBy":"tester"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var pane struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pane); err != nil || resp.StatusCode != 201 {
		t.Fatalf("spawn %s = %d %v", harness, resp.StatusCode, err)
	}
	return pane.ID
}
