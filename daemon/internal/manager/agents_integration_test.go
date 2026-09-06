package manager

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// Two adds of one base into one window inside the start window make ONE
// session: the second is refused as already running. The guard is keyed by
// window and base because the first session's membership row lands after
// the session exists, so nothing else can see the duplicate coming.
func TestCreateAgentWorkspace_RefusesADoubleAddInOneWindow(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	tsrv := &tmux.Server{Socket: "ccmux-agentws-itest", ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })
	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	l := AgentLaunch{Name: "x-poster", Version: "1.0.0", Harness: harness.Harness{Name: "noop", Command: "sleep 1;:"},
		Persist: "sleep 1;:", Deliver: "sleep 1;:", CWD: t.TempDir()}
	ws, err := mgr.CreateAgentWorkspace("🐦 x-poster", "tester", "Chart Labs", l)
	if err != nil || ws.Agent != "x-poster" || ws.Panes[0].Agent != "x-poster" {
		t.Fatalf("first add: %v %+v", err, ws)
	}
	if _, err := mgr.CreateAgentWorkspace("🐦 x-poster", "tester", "Chart Labs", l); !errors.Is(err, ErrAgentRunning) {
		t.Fatalf("second add in the window = %v, want ErrAgentRunning", err)
	}
	// Another window is another project: its own add goes through.
	if _, err := mgr.CreateAgentWorkspace("🐦 x-poster", "tester", "Other", l); err != nil {
		t.Fatalf("add in another window: %v", err)
	}
	if n := len(mgr.List()); n != 2 {
		t.Fatalf("sessions = %d, want 2", n)
	}
}
