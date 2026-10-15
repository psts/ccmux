package manager

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/model"
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

// A revive starts new sessions, so the clock a dead one left in the registry
// must go, in memory and on disk: kept, a 13-hour-old idle stretch would end
// the revived agent on its first tick. An agent that was not revived keeps
// its clock.
func TestReviveClearsTheDeadSessionsClock(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	tsrv := &tmux.Server{Socket: "ccmux-agentclock-itest", ConfigPath: "../../config/tmux.conf"}
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
	l := AgentLaunch{Name: "radar", Version: "1.0.0", Harness: harness.Harness{Name: "noop", Command: "sleep 30;:"},
		Persist: "sleep 30;:", Deliver: "sleep 30;:", CWD: t.TempDir()}
	revived, err := mgr.CreateAgentWorkspace("radar", "tester", "Chart Labs", l)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := mgr.CreateAgentWorkspace("radar", "tester", "Other", l)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-13 * time.Hour)
	mgr.activity.note(revived.Panes[0].ID, stateIdle, old)
	mgr.activity.note(kept.Panes[0].ID, stateIdle, old)

	if err := tsrv.KillSession(revived.TmuxSession); err != nil {
		t.Fatalf("kill-session: %v", err)
	}
	if !waitStatus(mgr, revived.ID, model.StatusCold, 3*time.Second) {
		t.Fatal("workspace did not go cold")
	}
	if _, err := mgr.ReviveWorkspace(revived.ID); err != nil {
		t.Fatalf("revive: %v", err)
	}
	if _, ok := mgr.activity.get(revived.Panes[0].ID); ok {
		t.Error("revived pane kept the dead session's clock in memory")
	}
	stored, err := st.PaneActivities()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stored[revived.Panes[0].ID]; ok {
		t.Error("revived pane kept the dead session's clock in the registry")
	}
	if _, ok := stored[kept.Panes[0].ID]; !ok {
		t.Error("an agent that was not revived lost its clock")
	}
}

// The restore runs where it matters, in Start: a daemon that comes up reads
// back the clocks the last one saved. Without this, every deploy restarted
// every idle clock and a 12-hour cap could hold an agent up for a day.
func TestStartRestoresSavedClocks(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	tsrv := &tmux.Server{Socket: "ccmux-agentrestore-itest", ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })
	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveWorkspace(&model.Workspace{ID: "ws", Name: "radar", TmuxSession: "ccmux-radar-itest", Agent: "radar"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"idle", "busy", "none"} {
		if err := st.SavePane(&model.Pane{ID: id, WorkspaceID: "ws", Agent: "radar", Harness: "claude"}); err != nil {
			t.Fatal(err)
		}
	}
	idleSince := time.Now().Add(-13 * time.Hour).Truncate(time.Millisecond)
	if err := st.UpdatePaneActivity("idle", string(stateIdle), idleSince.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdatePaneActivity("busy", string(stateBusy), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	if act, ok := mgr.activity.get("idle"); !ok || act.state != stateIdle || !act.since.Equal(idleSince) {
		t.Errorf("idle clock after start = %+v ok=%v, want idle since %v", act, ok, idleSince)
	}
	if !mgr.AgentBusy("busy") {
		t.Error("a busy reading was not restored at start")
	}
	if _, ok := mgr.activity.get("none"); ok {
		t.Error("a pane with no saved reading got one at start")
	}
}
