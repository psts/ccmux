package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/hub"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/version"
)

type fakeLister struct{ wss []*model.Workspace }

func (f fakeLister) List() []*model.Workspace { return f.wss }

// hubFixture wires a hub-mode server whose one remote member is a TLS httptest
// server (so the reverse proxy exercises real TLS-over-transport). remoteContract
// sets the member's health so we can test compat gating.
func hubFixture(t *testing.T, remoteContract int) (*hubMode, *httptest.Server) {
	t.Helper()
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("REMOTE " + r.Method + " " + r.URL.Path))
	}))
	t.Cleanup(remote.Close)
	remoteAddr := strings.TrimPrefix(remote.URL, "https://")

	reg := hub.NewRegistry("hub", hub.DefaultFloor,
		func() ([]hub.Node, error) {
			return []hub.Node{{ID: "hub", Addr: "hub.invalid"}, {ID: "remote", Addr: remoteAddr}}, nil
		},
		func(baseURL string) (hub.Health, error) {
			if strings.Contains(baseURL, remoteAddr) {
				return hub.Health{Contract: remoteContract}, nil
			}
			return hub.Health{Contract: version.Contract}, nil
		},
		func() int64 { return 1 },
	)
	reg.Refresh()

	local := fakeLister{wss: []*model.Workspace{{ID: "wl"}}}
	agg := hub.NewAggregator("hub", reg, local, func(_ context.Context, h hub.Host) ([]*model.Workspace, error) {
		return []*model.Workspace{{ID: "wr"}}, nil
	})
	agg.Aggregate(context.Background()) // seed the owner index

	client := hub.NewClient(remote.Client().Transport)
	return &hubMode{reg: reg, agg: agg, client: client, selfID: "hub"}, remote
}

// TestHub_HostnameConflict: the global registrar rejects a label already claimed
// by a DIFFERENT workspace anywhere, allows a re-claim by the same workspace, and
// allows a free label.
func TestHub_HostnameConflict(t *testing.T) {
	reg := hub.NewRegistry("hub", hub.DefaultFloor,
		func() ([]hub.Node, error) { return []hub.Node{{ID: "hub", Addr: "hub.ts.net"}}, nil },
		func(string) (hub.Health, error) { return hub.Health{Contract: version.Contract}, nil },
		func() int64 { return 1 },
	)
	reg.Refresh()
	local := fakeLister{wss: []*model.Workspace{{ID: "wl", Hostnames: []model.Hostname{{Name: "app", Port: 3000}}}}}
	agg := hub.NewAggregator("hub", reg, local, func(context.Context, hub.Host) ([]*model.Workspace, error) { return nil, nil })
	agg.Aggregate(context.Background())
	h := &hubMode{reg: reg, agg: agg, selfID: "hub"}

	if msg := h.hostnameConflict("wr", []byte(`{"hostnames":[{"name":"app","port":3001}]}`)); msg == "" {
		t.Error("expected a conflict: 'app' is claimed by another workspace")
	}
	if msg := h.hostnameConflict("wl", []byte(`{"hostnames":[{"name":"app","port":3000}]}`)); msg != "" {
		t.Errorf("same-workspace reclaim should be allowed, got %q", msg)
	}
	if msg := h.hostnameConflict("wr", []byte(`{"hostnames":[{"name":"free"}]}`)); msg != "" {
		t.Errorf("a free label should be allowed, got %q", msg)
	}
}

func TestHub_ListHostsAndWorkspaces(t *testing.T) {
	h, _ := hubFixture(t, version.Contract)

	rec := httptest.NewRecorder()
	h.listWorkspaces(rec, httptest.NewRequest("GET", "/v1/workspaces", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `"wl"`) || !strings.Contains(body, `"wr"`) {
		t.Fatalf("aggregate missing local or remote workspace: %s", body)
	}
	if !strings.Contains(body, `"host":"remote"`) {
		t.Errorf("remote workspace not host-stamped: %s", body)
	}

	rec = httptest.NewRecorder()
	h.listHosts(rec, httptest.NewRequest("GET", "/v1/hosts", nil))
	if hb := rec.Body.String(); !strings.Contains(hb, `"remote"`) || !strings.Contains(hb, `"hub"`) {
		t.Fatalf("hosts list = %s", hb)
	}
}

func TestHub_OwnerRoute_LocalVsProxy(t *testing.T) {
	h, _ := hubFixture(t, version.Contract)
	wrapped := h.ownerRoute(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("LOCAL"))
	})

	// Self-owned id → local handler.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/workspaces/wl/archive", nil)
	req.SetPathValue("id", "wl")
	wrapped(rec, req)
	if rec.Body.String() != "LOCAL" {
		t.Fatalf("self route = %q, want LOCAL", rec.Body.String())
	}

	// Remote-owned id → proxied to the member host.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/workspaces/wr/archive", nil)
	req.SetPathValue("id", "wr")
	wrapped(rec, req)
	if got := rec.Body.String(); !strings.HasPrefix(got, "REMOTE POST /v1/workspaces/wr/archive") {
		t.Fatalf("remote route = %q, want proxied to member", got)
	}
}

func TestHub_Gating_DegradedRefusesMutation(t *testing.T) {
	h, _ := hubFixture(t, version.Contract-1) // remote is one contract behind → degraded
	wrapped := h.ownerRoute(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("LOCAL"))
	})

	// Non-GET to a degraded host → 409, never proxied.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/workspaces/wr/archive", nil)
	req.SetPathValue("id", "wr")
	wrapped(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("degraded mutation = %d, want 409; body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "REMOTE") {
		t.Error("degraded mutation must not reach the member host")
	}

	// GET to a degraded host is allowed (list-only + attach reads).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/panes/wr/snapshot", nil)
	req.SetPathValue("id", "wr")
	wrapped(rec, req)
	if !strings.HasPrefix(rec.Body.String(), "REMOTE GET") {
		t.Fatalf("degraded GET = %q, want proxied", rec.Body.String())
	}
}
