package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/loadout"
	"ccmux.dev/ccmuxd/internal/store"
)

// choicesServer is sessionsServer with loadouts on: a catalog holding qa,
// kb and a connector, the repo session ws-app in window w1, and the real
// registry behind the session routes.
func choicesServer(t *testing.T) (*Server, *store.SQLite, *agent.Store) {
	t.Helper()
	s, f, st := sessionsServer(t, t.TempDir())
	s.SetRunners(f, st)
	agents := agent.NewStore(filepath.Join(t.TempDir(), "agents"))
	for _, c := range []agent.Capability{
		{ID: "qa", Kind: agent.CapPlugin, Plugin: "qa@personal"},
		{ID: "kb", Kind: agent.CapPlugin, Plugin: "kb@personal"},
		{ID: "claude.ai-Gmail", Kind: agent.CapConnector, Connector: "claude.ai Gmail"},
	} {
		if _, err := agents.SaveCapability(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateWindow("w1", "ChartLabs"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWindowMember("ws-app", "w1"); err != nil {
		t.Fatal(err)
	}
	claude := t.TempDir()
	svc := &loadout.Service{St: st, Catalog: agents, ClaudeDir: func() (string, error) { return claude, nil },
		Repos: func() []loadout.Repo { return nil }, Now: time.Now}
	s.SetLoadouts(svc, st)
	return s, st, agents
}

func jsonOf(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return out
}

func capIDsOf(v any) []string {
	out := []string{}
	list, _ := v.([]any)
	for _, x := range list {
		out = append(out, x.(string))
	}
	return out
}

func TestChoiceRoutesAreOffWithoutLoadouts(t *testing.T) {
	s, _, _ := sessionsServer(t, t.TempDir())
	for _, path := range []string{"/v1/capabilities/choices", "/v1/windows/w1/capabilities", "/v1/workspaces/ws-app/capabilities"} {
		if w := callSessions(t, s, "GET", path, ""); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s without loadouts: %d", path, w.Code)
		}
	}
}

// A group has on and off, a repo has as-group, on and off, and the repo's
// own choice wins.
func TestChoiceRoutes_GroupAndRepo(t *testing.T) {
	s, _, _ := choicesServer(t)
	put := func(path, state string, want int) {
		t.Helper()
		if w := callSessions(t, s, "PUT", path, `{"state":"`+state+`"}`); w.Code != want {
			t.Fatalf("PUT %s %s: %d %s, want %d", path, state, w.Code, w.Body, want)
		}
	}
	effective := func() []string {
		t.Helper()
		w := callSessions(t, s, "GET", "/v1/workspaces/ws-app/capabilities", "")
		if w.Code != 200 {
			t.Fatalf("GET: %d %s", w.Code, w.Body)
		}
		return capIDsOf(jsonOf(t, w.Body.String())["effective"])
	}
	if got := effective(); len(got) != 0 {
		t.Fatalf("a new group and repo start with nothing on: %v", got)
	}
	put("/v1/windows/w1/capabilities/qa", "on", http.StatusNoContent)
	put("/v1/windows/w1/capabilities/kb", "on", http.StatusNoContent)
	if got := effective(); !slices.Equal(got, []string{"kb", "qa"}) {
		t.Fatalf("as the group: %v", got)
	}
	put("/v1/workspaces/ws-app/capabilities/kb", "off", http.StatusNoContent)
	put("/v1/workspaces/ws-app/capabilities/claude.ai-Gmail", "on", http.StatusNoContent)
	if got := effective(); !slices.Equal(got, []string{"claude.ai-Gmail", "qa"}) {
		t.Fatalf("the repo's own off and on: %v", got)
	}
	body := jsonOf(t, callSessions(t, s, "GET", "/v1/workspaces/ws-app/capabilities", "").Body.String())
	if body["window"] != "w1" || !slices.Equal(capIDsOf(body["group"]), []string{"kb", "qa"}) ||
		body["choices"].(map[string]any)["kb"] != "off" || body["choices"].(map[string]any)["claude.ai-Gmail"] != "on" {
		t.Fatalf("the repo's view: %v", body)
	}
	put("/v1/workspaces/ws-app/capabilities/kb", "group", http.StatusNoContent)
	put("/v1/windows/w1/capabilities/qa", "off", http.StatusNoContent)
	if got := effective(); !slices.Equal(got, []string{"claude.ai-Gmail", "kb"}) {
		t.Fatalf("back to the group for kb, and the group dropped qa: %v", got)
	}
	win := jsonOf(t, callSessions(t, s, "GET", "/v1/windows/w1/capabilities", "").Body.String())
	if !slices.Equal(capIDsOf(win["on"]), []string{"kb"}) {
		t.Fatalf("the group's list: %v", win)
	}
	all := jsonOf(t, callSessions(t, s, "GET", "/v1/capabilities/choices", "").Body.String())
	if !slices.Equal(capIDsOf(all["windows"].(map[string]any)["w1"]), []string{"kb"}) ||
		all["workspaces"].(map[string]any)["ws-app"].(map[string]any)["claude.ai-Gmail"] != "on" {
		t.Fatalf("every choice in one read: %v", all)
	}
}

func TestChoiceRoutes_Refusals(t *testing.T) {
	s, _, _ := choicesServer(t)
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/v1/windows/nope/capabilities/qa", `{"state":"on"}`, http.StatusNotFound},
		{"/v1/workspaces/nope/capabilities/qa", `{"state":"on"}`, http.StatusNotFound},
		{"/v1/windows/w1/capabilities/ghost", `{"state":"on"}`, http.StatusNotFound},
		{"/v1/workspaces/ws-app/capabilities/ghost", `{"state":"on"}`, http.StatusNotFound},
		{"/v1/windows/w1/capabilities/qa", `{"state":"group"}`, http.StatusBadRequest},
		{"/v1/workspaces/ws-app/capabilities/qa", `{"state":"maybe"}`, http.StatusBadRequest},
		{"/v1/windows/w1/capabilities/qa", `not json`, http.StatusBadRequest},
		// Off and "as the group" clear a choice of something since deleted.
		{"/v1/windows/w1/capabilities/ghost", `{"state":"off"}`, http.StatusNoContent},
		{"/v1/workspaces/ws-app/capabilities/ghost", `{"state":"group"}`, http.StatusNoContent},
	} {
		if w := callSessions(t, s, "PUT", c.path, c.body); w.Code != c.want {
			t.Errorf("PUT %s %s: %d %s, want %d", c.path, c.body, w.Code, w.Body, c.want)
		}
	}
	if w := callSessions(t, s, "GET", "/v1/windows/nope/capabilities", ""); w.Code != http.StatusNotFound {
		t.Errorf("an unknown window: %d", w.Code)
	}
}

// An entry a group has on is named in the catalog's "in use" refusal, and
// one deleted from under a choice is served as missing.
func TestChoices_HoldTheCatalogEntryAndSayWhenItIsGone(t *testing.T) {
	s, st, agents := choicesServer(t)
	agents.ChoiceUsers = st.CapabilityChoiceUsers
	s.agents = agents
	callSessions(t, s, "PUT", "/v1/windows/w1/capabilities/qa", `{"state":"on"}`)
	if w := callSessions(t, s, "DELETE", "/v1/capabilities/qa", ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "group ChartLabs") {
		t.Fatalf("deleting an entry a group has on: %d %s", w.Code, w.Body)
	}
	agents.ChoiceUsers = nil // as if deleted by hand from the catalog file
	if err := agents.RemoveCapability("qa"); err != nil {
		t.Fatal(err)
	}
	body := jsonOf(t, callSessions(t, s, "GET", "/v1/workspaces/ws-app/capabilities", "").Body.String())
	if !slices.Equal(capIDsOf(body["missing"]), []string{"qa"}) || !slices.Equal(capIDsOf(body["effective"]), []string{"qa"}) {
		t.Fatalf("a choice of a deleted entry is still on, and said to be missing: %v", body)
	}
}

// A session is served with what it started with, and says when its list
// has moved on since.
func TestSessionView_LoadoutAndOutOfDate(t *testing.T) {
	s, st, _ := choicesServer(t)
	callSessions(t, s, "PUT", "/v1/windows/w1/capabilities/qa", `{"state":"on"}`)
	if err := st.AddRunnerSession(store.RunnerSession{ID: "s1", Cwd: "/r", Wanted: store.SessionRunning, WorkspaceID: "ws-app", PaneID: "p-s1"}); err != nil {
		t.Fatal(err)
	}
	view := func() map[string]any {
		t.Helper()
		w := callSessions(t, s, "GET", "/v1/sessions", "")
		if w.Code != 200 {
			t.Fatalf("list: %d %s", w.Code, w.Body)
		}
		return jsonOf(t, w.Body.String())["sessions"].([]any)[0].(map[string]any)
	}
	if v := view(); v["loadoutStale"] != true || v["loadout"] != nil {
		t.Fatalf("a running session from before loadouts has everything: out of date, no record: %v", v)
	}
	snap, _ := s.loadouts.Snapshot()
	rec := agent.LoadoutRecord{Signature: agent.LoadoutSignature(mustCatalog(t, s), snap.IDs("ws-app")),
		Entries: []agent.LoadoutEntry{{ID: "qa", Kind: agent.CapPlugin}}, Notes: []string{}, StartedAt: 5}
	blob, _ := json.Marshal(rec)
	if err := st.SetRunnerSessionLoadout("s1", string(blob)); err != nil {
		t.Fatal(err)
	}
	v := view()
	if v["loadoutStale"] != nil || v["loadout"].(map[string]any)["entries"].([]any)[0].(map[string]any)["id"] != "qa" {
		t.Fatalf("a session on its current list: %v", v)
	}
	if strings.Contains(callSessions(t, s, "GET", "/v1/sessions", "").Body.String(), `"Loadout"`) {
		t.Fatal("the row's raw loadout text leaks into the view")
	}
	callSessions(t, s, "PUT", "/v1/workspaces/ws-app/capabilities/kb", `{"state":"on"}`)
	if v := view(); v["loadoutStale"] != true {
		t.Fatalf("a choice changed under a running session: %v", v)
	}
	one := jsonOf(t, callSessions(t, s, "GET", "/v1/sessions/s1", "").Body.String())["session"].(map[string]any)
	if one["loadoutStale"] != true || one["loadout"] == nil {
		t.Fatalf("one session says the same as the list: %v", one)
	}
}

func mustCatalog(t *testing.T, s *Server) []agent.Capability {
	t.Helper()
	list, err := s.loadouts.Catalog.Capabilities()
	if err != nil {
		t.Fatal(err)
	}
	return list
}
