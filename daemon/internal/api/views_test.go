package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/hub"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/version"
)

// viewsFixture is a hub-mode server whose aggregate holds the given local
// workspaces — the cheapest way to a Server with KNOWN workspace ids and no
// tmux. res decides who the caller resolves to.
func viewsFixture(t *testing.T, res whoisResolver, wss ...*model.Workspace) *Server {
	t.Helper()
	s := newIdentityServer(t, res)
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
	return s
}

func listAs(t *testing.T, s *Server) []*model.Workspace {
	t.Helper()
	rec := httptest.NewRecorder()
	s.hubListWorkspaces(rec, httptest.NewRequest("GET", "/v1/workspaces", nil))
	var got []*model.Workspace
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode list: %v (%s)", err, rec.Body)
	}
	return got
}

func putGroupAs(t *testing.T, s *Server, wsID, group string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/v1/workspaces/"+wsID+"/group", strings.NewReader(`{"group":`+jsonStr(group)+`}`))
	req.SetPathValue("id", wsID)
	s.putGroup(rec, req)
	return rec
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Two people, one workspace, two different windows: each caller sees their OWN
// row in group, and nobody sees the other's. The original bug — one shared
// string all Macs fight over — cannot come back through this shape.
func TestViews_GroupIsPerCaller(t *testing.T) {
	ws := &model.Workspace{ID: "w1", Name: "chartlabs"}
	s := viewsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, ws)
	if err := s.mgr.SetView("patric@x.com", "w1", "CHARTLABS"); err != nil {
		t.Fatal(err)
	}
	if err := s.mgr.SetView("dasha@x.com", "w1", "dasha"); err != nil {
		t.Fatal(err)
	}

	got := listAs(t, s)
	if len(got) != 1 || got[0].Group != "CHARTLABS" {
		t.Fatalf("patric sees group %q, want CHARTLABS", got[0].Group)
	}

	s.identity = fakeResolver{login: "dasha@x.com", ok: true}
	if got = listAs(t, s); got[0].Group != "dasha" {
		t.Fatalf("dasha sees group %q, want dasha", got[0].Group)
	}

	s.identity = fakeResolver{login: "carol@x.com", ok: true}
	if got = listAs(t, s); got[0].Group != "" {
		t.Fatalf("a third person sees group %q, want none (Available)", got[0].Group)
	}
}

// Owner and OwnerGroup label an Available session ("Patric · CHARTLABS"): the
// owner comes from the owning host (self → this daemon's owner setting), the
// ownerGroup from the owner's own row.
func TestViews_OwnerLabels(t *testing.T) {
	ws := &model.Workspace{ID: "w1"}
	s := viewsFixture(t, fakeResolver{login: "dasha@x.com", ok: true}, ws)
	if err := s.mgr.SetOwner("patric@x.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.mgr.SetView("patric@x.com", "w1", "CHARTLABS"); err != nil {
		t.Fatal(err)
	}

	got := listAs(t, s)
	if got[0].Owner != "patric@x.com" || got[0].OwnerGroup != "CHARTLABS" {
		t.Fatalf("owner label = %q · %q, want patric@x.com · CHARTLABS", got[0].Owner, got[0].OwnerGroup)
	}
	if got[0].Group != "" {
		t.Fatalf("dasha's own group = %q, want empty — the owner's window is a label, not her view", got[0].Group)
	}
}

// A workspace still carrying its legacy persisted group and no view rows gets
// that arrangement imported as the host owner's row — once, on first read.
func TestViews_LegacyGroupImportsToOwner(t *testing.T) {
	ws := &model.Workspace{ID: "w1", Group: "CHARTLABS"}
	s := viewsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, ws)
	if err := s.mgr.SetOwner("patric@x.com"); err != nil {
		t.Fatal(err)
	}

	if got := listAs(t, s); got[0].Group != "CHARTLABS" || got[0].OwnerGroup != "CHARTLABS" {
		t.Fatalf("after import: group %q ownerGroup %q, want CHARTLABS both", got[0].Group, got[0].OwnerGroup)
	}
	if rows := s.mgr.Views()["w1"]; rows["patric@x.com"] != "CHARTLABS" {
		t.Fatalf("import did not persist a row: %v", rows)
	}

	// The import must not resurrect an arrangement the owner then changed.
	if err := s.mgr.SetView("patric@x.com", "w1", ""); err != nil {
		t.Fatal(err)
	}
	if got := listAs(t, s); got[0].Group != "" {
		t.Fatalf("legacy group re-imported after the owner put it away: %q", got[0].Group)
	}
}

