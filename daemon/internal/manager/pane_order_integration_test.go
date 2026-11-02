package manager

import (
	"context"
	"os/exec"
	"testing"

	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// startOrderManager is a manager on its own tmux socket and a fresh registry,
// with `saved` written to the registry before Start loads it.
func startOrderManager(t *testing.T, socket string, saved ...*model.Workspace) (*Manager, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	tsrv := &tmux.Server{Socket: socket, ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })
	path := t.TempDir() + "/reg.db"
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, ws := range saved {
		if err := st.SaveWorkspace(ws); err != nil {
			t.Fatal(err)
		}
		for _, p := range ws.Panes {
			if err := st.SavePane(p); err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := New(ctx, tsrv, st)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	return m, path
}

func servedIDs(t *testing.T, m *Manager, wsID string) []string {
	t.Helper()
	for _, ws := range m.List() {
		if ws.ID == wsID {
			return idsOf(ws.Panes)
		}
	}
	t.Fatalf("workspace %s not served", wsID)
	return nil
}

// A new agent tab goes after the agents and before the terminals, a new
// terminal goes last, and the stored positions match, so a restart serves the
// same order.
func TestSpawnPane_AgentTabsGoBeforeTerminals(t *testing.T) {
	m, path := startOrderManager(t, "ccmux-order-spawn-itest")
	ws, err := m.CreateWorkspace("w", "/tmp", "/tmp", "", "tester", "")
	if err != nil {
		t.Fatal(err)
	}
	term0 := ws.Panes[0].ID
	spawn := func(harness string) string {
		t.Helper()
		p, err := m.spawnPane(ws.ID, "/tmp", "true", "true", "tester", harness, false, "")
		if err != nil {
			t.Fatalf("spawn %q: %v", harness, err)
		}
		return p.ID
	}
	term1 := spawn("") // a terminal goes last, nothing moves
	if got := servedIDs(t, m, ws.ID); !sameIDs(got, []string{term0, term1}) {
		t.Fatalf("after a terminal: %v", got)
	}
	pi := spawn("pi")
	claude := spawn("claude")
	want := []string{pi, claude, term0, term1}
	if got := servedIDs(t, m, ws.ID); !sameIDs(got, want) {
		t.Fatalf("served %v, want %v", got, want)
	}

	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	all, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || !sameIDs(idsOf(all[0].Panes), want) {
		t.Fatalf("stored order %v, want %v", idsOf(all[0].Panes), want)
	}
}

// A session saved before the rule (terminal first) is served agent first at
// startup; one with no agent keeps its order.
func TestStart_ServesSavedAgentsFirst(t *testing.T) {
	old := &model.Workspace{ID: "old", Name: "old", TmuxSession: "ccmux-old", Panes: []*model.Pane{
		{ID: "t", WorkspaceID: "old", Position: 0, CreatedAt: 1},
		{ID: "c", WorkspaceID: "old", Position: 1, CreatedAt: 2, Harness: "claude"},
	}}
	shells := &model.Workspace{ID: "shells", Name: "shells", TmuxSession: "ccmux-shells", Panes: []*model.Pane{
		{ID: "a", WorkspaceID: "shells", Position: 0, CreatedAt: 1},
		{ID: "b", WorkspaceID: "shells", Position: 1, CreatedAt: 2},
	}}
	m, _ := startOrderManager(t, "ccmux-order-start-itest", old, shells)
	if got := servedIDs(t, m, "old"); !sameIDs(got, []string{"c", "t"}) {
		t.Fatalf("saved terminal-first session served %v, want [c t]", got)
	}
	if got := servedIDs(t, m, "shells"); !sameIDs(got, []string{"a", "b"}) {
		t.Fatalf("session with no agent served %v, want [a b]", got)
	}
}

// A terminal that starts an agent ("start it here") moves up with the agents
// at once, and the store holds the new order; a terminal that stays one does
// not move.
func TestStartInPane_TerminalThatBecomesAnAgentMovesFirst(t *testing.T) {
	m, path := startOrderManager(t, "ccmux-order-startin-itest")
	ws, err := m.CreateWorkspace("w", "/tmp", "/tmp", "", "tester", "")
	if err != nil {
		t.Fatal(err)
	}
	term0 := ws.Panes[0].ID
	term1, err := m.spawnPane(ws.ID, "/tmp", "", "", "tester", "", false, "")
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	term1.RawCommand = "bash" // at a bare shell, as the tmux signal would report
	m.mu.Unlock()
	if err := m.startInPane(term1.ID, harness.Harness{Name: "pi"}, "true", "true", ""); err != nil {
		t.Fatal(err)
	}
	want := []string{term1.ID, term0}
	if got := servedIDs(t, m, ws.ID); !sameIDs(got, want) {
		t.Fatalf("served %v, want %v", got, want)
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	all, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || !sameIDs(idsOf(all[0].Panes), want) {
		t.Fatalf("stored order %v, want %v", idsOf(all[0].Panes), want)
	}
}
