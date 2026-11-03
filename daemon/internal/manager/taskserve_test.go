package manager

import (
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/listeners"
	"ccmux.dev/ccmuxd/internal/model"
)

// TestTaskWorkerNeverTakesTheSessionsName: a task worker runs in the repo's
// own session. Whether it binds the session's port (the person's server is
// off) or one just above it (the bump rule), the session's name must route
// nowhere rather than serve the worker's half-built app.
func TestTaskWorkerNeverTakesTheSessionsName(t *testing.T) {
	for _, port := range []int{4000, 4001} {
		m, st := devhostManager(t)
		givePanes(t, m, st, "w1", "pane-me", "pane-worker")
		m.mu.Lock()
		m.byID["w1"].ws.Panes[1].CreatedBy = taskRunner
		m.mu.Unlock()
		if _, err := m.SetHostnames("w1", []model.Hostname{{Name: "chartlabs-app", Port: 4000}}); err != nil {
			t.Fatal(err)
		}
		m.applyListeners([]listeners.Listener{{Port: port, Process: "next-server", PaneID: "pane-worker"}})
		ws := m.Workspace("w1")
		if len(ws.Listeners) != 0 {
			t.Fatalf("port %d: the worker's listener was filed as the session's: %+v", port, ws.Listeners)
		}
		if port == 4000 && ws.Hostnames[0].HeldBy != taskWorkerOwner.name {
			t.Fatalf("HeldBy = %q, want the task worker", ws.Hostnames[0].HeldBy)
		}
		want := 4000 // the bump: nobody of the session's holds 4000, so the configured port
		if port == 4000 {
			want = 0 // held by the worker: routes nowhere
		}
		if got := m.AllHostnames()["chartlabs-app"]; got != want {
			t.Fatalf("worker on %d: chartlabs-app routes to %d, want %d", port, got, want)
		}
	}
}

// TestTaskWorkerOwnServerKeepsName: the person's own
// server on the port keeps the name, a worker elsewhere changes nothing.
func TestTaskWorkerOwnServerKeepsName(t *testing.T) {
	m, st := devhostManager(t)
	givePanes(t, m, st, "w1", "pane-me", "pane-worker")
	m.mu.Lock()
	m.byID["w1"].ws.Panes[1].CreatedBy = taskRunner
	m.mu.Unlock()
	if _, err := m.SetHostnames("w1", []model.Hostname{{Name: "chartlabs-app", Port: 4000}}); err != nil {
		t.Fatal(err)
	}
	m.applyListeners([]listeners.Listener{
		{Port: 4000, Process: "next-server", PaneID: "pane-me"},
		{Port: 22070, Process: "next-server", PaneID: "pane-worker"},
	})
	if got := m.AllHostnames()["chartlabs-app"]; got != 4000 {
		t.Fatalf("chartlabs-app routes to %d, want the person's 4000", got)
	}
}

// TestSetTaskRoutesPokesOnlyOnChange: the runner sends the routes every pass.
func TestSetTaskRoutesPokesOnlyOnChange(t *testing.T) {
	m, _ := devhostManager(t)
	pokes := make(chan struct{}, 8)
	m.OnDevhostChange = func() { pokes <- struct{}{} }
	routes := map[string]int{"chartlabs-app-t3": 22030}
	m.SetTaskRoutes(routes)
	m.SetTaskRoutes(map[string]int{"chartlabs-app-t3": 22030})
	routes["x"] = 1 // the manager keeps its own copy
	if got := m.TaskRoutes(); len(got) != 1 || got["chartlabs-app-t3"] != 22030 {
		t.Fatalf("TaskRoutes = %v", got)
	}
	waitPokes(t, pokes, 1)
	m.SetTaskRoutes(nil)
	waitPokes(t, pokes, 1)
	if got := m.TaskRoutes(); got == nil || len(got) != 0 {
		t.Fatalf("TaskRoutes after clearing = %v, want empty and not nil", got)
	}
}

// waitPokes wants exactly n devhost pokes (they arrive from a goroutine).
func waitPokes(t *testing.T, pokes chan struct{}, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-pokes:
		case <-time.After(time.Second):
			t.Fatalf("got %d devhost pokes, want %d", i, n)
		}
	}
	select {
	case <-pokes:
		t.Fatalf("more than %d devhost pokes", n)
	case <-time.After(50 * time.Millisecond):
	}
}
