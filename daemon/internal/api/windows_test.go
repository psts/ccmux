package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/hub"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/version"
)

// hubWire puts a Server into hub mode with the given local workspaces in its
// aggregate — the cheapest way to KNOWN workspace ids and no tmux.
func hubWire(s *Server, wss ...*model.Workspace) {
	reg := hub.NewRegistry("hub", hub.DefaultFloor,
		func() ([]hub.Node, error) { return []hub.Node{{ID: "hub", Addr: "hub.invalid"}}, nil },
		func(string) (hub.Health, error) { return hub.Health{Contract: version.Contract}, nil },
		func() int64 { return 1 },
	)
	reg.Refresh()
	agg := hub.NewAggregator("hub", reg, fakeLister{wss: wss}, func(context.Context, hub.Host) ([]*model.Workspace, error) {
		return nil, nil
	})
	agg.Aggregate(context.Background())
	s.hub = &hubMode{reg: reg, agg: agg, selfID: "hub"}
}

// windowsFixture is a hub-mode server over a working store; res decides who
// the caller resolves to.
func windowsFixture(t *testing.T, res whoisResolver, wss ...*model.Workspace) *Server {
	t.Helper()
	s := newIdentityServer(t, res)
	hubWire(s, wss...)
	return s
}

func listStamped(t *testing.T, s *Server) []*model.Workspace {
	t.Helper()
	rec := httptest.NewRecorder()
	s.hubListWorkspaces(rec, httptest.NewRequest("GET", "/v1/workspaces", nil))
	var got []*model.Workspace
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode list: %v (%s)", err, rec.Body)
	}
	return got
}

func putGroupReq(t *testing.T, s *Server, wsID, group string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"group": group})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/v1/workspaces/"+wsID+"/group", strings.NewReader(string(b)))
	req.SetPathValue("id", wsID)
	s.putGroup(rec, req)
	return rec
}

// Windows are SHARED: an assignment by one person is what every caller sees —
// the whole point of v2. The mishmash's root cause (per-person truths that
// disagree) cannot come back through this shape.
func TestWindows_GroupIsSharedAcrossCallers(t *testing.T) {
	ws := &model.Workspace{ID: "w1", Name: "chartlabs"}
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, ws)
	if rec := putGroupReq(t, s, "w1", "CHARTLABS"); rec.Code != http.StatusNoContent {
		t.Fatalf("assign = %d (%s)", rec.Code, rec.Body)
	}

	for _, caller := range []string{"patric@x.com", "dasha@x.com", "carol@x.com"} {
		s.identity = fakeResolver{login: caller, ok: true}
		if got := listStamped(t, s); got[0].Group != "CHARTLABS" {
			t.Fatalf("%s sees group %q, want CHARTLABS for everyone", caller, got[0].Group)
		}
	}
}

// A workspace still carrying its legacy persisted group and no membership gets
// imported into a shared window once, on first read — no owner required, the
// window it creates is shared. Removing it afterwards STICKS (the marker), and
// the peers bus agrees.
func TestWindows_LegacyImportOnceAndRemovalSticks(t *testing.T) {
	ws := &model.Workspace{ID: "w1", Group: "CHARTLABS"}
	s := windowsFixture(t, fakeResolver{login: "dasha@x.com", ok: true}, ws)

	if got := listStamped(t, s); got[0].Group != "CHARTLABS" {
		t.Fatalf("after import: group %q, want CHARTLABS", got[0].Group)
	}
	if _, ok := s.mgr.WindowByName("CHARTLABS"); !ok {
		t.Fatal("import did not create the shared window")
	}

	if rec := putGroupReq(t, s, "w1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove = %d", rec.Code)
	}
	if got := listStamped(t, s); got[0].Group != "" {
		t.Fatalf("legacy group re-imported after removal: %q", got[0].Group)
	}
	if g := s.mgr.SharedGroupResolver()("w1", "CHARTLABS"); g != "" {
		t.Fatalf("bus still groups the removed workspace under %q", g)
	}
}

// Window names are one namespace, case-insensitively: assigning to "chartlabs"
// joins the window named "CHARTLABS" instead of minting a twin.
func TestWindows_AssignMergesNamesCaseInsensitively(t *testing.T) {
	a := &model.Workspace{ID: "w1"}
	b := &model.Workspace{ID: "w2"}
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, a, b)
	if rec := putGroupReq(t, s, "w1", "CHARTLABS"); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	if rec := putGroupReq(t, s, "w2", "chartlabs"); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	if n := len(s.mgr.Windows()); n != 1 {
		t.Fatalf("%d windows, want the two spellings merged into one", n)
	}

	if rec := putGroupReq(t, s, "nope", "x"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown workspace = %d, want 404", rec.Code)
	}
}

