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
	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "printf "+marker, "tester")
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
