package hub

import (
	"context"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/version"
)

type fakeLocal struct{ wss []*model.Workspace }

func (f fakeLocal) List() []*model.Workspace { return f.wss }

// twoHostRegistry returns a registry with a healthy self ("hub") and one healthy
// remote ("remote"), both on the hub's contract.
func twoHostRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry("hub", 1,
		func() ([]Node, error) {
			return []Node{{ID: "hub", Addr: "hub.ts.net"}, {ID: "remote", Addr: "remote.ts.net"}}, nil
		},
		func(string) (Health, error) { return Health{Contract: version.Contract}, nil },
		func() int64 { return 1 },
	)
	r.Refresh()
	return r
}

func TestAggregate_MergesStampsIndexes(t *testing.T) {
	localWS := &model.Workspace{ID: "wl", Panes: []*model.Pane{{ID: "pl"}}}
	remoteWS := &model.Workspace{ID: "wr", Panes: []*model.Pane{{ID: "pr"}}}

	agg := NewAggregator("hub", twoHostRegistry(t),
		fakeLocal{wss: []*model.Workspace{localWS}},
		func(_ context.Context, h Host) ([]*model.Workspace, error) {
			if h.ID != "remote" {
				t.Errorf("unexpected fetch for host %q", h.ID)
			}
			return []*model.Workspace{remoteWS}, nil
		},
	)

	out := agg.Aggregate(context.Background())
	if len(out) != 2 {
		t.Fatalf("merged = %d workspaces, want 2", len(out))
	}
	byID := map[string]*model.Workspace{}
	for _, ws := range out {
		byID[ws.ID] = ws
	}
	if byID["wl"].Host != "hub" || byID["wr"].Host != "remote" {
		t.Fatalf("host stamps: wl=%q wr=%q", byID["wl"].Host, byID["wr"].Host)
	}
	if byID["wl"].Panes[0].Host != "hub" || byID["wr"].Panes[0].Host != "remote" {
		t.Error("panes not host-stamped")
	}

	// Stamping must not mutate the manager's live objects.
	if localWS.Host != "" || localWS.Panes[0].Host != "" {
		t.Errorf("stampHost mutated shared local workspace: %+v", localWS)
	}

	// Ownership index covers workspaces and panes.
	for id, want := range map[string]string{"wl": "hub", "pl": "hub", "wr": "remote", "pr": "remote"} {
		if got, ok := agg.Owner(id); !ok || got != want {
			t.Errorf("Owner(%q) = %q,%v; want %q", id, got, ok, want)
		}
	}
	if _, ok := agg.Owner("nope"); ok {
		t.Error("unknown id should not resolve")
	}
}

// TestAggregate_GroupForPane proves the peers group resolver: panes on DIFFERENT
// hosts that share a window group both resolve to it — the cross-host-window case.
func TestAggregate_GroupForPane(t *testing.T) {
	localWS := &model.Workspace{ID: "wl", Group: "CHARTLABS", Panes: []*model.Pane{{ID: "pl"}}}
	remoteWS := &model.Workspace{ID: "wr", Group: "CHARTLABS", Panes: []*model.Pane{{ID: "pr"}}}
	agg := NewAggregator("hub", twoHostRegistry(t),
		fakeLocal{wss: []*model.Workspace{localWS}},
		func(_ context.Context, h Host) ([]*model.Workspace, error) {
			return []*model.Workspace{remoteWS}, nil
		})
	agg.Aggregate(context.Background())

	for pane, want := range map[string]string{"pl": "CHARTLABS", "pr": "CHARTLABS"} {
		if g, ok := agg.GroupForPane(pane); !ok || g != want {
			t.Errorf("GroupForPane(%q) = %q,%v; want %q", pane, g, ok, want)
		}
	}
	if _, ok := agg.GroupForPane("nope"); ok {
		t.Error("unknown pane should not resolve a group")
	}
}

// TestAggregate_DegradedHostStillLists: a degraded host's workspaces still appear
// (list-only), an unsupported host's do not.
func TestAggregate_GatingByCompat(t *testing.T) {
	r := NewRegistry("hub", 1,
		func() ([]Node, error) {
			return []Node{
				{ID: "hub", Addr: "hub.ts.net"},
				{ID: "old", Addr: "old.ts.net"},
				{ID: "ancient", Addr: "ancient.ts.net"},
			}, nil
		},
		func(baseURL string) (Health, error) {
			switch baseURL {
			case "https://old.ts.net":
				return Health{Contract: version.Contract - 1}, nil // degraded
			case "https://ancient.ts.net":
				return Health{Contract: version.Contract - 5}, nil // unsupported
			default:
				return Health{Contract: version.Contract}, nil
			}
		},
		func() int64 { return 1 },
	)
	r.Refresh()

	fetched := map[string]bool{}
	agg := NewAggregator("hub", r, fakeLocal{},
		func(_ context.Context, h Host) ([]*model.Workspace, error) {
			fetched[h.ID] = true
			return []*model.Workspace{{ID: "w-" + h.ID}}, nil
		},
	)
	out := agg.Aggregate(context.Background())

	if !fetched["old"] {
		t.Error("degraded host should still be listed (fetched)")
	}
	if fetched["ancient"] {
		t.Error("unsupported host must not be aggregated")
	}
	if len(out) != 1 || out[0].ID != "w-old" {
		t.Fatalf("aggregate = %+v, want only the degraded host's workspace", out)
	}
}
