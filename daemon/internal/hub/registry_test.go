package hub

import (
	"errors"
	"testing"

	"ccmux.dev/ccmuxd/internal/version"
)

func TestClassify(t *testing.T) {
	hub := 5
	cases := []struct {
		name       string
		host       int
		floor      int
		wantCompat string
	}{
		{"same", 5, 1, CompatOK},
		{"one-behind", 4, 1, CompatDegraded},
		{"one-ahead", 6, 1, CompatDegraded},
		{"two-behind-over-floor", 3, 1, CompatUnsupported},
		{"two-behind-within-wider-floor", 3, 2, CompatDegraded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := classify(c.host, hub, c.floor)
			if got != c.wantCompat {
				t.Fatalf("classify(%d,%d,%d) = %q, want %q", c.host, hub, c.floor, got, c.wantCompat)
			}
			if got != CompatOK && reason == "" {
				t.Error("non-ok compat should carry a reason")
			}
		})
	}
}

// TestRefresh_BuildsSnapshot drives the registry with fake discovery + probes:
// one healthy same-contract host (self), one unreachable, one on an older
// contract → degraded.
func TestRefresh_BuildsSnapshot(t *testing.T) {
	nodes := []Node{
		{ID: "hubnode", Addr: "hubnode.ts.net"},
		{ID: "dead", Addr: "dead.ts.net"},
		{ID: "old", Addr: "old.ts.net"},
	}
	probe := func(baseURL string) (Health, error) {
		switch baseURL {
		case "https://hubnode.ts.net":
			return Health{Version: "v1", Contract: version.Contract}, nil
		case "https://old.ts.net":
			return Health{Version: "v0", Contract: version.Contract - 1}, nil
		default:
			return Health{}, errors.New("dial timeout")
		}
	}
	r := NewRegistry("hubnode", 0, // floor 0 → DefaultFloor (1)
		func() ([]Node, error) { return nodes, nil },
		probe,
		func() int64 { return 1000 },
	)
	r.Refresh()

	self, ok := r.Get("hubnode")
	if !ok || !self.Self || !self.Healthy || self.Compat != CompatOK || !self.Serves() {
		t.Fatalf("self host = %+v", self)
	}
	if self.LastSeen != 1000 {
		t.Errorf("lastSeen = %d, want 1000", self.LastSeen)
	}
	dead, _ := r.Get("dead")
	if dead.Healthy || dead.Compat != CompatUnreachable || dead.Reason == "" {
		t.Fatalf("dead host = %+v, want unreachable with reason", dead)
	}
	if dead.Serves() {
		t.Error("unreachable host must not Serves()")
	}
	old, _ := r.Get("old")
	if !old.Healthy || old.Compat != CompatDegraded || old.Serves() {
		t.Fatalf("old host = %+v, want healthy+degraded and !Serves", old)
	}

	// List: self first, then alphabetical.
	list := r.List()
	if len(list) != 3 || !list[0].Self || list[1].ID != "dead" || list[2].ID != "old" {
		t.Fatalf("List order = %+v", list)
	}
}

// TestRefresh_DiscoveryErrorKeepsSnapshot: a flaky control-plane read must not
// blank the federation.
func TestRefresh_DiscoveryErrorKeepsSnapshot(t *testing.T) {
	fail := false
	r := NewRegistry("a", 1,
		func() ([]Node, error) {
			if fail {
				return nil, errors.New("control plane down")
			}
			return []Node{{ID: "a", Addr: "a.ts.net"}}, nil
		},
		func(string) (Health, error) { return Health{Contract: version.Contract}, nil },
		func() int64 { return 1 },
	)
	r.Refresh()
	if len(r.List()) != 1 {
		t.Fatalf("after good refresh: %d hosts", len(r.List()))
	}
	fail = true
	r.Refresh()
	if len(r.List()) != 1 {
		t.Fatalf("discovery error blanked the snapshot: %d hosts", len(r.List()))
	}
}
