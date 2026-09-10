package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// The reviewer's migration repro: a workspace whose membership exists (e.g.
// written by the v1→v2 migration) but whose import MARKER is missing must NOT
// have its create-time legacy group imported over the arrangement — the
// membership guard catches what v1's missing markers left behind.
func TestWindows_LegacyImportNeverOverwritesExistingMembership(t *testing.T) {
	ws := &model.Workspace{ID: "w1", Group: "OLD"}
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, ws)
	// Membership without a marker — what a v1 daemon that never wrote markers
	// migrates into.
	if err := s.mgr.AssignWorkspace("w1", "NEW"); err != nil {
		t.Fatal(err)
	}
	if got := listStamped(t, s); got[0].Group != "NEW" {
		t.Fatalf("stamped group = %q; the legacy OLD must not overwrite the arrangement", got[0].Group)
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
		if _, _, err := s.mgr.SetWindowOpen(store.WindowOpenFlag{Login: "dasha@x.com", WindowID: wid}, true); err != nil {
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

// The repair path: a lens declares its whole set, and the daemon makes that
// DEVICE's rows match — including dropping one it no longer has. Before this a
// flag could only ever be added, so a close that never landed (quit, crash,
// unreachable daemon) blocked the archive-on-last-close rule forever.
func TestOpenSetClearsOnlyItsOwnDeviceRows(t *testing.T) {
	ws := &model.Workspace{ID: "w1"}
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, ws)
	if rec := putGroupReq(t, s, "w1", "ALPHA"); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	wid, _ := s.mgr.WindowByName("ALPHA")

	post := func(device string, ids string) *httptest.ResponseRecorder {
		body := `{"device":"` + device + `","deviceLabel":"` + device + `","windowIds":[` + ids + `]}`
		req := httptest.NewRequest("POST", "/v1/windows/open-set", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.syncWindowOpen(rec, req)
		return rec
	}
	// Two lenses under ONE login, both holding the window.
	for _, dev := range []string{"mac-1", "web-1"} {
		if rec := post(dev, `"`+wid+`"`); rec.Code != http.StatusOK {
			t.Fatalf("open-set %s = %d: %s", dev, rec.Code, rec.Body.String())
		}
	}
	// The Mac stops reporting it. The web lens still shows it, so it must stay
	// open — this is the case a login-keyed flag could not express at all.
	if rec := post("mac-1", ""); rec.Code != http.StatusOK {
		t.Fatalf("clearing set = %d: %s", rec.Code, rec.Body.String())
	}
	logins, grouped := s.mgr.WindowOpenLogins("w1")
	if !grouped || len(logins) == 0 {
		t.Fatal("window closed entirely — one lens's set cleared another lens's row")
	}
	// And when the last lens drops it, it really is closed.
	if rec := post("web-1", ""); rec.Code != http.StatusOK {
		t.Fatalf("second clear = %d", rec.Code)
	}
	if logins, _ := s.mgr.WindowOpenLogins("w1"); len(logins) != 0 {
		t.Fatalf("still open by %v after every lens dropped it", logins)
	}
}

// A device is required: without one there is nothing to scope the clear to,
// and clearing by login would close windows this lens knows nothing about.
func TestOpenSetRefusesWithoutADevice(t *testing.T) {
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true})
	req := httptest.NewRequest("POST", "/v1/windows/open-set", strings.NewReader(`{"windowIds":[]}`))
	rec := httptest.NewRecorder()
	s.syncWindowOpen(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// openHere is per-DEVICE where open is per-login. Without it a window another
// of your own lenses held read open=true everywhere, so the lens that did NOT
// have it showed nothing and its Open Window menu — which lists closed windows
// — excluded it too. Invisible and unopenable, with no exit from the UI.
func TestOpenHereIsPerDeviceNotPerLogin(t *testing.T) {
	ws := &model.Workspace{ID: "w1"}
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, ws)
	if rec := putGroupReq(t, s, "w1", "ALPHA"); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	wid, _ := s.mgr.WindowByName("ALPHA")
	// The phone opens it. Same login, different device.
	if _, _, err := s.mgr.SetWindowOpen(store.WindowOpenFlag{
		Login: "patric@x.com", WindowID: wid, Device: "phone-1", Seen: time.Now().UnixMilli(),
	}, true); err != nil {
		t.Fatal(err)
	}
	list := func(device string) []map[string]any {
		req := httptest.NewRequest("GET", "/v1/windows?device="+device, nil)
		rec := httptest.NewRecorder()
		s.listWindows(rec, req)
		var out []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
		return out
	}
	for _, w := range list("mac-1") {
		if w["id"] != wid {
			continue
		}
		if w["open"] != true {
			t.Errorf("open = %v, want true: the login does have it open", w["open"])
		}
		if w["openHere"] != false {
			t.Errorf("openHere = %v on the Mac, want false: the PHONE holds it", w["openHere"])
		}
	}
	for _, w := range list("phone-1") {
		if w["id"] == wid && w["openHere"] != true {
			t.Errorf("openHere = %v on the phone, want true", w["openHere"])
		}
	}
}

// A row nobody refreshes stops counting, so a machine that never comes back
// cannot hold a window open forever — which blocked archive-on-last-close and
// the window's own pruning.
func TestStaleFlagStopsCountingAsOpen(t *testing.T) {
	ws := &model.Workspace{ID: "w1"}
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, ws)
	if rec := putGroupReq(t, s, "w1", "ALPHA"); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	wid, _ := s.mgr.WindowByName("ALPHA")
	// A laptop that was switched off two months ago.
	old := time.Now().Add(-60 * 24 * time.Hour).UnixMilli()
	if _, _, err := s.mgr.SetWindowOpen(store.WindowOpenFlag{
		Login: "dasha@x.com", WindowID: wid, Device: "dasha-laptop", Label: "dasha-mbp", Seen: old,
	}, true); err != nil {
		t.Fatal(err)
	}
	if logins, _ := s.mgr.WindowOpenLogins("w1"); len(logins) != 0 {
		t.Fatalf("still open by %v — a machine that never came back holds it forever", logins)
	}
}

// brokenDeviceOpens fails ONLY the per-device read, so the sibling window list
// still succeeds. A closed store cannot express this: it fails the list read
// first and returns 503 for that reason instead, which is how the earlier
// version of this test passed without the fix.
type brokenDeviceOpens struct{ store.Store }

func (brokenDeviceOpens) DeviceWindowOpens(string, string, int64) (map[string]bool, error) {
	return nil, errors.New("database is locked")
}

// An unreadable device-open table must not be served as openHere=false. Both
// lenses list a window as CLOSED on that, and clicking it opens a SECOND window
// onto a shared one this lens already has — the duplicate-owner state
// claimHostedWorkspace exists to undo.
func TestOpenHereReadFailureIsA503(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "openhere.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := NewServer(manager.New(context.Background(), nil, brokenDeviceOpens{Store: st}))
	s.identity = fakeResolver{login: "patric@x.com", ok: true}
	hubWire(s, &model.Workspace{ID: "w1"})

	req := httptest.NewRequest("GET", "/v1/windows?device=mac-1", nil)
	rec := httptest.NewRecorder()
	s.listWindows(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: a false openHere is worse than no answer", rec.Code)
	}
}
