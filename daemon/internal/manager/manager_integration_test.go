package manager

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// TestManager_ReviveReplaysStartup verifies the resurrection path: a workspace
// whose tmux session is killed goes cold, and ReviveWorkspace recreates the
// session and replays each pane's startup command.
func TestManager_ReviveReplaysStartup(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const socket = "ccmux-revive-itest"
	tsrv := &tmux.Server{Socket: socket, ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })

	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	marker := "REVIVE_MARKER_7788"
	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "printf "+marker, "tester", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Kill just the session (server stays up) and wait for the workspace to go
	// cold via the control connection's %exit notice.
	if err := tsrv.KillSession(ws.TmuxSession); err != nil {
		t.Fatalf("kill-session: %v", err)
	}
	if !waitStatus(mgr, ws.ID, model.StatusCold, 3*time.Second) {
		t.Fatalf("workspace did not go cold after session kill")
	}

	// Revive and confirm the startup command replayed into pane 0.
	revived, err := mgr.ReviveWorkspace(ws.ID)
	if err != nil {
		t.Fatalf("revive: %v", err)
	}
	if revived.Status != model.StatusLive {
		t.Fatalf("revived status = %s, want live", revived.Status)
	}
	if !tsrv.HasSession(ws.TmuxSession) {
		t.Fatalf("tmux session not recreated on revive")
	}

	ctrl := mgr.Controller(ws.ID)
	if ctrl == nil {
		t.Fatalf("no controller after revive")
	}
	pane0 := revived.Panes[0].ID
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := ctrl.Capture(pane0, 0)
		if strings.Contains(string(b), marker) {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("startup marker %q not replayed after revive", marker)
}

// TestManager_ArchiveKeepsRecipe pins the "Close Session" path: archive kills
// the tmux session but keeps the registry recipe, so the workspace goes cold
// and revives with its startup command intact. Archive is idempotent.
func TestManager_ArchiveKeepsRecipe(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const socket = "ccmux-archive-itest"
	tsrv := &tmux.Server{Socket: socket, ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })

	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	marker := "ARCHIVE_MARKER_4411"
	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "printf "+marker, "tester", "grp")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	archived, err := mgr.ArchiveWorkspace(ws.ID)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if archived.Status != model.StatusCold {
		t.Fatalf("status = %v, want cold", archived.Status)
	}
	if tsrv.HasSession(ws.TmuxSession) {
		t.Fatal("tmux session survived the archive")
	}
	// The recipe survives: registry row with panes, startup command, group.
	loaded, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	found := false
	for _, l := range loaded {
		if l.ID != ws.ID {
			continue
		}
		found = true
		if len(l.Panes) != 1 || l.Panes[0].StartupCommand != "printf "+marker {
			t.Fatalf("persisted panes = %+v, want the startup recipe", l.Panes)
		}
		if l.Group != "grp" {
			t.Fatalf("persisted group = %q, want grp", l.Group)
		}
	}
	if !found {
		t.Fatal("registry row deleted — archive must keep it")
	}

	// Idempotent on an already-cold workspace.
	if _, err := mgr.ArchiveWorkspace(ws.ID); err != nil {
		t.Fatalf("second archive: %v", err)
	}

	// And it revives.
	revived, err := mgr.ReviveWorkspace(ws.ID)
	if err != nil {
		t.Fatalf("revive: %v", err)
	}
	if revived.Status != model.StatusLive {
		t.Fatalf("revived status = %v, want live", revived.Status)
	}
	if !tsrv.HasSession(ws.TmuxSession) {
		t.Fatal("revive did not recreate the tmux session")
	}
}

// TestManager_KillLastPaneArchivesLiveSession exercises the last-pane branch against
// a real tmux session, which the unit-level fixture cannot: its workspaces have no
// controller, so ArchiveWorkspace returns at its already-cold guard and never kills a
// session, persists a status, or publishes an event. Here the session is live, so all
// three actually run — and the pane row must still survive as the revive recipe.
func TestManager_KillLastPaneArchivesLiveSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const socket = "ccmux-lastpane-itest"
	tsrv := &tmux.Server{Socket: socket, ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })

	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	marker := "LASTPANE_MARKER_9021"
	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "printf "+marker, "tester", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(ws.Panes) != 1 {
		t.Fatalf("fixture panes = %d, want 1", len(ws.Panes))
	}

	if err := mgr.KillPane(ws.ID, ws.Panes[0].ID); err != nil {
		t.Fatalf("kill last pane: %v", err)
	}

	if got := mgr.Workspace(ws.ID); got == nil || got.Status != model.StatusCold {
		t.Fatalf("workspace = %+v, want a cold row", got)
	}
	if tsrv.HasSession(ws.TmuxSession) {
		t.Fatal("tmux session survived closing its last pane")
	}
	// Persisted, not just flipped in memory — the cold-only path never gets here.
	loaded, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	found := false
	for _, l := range loaded {
		if l.ID != ws.ID {
			continue
		}
		found = true
		if l.Status != model.StatusCold {
			t.Fatalf("persisted status = %v, want cold", l.Status)
		}
		if len(l.Panes) != 1 || l.Panes[0].StartupCommand != "printf "+marker {
			t.Fatalf("persisted panes = %+v, want the recipe kept", l.Panes)
		}
	}
	if !found {
		t.Fatal("registry row deleted — closing a pane must not purge the workspace")
	}

	// The whole point of keeping the recipe: it comes back.
	revived, err := mgr.ReviveWorkspace(ws.ID)
	if err != nil {
		t.Fatalf("revive after last-pane close: %v", err)
	}
	if revived.Status != model.StatusLive || !tsrv.HasSession(ws.TmuxSession) {
		t.Fatalf("revive did not restore the session (status=%v)", revived.Status)
	}
}

func waitStatus(mgr *Manager, wsID string, want model.Status, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ws := mgr.Workspace(wsID); ws != nil && ws.Status == want {
			return true
		}
		time.Sleep(30 * time.Millisecond)
	}
	return false
}
