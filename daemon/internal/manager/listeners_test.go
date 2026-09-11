package manager

import (
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/listeners"
	"ccmux.dev/ccmuxd/internal/model"
)

// TestResolveRoutes pins where a name routes against what is listening.
func TestResolveRoutes(t *testing.T) {
	ws := func(name string, hs []model.Hostname, ports ...int) *model.Workspace {
		w := &model.Workspace{ID: name + "-id", Name: name, Hostnames: hs}
		for _, p := range ports {
			w.Listeners = append(w.Listeners, model.Listener{Port: p, Process: "node", PaneID: "p"})
		}
		return w
	}
	cases := []struct {
		name  string
		ws    *model.Workspace
		owner map[int]portOwner
		want  []model.Hostname
	}{
		{
			// The website case: the script pins -p 3003 and the pane holds it.
			"held port routes as configured",
			ws("website", []model.Hostname{{Name: "chartlabs", Port: 3003}}, 3003),
			map[int]portOwner{3003: {"website-id", "website"}},
			[]model.Hostname{{Name: "chartlabs", Port: 3003}},
		},
		{
			// vite: "5173 in use, using 5174" — one unmatched row, one spare
			// listener, they pair up.
			"lone spare listener follows",
			ws("kullio", []model.Hostname{{Name: "kullio", Port: 5173}}, 5174),
			map[int]portOwner{5174: {"kullio-id", "kullio"}},
			[]model.Hostname{{Name: "kullio", Port: 5173, LivePort: 5174}},
		},
		{
			// The monorepo case: three rows, three listeners, all exact. A
			// fourth listener (a debugger port) must not steal anything.
			"multi-app exact matches, extra listener ignored",
			ws("kullio", []model.Hostname{{Name: "kullio", Port: 5173}, {Name: "kullio-admin", Port: 8082}, {Name: "kullio-marketing", Port: 4321}}, 4321, 5173, 8082, 9229),
			nil,
			[]model.Hostname{{Name: "kullio", Port: 5173}, {Name: "kullio-admin", Port: 8082}, {Name: "kullio-marketing", Port: 4321}},
		},
		{
			// Two unmatched rows and one spare: ambiguous, nothing pairs.
			"ambiguous spare does not pair",
			ws("w", []model.Hostname{{Name: "a", Port: 3000}, {Name: "b", Port: 3001}}, 3005),
			nil,
			[]model.Hostname{{Name: "a", Port: 3000}, {Name: "b", Port: 3001}},
		},
		{
			// Same repo open twice, both pinned to 3003: the other one holds
			// it. Both workspaces carry the SAME name (a second worktree of
			// one repo usually does) — ownership is by id, the name is only
			// what gets shown.
			"port held by another workspace",
			ws("website", []model.Hostname{{Name: "chartlabs-2", Port: 3003}}),
			map[int]portOwner{3003: {"other-id", "website"}},
			[]model.Hostname{{Name: "chartlabs-2", Port: 3003, HeldBy: "website"}},
		},
		{
			// Docker's published port: nobody's pane holds it, and the
			// workspace's unrelated vite server is far from it — routes
			// plainly, never hijacks the vite port.
			"far spare listener is a different server",
			ws("kullio", []model.Hostname{{Name: "kullio-supabase", Port: 54321}}, 5173),
			nil,
			[]model.Hostname{{Name: "kullio-supabase", Port: 54321}},
		},
		{
			// A spare BELOW the configured port is not a bump either.
			"spare below configured port does not pair",
			ws("w", []model.Hostname{{Name: "a", Port: 3005}}, 3000),
			nil,
			[]model.Hostname{{Name: "a", Port: 3005}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolveRoutes(tc.ws, tc.owner)
			for i, want := range tc.want {
				if got := tc.ws.Hostnames[i]; got != want {
					t.Errorf("row %d = %+v, want %+v", i, got, want)
				}
			}
		})
	}
}

// TestApplyListeners pins the scan → workspace filing: rows land under the
// pane's workspace, unknown panes are dropped, and only a change broadcasts
// and pokes the devhost server.
func TestApplyListeners(t *testing.T) {
	m, st := devhostManager(t)
	givePanes(t, m, st, "w1", "pane-1")
	if _, err := m.SetHostnames("w1", []model.Hostname{{Name: "chartlabs", Port: 3003}}); err != nil {
		t.Fatal(err)
	}
	pokes := make(chan struct{}, 8)
	m.OnDevhostChange = func() { pokes <- struct{}{} }

	scan := []listeners.Listener{
		{Port: 3003, PID: 1, Process: "next-server", PaneID: "pane-1"},
		{Port: 9000, PID: 2, Process: "stray", PaneID: "not-a-pane"},
	}
	poked := func() bool {
		select {
		case <-pokes:
			return true
		case <-time.After(time.Second):
			return false
		}
	}
	m.applyListeners(scan)
	ws := m.Workspace("w1")
	if len(ws.Listeners) != 1 || ws.Listeners[0] != (model.Listener{Port: 3003, Process: "next-server", PaneID: "pane-1"}) {
		t.Fatalf("listeners = %+v", ws.Listeners)
	}
	if len(m.Workspace("w2").Listeners) != 0 {
		t.Fatal("stray listener filed under a workspace")
	}
	if !poked() {
		t.Fatal("a changed listener set must poke the devhost server")
	}

	// Same scan again: nothing changed, nothing poked.
	m.applyListeners(scan)
	select {
	case <-pokes:
		t.Fatal("an unchanged scan must not poke")
	default:
	}

	// The server went away: the set empties and that is a change too.
	m.applyListeners(nil)
	if len(m.Workspace("w1").Listeners) != 0 {
		t.Fatal("listeners not cleared")
	}
	if !poked() {
		t.Fatal("clearing must poke")
	}

	// w2 maps the port w1's pane holds: w2's own listeners never change, but
	// its HeldBy does, and that must reach the lens too.
	givePanes(t, m, st, "w2", "pane-2")
	if _, err := m.SetHostnames("w2", []model.Hostname{{Name: "gnubok", Port: 3003}}); err != nil {
		t.Fatal(err)
	}
	for len(pokes) > 0 {
		<-pokes
	}
	m.applyListeners(scan[:1])
	if got := m.Workspace("w2").Hostnames[0].HeldBy; got != "chartlabs" {
		t.Fatalf("HeldBy = %q, want the workspace whose pane holds 3003", got)
	}
	if !poked() {
		t.Fatal("a HeldBy change must poke")
	}
}