// The open flags are the ONLY personal state: /v1/windows stamps `open` per
// caller, and a close by the final opener answers {last:true, members} so the
// lens can put the window to sleep.
func TestWindows_OpenFlagsPerLoginAndLastClose(t *testing.T) {
	ws := &model.Workspace{ID: "w1"}
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, ws)
	if rec := putGroupReq(t, s, "w1", "CHARTLABS"); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	wid, _ := s.mgr.WindowByName("CHARTLABS")

	setOpen := func(login string, open bool) map[string]any {
		s.identity = fakeResolver{login: login, ok: true}
		verb := "close"
		if open {
			verb = "open"
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/windows/"+wid+"/"+verb, nil)
		req.SetPathValue("id", wid)
		s.setWindowOpen(open)(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s = %d (%s)", login, verb, rec.Code, rec.Body)
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	windowsFor := func(login string) []struct {
		ID     string   `json:"id"`
		Open   bool     `json:"open"`
		OpenBy []string `json:"openBy"`
	} {
		s.identity = fakeResolver{login: login, ok: true}
		rec := httptest.NewRecorder()
		s.listWindows(rec, httptest.NewRequest("GET", "/v1/windows", nil))
		var out []struct {
			ID     string   `json:"id"`
			Open   bool     `json:"open"`
			OpenBy []string `json:"openBy"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode windows: %v (%s)", err, rec.Body)
		}
		return out
	}

	setOpen("patric@x.com", true)
	setOpen("dasha@x.com", true)
	if w := windowsFor("patric@x.com"); !w[0].Open || len(w[0].OpenBy) != 2 {
		t.Fatalf("patric's view = %+v, want open by both", w[0])
	}

	// Dasha closes: not last — Patric still has it open, nothing sleeps.
	if out := setOpen("dasha@x.com", false); out["last"] == true {
		t.Fatalf("first close reported last: %v", out)
	}
	if w := windowsFor("dasha@x.com"); w[0].Open || len(w[0].OpenBy) != 1 {
		t.Fatalf("dasha's view after her close = %+v, want closed for her, open for patric", w[0])
	}

	// Patric closes: LAST — the members come back for the lens to archive.
	out := setOpen("patric@x.com", false)
	members, _ := out["members"].([]any)
	if out["last"] != true || len(members) != 1 || members[0] != "w1" {
		t.Fatalf("last close = %v, want last:true members:[w1]", out)
	}
}

// The archive guard keys on OPEN state: stopping a session is refused while
// someone else has its window open (force=1 overrides), allowed once nobody
// does, and refused for a non-owner of the session's host.
func TestWindows_ArchiveGuard(t *testing.T) {
	archive := func(s *Server, url string) (*httptest.ResponseRecorder, *bool) {
		ran := false
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", url, nil)
		req.SetPathValue("id", "w1")
		s.archiveGuard(func(http.ResponseWriter, *http.Request) { ran = true })(rec, req)
		return rec, &ran
	}
	ws := &model.Workspace{ID: "w1"}
	fixture := func(t *testing.T) *Server {
		s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, ws)
		if rec := putGroupReq(t, s, "w1", "CHARTLABS"); rec.Code != http.StatusNoContent {
			t.Fatal(rec.Code)
		}
		return s
	}

	t.Run("open by someone else blocks", func(t *testing.T) {
		s := fixture(t)
		wid, _ := s.mgr.WindowByName("CHARTLABS")
		if _, _, err := s.mgr.SetWindowOpen("dasha@x.com", wid, true); err != nil {
			t.Fatal(err)
		}
		rec, ran := archive(s, "/v1/workspaces/w1/archive")
		if rec.Code != http.StatusConflict || *ran {
			t.Fatalf("archive with dasha's window open = %d ran=%v, want 409 blocked", rec.Code, *ran)
		}
		if !strings.Contains(rec.Body.String(), "dasha@x.com") || !strings.Contains(rec.Body.String(), "CHARTLABS") {
			t.Fatalf("409 should name the person and the window: %s", rec.Body)
		}
		if _, ran := archive(s, "/v1/workspaces/w1/archive?force=1"); !*ran {
			t.Fatal("force=1 must override")
		}
	})

	t.Run("nobody open allows", func(t *testing.T) {
		s := fixture(t)
		if _, ran := archive(s, "/v1/workspaces/w1/archive"); !*ran {
			t.Fatal("archive with the window open by nobody must proceed")
		}
	})

	t.Run("non-owner blocked", func(t *testing.T) {
		s := fixture(t)
		if err := s.mgr.SetOwner("patric@x.com"); err != nil {
			t.Fatal(err)
		}
		s.identity = fakeResolver{login: "dasha@x.com", ok: true}
		rec, ran := archive(s, "/v1/workspaces/w1/archive")
		if rec.Code != http.StatusConflict || *ran {
			t.Fatalf("non-owner archive = %d ran=%v, want 409 blocked", rec.Code, *ran)
		}
	})
}

// The guard's core safety claim survives v2: unreadable window tables answer
// 503, never "not open" — a DB hiccup must not impersonate permission to stop
// a session for everyone. force=1 still overrides, as the message advertises.
func TestWindows_ArchiveGuardFailsClosedOnUnreadableState(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "guard.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(manager.New(context.Background(), nil, st))
	s.identity = fakeResolver{login: "patric@x.com", ok: true}
	hubWire(s, &model.Workspace{ID: "w1"})
	_ = st.Close() // the guard's evidence is now unreadable

	ran := false
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/workspaces/w1/archive", nil)
	req.SetPathValue("id", "w1")
	s.archiveGuard(func(http.ResponseWriter, *http.Request) { ran = true })(rec, req)
	if rec.Code != http.StatusServiceUnavailable || ran {
		t.Fatalf("guard with unreadable state = %d ran=%v, want 503 blocked", rec.Code, ran)
	}
}

// The hub's cross-host create: the proxied response is teed, the new id
// parsed, and the shared membership seeded WITH an import marker — every
// failure here is silent by design (log-only), so this test is the alarm.
func TestWindows_HostCreateSeedsMembership(t *testing.T) {
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"wr-new","name":"repo"}`))
	}))
	defer remote.Close()
	remoteAddr := strings.TrimPrefix(remote.URL, "https://")

	s := newIdentityServer(t, fakeResolver{login: "dasha@x.com", ok: true})
	reg := hub.NewRegistry("hub", hub.DefaultFloor,
		func() ([]hub.Node, error) {
			return []hub.Node{{ID: "hub", Addr: "hub.invalid"}, {ID: "remote", Addr: remoteAddr}}, nil
		},
		func(string) (hub.Health, error) { return hub.Health{Contract: version.Contract}, nil },
		func() int64 { return 1 },
	)
	reg.Refresh()
	agg := hub.NewAggregator("hub", reg, fakeLister{}, func(context.Context, hub.Host) ([]*model.Workspace, error) {
		return nil, nil
	})
	agg.Aggregate(context.Background())
	s.hub = &hubMode{reg: reg, agg: agg, client: hub.NewClient(remote.Client().Transport), selfID: "hub"}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/hosts/remote/workspaces", strings.NewReader(`{"repoPath":"/r","group":"WIN"}`))
	req.SetPathValue("host", "remote")
	s.hostCreateRoute()(rec, req)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), "wr-new") {
		t.Fatalf("proxied create = %d (%s)", rec.Code, rec.Body)
	}
	if g := s.mgr.SharedGroupResolver()("wr-new", ""); g != "WIN" {
		t.Fatalf("membership = %q, want WIN", g)
	}
	if !s.mgr.ViewImports()["wr-new"] {
		t.Fatal("create must mark the import: the member's legacy column must never resurrect this workspace")
	}
}