// No owner configured → nothing to import onto, and that must be safe: the
// session simply shows as Available (no group), not lost.
func TestViews_LegacyGroupWithoutOwnerStaysAvailable(t *testing.T) {
	ws := &model.Workspace{ID: "w1", Group: "CHARTLABS"}
	s := viewsFixture(t, fakeResolver{login: "dasha@x.com", ok: true}, ws)
	if got := listAs(t, s); got[0].Group != "" || got[0].Owner != "" {
		t.Fatalf("unowned legacy ws = group %q owner %q, want Available", got[0].Group, got[0].Owner)
	}
	if len(s.mgr.Views()["w1"]) != 0 {
		t.Fatal("import ran without an owner to attribute to")
	}
}

func TestViews_PutGroupWritesCallerRowOnly(t *testing.T) {
	ws := &model.Workspace{ID: "w1"}
	s := viewsFixture(t, fakeResolver{login: "dasha@x.com", ok: true}, ws)
	if err := s.mgr.SetView("patric@x.com", "w1", "CHARTLABS"); err != nil {
		t.Fatal(err)
	}

	if rec := putGroupAs(t, s, "w1", "dasha"); rec.Code != http.StatusNoContent {
		t.Fatalf("put group = %d (%s)", rec.Code, rec.Body)
	}
	rows := s.mgr.Views()["w1"]
	if rows["dasha@x.com"] != "dasha" || rows["patric@x.com"] != "CHARTLABS" {
		t.Fatalf("rows = %v; dasha's write must not touch patric's row", rows)
	}

	// Empty group = put it away: deletes the caller's row, nobody else's.
	if rec := putGroupAs(t, s, "w1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("clear group = %d", rec.Code)
	}
	rows = s.mgr.Views()["w1"]
	if _, still := rows["dasha@x.com"]; still || rows["patric@x.com"] != "CHARTLABS" {
		t.Fatalf("rows after clear = %v", rows)
	}

	if rec := putGroupAs(t, s, "nope", "x"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown workspace = %d, want 404", rec.Code)
	}
}

// The archive guard: stopping a session is global, so it is refused when the
// caller is not the owner or someone else still keeps the workspace in a
// window — force=1 overrides ("Archive anyway"). An unowned host keeps
// today's single-user behavior: no owner to check, archive proceeds.
func TestViews_ArchiveGuard(t *testing.T) {
	archive := func(s *Server, url string) (*httptest.ResponseRecorder, *bool) {
		ran := false
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", url, nil)
		req.SetPathValue("id", "w1")
		s.archiveGuard(func(http.ResponseWriter, *http.Request) { ran = true })(rec, req)
		return rec, &ran
	}
	ws := &model.Workspace{ID: "w1"}

	t.Run("not the owner", func(t *testing.T) {
		s := viewsFixture(t, fakeResolver{login: "dasha@x.com", ok: true}, ws)
		if err := s.mgr.SetOwner("patric@x.com"); err != nil {
			t.Fatal(err)
		}
		rec, ran := archive(s, "/v1/workspaces/w1/archive")
		if rec.Code != http.StatusConflict || *ran {
			t.Fatalf("non-owner archive = %d ran=%v, want 409 blocked", rec.Code, *ran)
		}
		rec, ran = archive(s, "/v1/workspaces/w1/archive?force=1")
		if !*ran {
			t.Fatalf("force=1 must override (got %d)", rec.Code)
		}
	})

	t.Run("someone else holds a window", func(t *testing.T) {
		s := viewsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, ws)
		if err := s.mgr.SetOwner("patric@x.com"); err != nil {
			t.Fatal(err)
		}
		if err := s.mgr.SetView("dasha@x.com", "w1", "dasha"); err != nil {
			t.Fatal(err)
		}
		rec, ran := archive(s, "/v1/workspaces/w1/archive")
		if rec.Code != http.StatusConflict || *ran {
			t.Fatalf("archive with another holder = %d ran=%v, want 409 blocked", rec.Code, *ran)
		}
	})

	t.Run("owner alone archives as today", func(t *testing.T) {
		s := viewsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, ws)
		if err := s.mgr.SetOwner("patric@x.com"); err != nil {
			t.Fatal(err)
		}
		if err := s.mgr.SetView("patric@x.com", "w1", "CHARTLABS"); err != nil {
			t.Fatal(err)
		}
		if _, ran := archive(s, "/v1/workspaces/w1/archive"); !*ran {
			t.Fatal("owner with no other holders must archive")
		}
	})

	t.Run("unowned host stays permissive", func(t *testing.T) {
		s := viewsFixture(t, fakeResolver{ok: false}, ws)
		if _, ran := archive(s, "/v1/workspaces/w1/archive"); !*ran {
			t.Fatal("no owner configured → archive must proceed (single-user behavior)")
		}
	})
}
