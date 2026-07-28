package manager

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// The failure this exists to end: a workspace whose tmux session is alive gets
// cooled — a spurious %exit, or an attach that failed at boot — and stays cold
// forever, its controller nilled with no route back. Every Claude inside it
// keeps running, invisible to every lens. Reconcile must bring it back.
func TestReconcile_ReattachesCooledWorkspaceWhoseSessionLives(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const socket = "ccmux-reconcile-itest"
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
	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "", "tester", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Cool it WITHOUT touching tmux — the session and its work stay alive.
	mgr.markCold(ws.ID)
	if got := mgr.entry(ws.ID); got == nil || got.ws.Status != model.StatusCold {
		t.Fatal("precondition: workspace should be cold")
	}
	if status := storedStatus(t, st, ws.ID); status != model.StatusCold {
		t.Fatalf("precondition: registry should read cold, got %q", status)
	}

	mgr.Reconcile()

	if !waitStatus(mgr, ws.ID, model.StatusLive, 3*time.Second) {
		t.Fatal("reconcile must re-attach a workspace whose tmux session is alive")
	}
	// And the registry must agree — a status only held in memory is the drift
	// that made this invisible in the first place.
	if status := storedStatus(t, st, ws.ID); status != model.StatusLive {
		t.Fatalf("registry status after reconcile = %q, want live", status)
	}
}

// A session that is genuinely gone must stay cold: reconcile repairs drift, it
// does not resurrect.
func TestReconcile_LeavesTrulyDeadWorkspaceCold(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const socket = "ccmux-reconcile-dead-itest"
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
	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "", "tester", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := tsrv.KillSession(ws.TmuxSession); err != nil {
		t.Fatalf("kill-session: %v", err)
	}
	if !waitStatus(mgr, ws.ID, model.StatusCold, 3*time.Second) {
		t.Fatal("workspace should go cold when its session is killed")
	}

	mgr.Reconcile()
	time.Sleep(200 * time.Millisecond)

	if e := mgr.entry(ws.ID); e == nil || e.ws.Status != model.StatusCold {
		t.Fatal("a workspace with no tmux session must stay cold")
	}
}

// storedStatus reads a workspace's status straight from the registry, which is
// the half that used to disagree with memory.
func storedStatus(t *testing.T, st *store.SQLite, wsID string) model.Status {
	t.Helper()
	all, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, ws := range all {
		if ws.ID == wsID {
			return ws.Status
		}
	}
	t.Fatalf("workspace %s not in registry", wsID)
	return ""
}