// Renames are shared and collision-checked case-insensitively.
func TestWindows_RenameSharedAndCollision(t *testing.T) {
	a := &model.Workspace{ID: "w1"}
	b := &model.Workspace{ID: "w2"}
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, a, b)
	_ = putGroupReq(t, s, "w1", "ALPHA")
	_ = putGroupReq(t, s, "w2", "BETA")
	alphaID, _ := s.mgr.WindowByName("ALPHA")

	rename := func(id, name string) *httptest.ResponseRecorder {
		bts, _ := json.Marshal(map[string]string{"name": name})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/v1/windows/"+id, strings.NewReader(string(bts)))
		req.SetPathValue("id", id)
		s.renameWindow(rec, req)
		return rec
	}
	if rec := rename(alphaID, "GAMMA"); rec.Code != http.StatusNoContent {
		t.Fatalf("rename = %d (%s)", rec.Code, rec.Body)
	}
	if got := listStamped(t, s); got[0].Group != "GAMMA" && got[1].Group != "GAMMA" {
		t.Fatalf("rename did not reach the stamped groups: %q/%q", got[0].Group, got[1].Group)
	}
	if rec := rename(alphaID, "beta"); rec.Code != http.StatusConflict {
		t.Fatalf("colliding rename = %d, want 409", rec.Code)
	}
}
