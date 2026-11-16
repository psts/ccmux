package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// A second daemon on this host keeps every file of its own under CCMUXD_ROOT,
// so it never writes into the folders the live daemon uses.
func TestAScratchDaemonKeepsItsFilesUnderItsRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CCMUXD_ROOT", root)
	for name, dir := range map[string]string{"config": configDir(), "runtime": runtimeDir(), "session state": runnerStateDir()} {
		if !strings.HasPrefix(dir, root+string(os.PathSeparator)) {
			t.Errorf("%s folder %q is outside the root %q", name, dir, root)
		}
	}
	if defaultDBPath() != filepath.Join(root, "config", "ccmuxd.db") {
		t.Errorf("the registry follows the config folder: %s", defaultDBPath())
	}
	t.Setenv("CCMUXD_ROOT", "")
	if strings.HasPrefix(configDir(), root) || strings.HasPrefix(runtimeDir(), root) {
		t.Error("without it the user's own folders are used, as before")
	}
}

func TestTheShimBesideTheDaemonIsTheOneSessionsLoad(t *testing.T) {
	if got := peersShim("/opt/named/ccmux-peers"); got != "/opt/named/ccmux-peers" {
		t.Fatalf("a named shim wins: %q", got)
	}
	beside := filepath.Join(filepath.Dir(selfExecutable()), "ccmux-peers")
	if err := os.WriteFile(beside, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Skipf("cannot write beside the test binary: %v", err)
	}
	t.Cleanup(func() { os.Remove(beside) })
	if got := peersShim(""); got != beside {
		t.Fatalf("the shim built with this daemon: got %q, want %q", got, beside)
	}
}

// What the supervisor's change callback does: the pane follows the row, and
// a session that has not ended is on the bus, asleep included.
func TestASessionsChangeMovesItsPaneAndItsPeer(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tsrv := &tmux.Server{Socket: "ccmux-runners-wiring-test"}
	t.Cleanup(func() { _ = tsrv.KillServer() })
	if err := st.SaveWorkspace(&model.Workspace{ID: "ws", Name: "app", RepoPath: "/r/app", TmuxSession: "ccmux-app-wiring"}); err != nil {
		t.Fatal(err)
	}
	mgr := manager.New(context.Background(), tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Skipf("no tmux to start a manager with: %v", err)
	}
	bus := peers.NewService(st, mgr, []byte("test-secret-test-secret-test-sec"))
	bus.RunnerPane = runnerPaneForBus(mgr)
	changed := sessionChanged(mgr, bus)
	sub, events := mgr.SubscribeEvents()
	defer mgr.UnsubscribeEvents(sub)

	sess := store.RunnerSession{ID: "0a1b2c3d-0000-0000-0000-000000000001", Cwd: "/r/app", Wanted: store.SessionAsleep, PaneID: "run1", WorkspaceID: "ws"}
	changed(sess)
	if ev := <-events; ev.Kind != "sessions-changed" || ev.WorkspaceID != "ws" {
		t.Fatalf("every lens is told the workspace's sessions changed, so a page listing them reads again: %+v", ev)
	}
	if bus.PeerForPane("run1") == "" {
		t.Fatal("an asleep session is on the bus: a message is what wakes it")
	}
	if info, ok := bus.RunnerPane("run1"); !ok || info.Name != "app-s-0a1b2c3d" || info.Repo != "/r/app" || !info.Asleep {
		t.Fatalf("the bus is told its name, its repo and that it sleeps by the manager: %+v %v", info, ok)
	}
	sess.Wanted = store.SessionRunning
	changed(sess)
	if info, _ := bus.RunnerPane("run1"); info.Asleep {
		t.Fatalf("and that it woke: %+v", info)
	}
	sess.Wanted = store.SessionDone
	changed(sess)
	if bus.PeerForPane("run1") != "" {
		t.Fatal("an ended session is not")
	}
	changed(store.RunnerSession{ID: "bare", Wanted: store.SessionRunning}) // no pane: nothing to do, nothing to break
	registerRunnerPeers(mgr, bus)
	loadRunnerPanes(mgr)
}

// The bus keeps its peers in memory, and an asleep session has no shim to
// register it again: after a daemon restart, rebuilding the panes from their
// rows and registering their peers is all that keeps such a session
// reachable by the message that would wake it.
func TestADaemonRestartPutsSessionsBackOnTheBus(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SaveWorkspace(&model.Workspace{ID: "ws", Name: "app", RepoPath: "/r/app", TmuxSession: "ccmux-app-restart"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePane(&model.Pane{ID: "tmux1", WorkspaceID: "ws", CWD: "/r/app", Harness: "claude"}); err != nil {
		t.Fatal(err)
	}
	// What the last daemon left: one session asleep, one running, one ended.
	for id, wanted := range map[string]string{"asleep1": store.SessionAsleep, "running1": store.SessionRunning, "ended1": store.SessionDone} {
		row := store.RunnerSession{ID: id, Cwd: "/r/app", Wanted: wanted, PaneID: "pane-" + id, WorkspaceID: "ws", Name: id}
		if err := st.AddRunnerSession(row); err != nil {
			t.Fatal(err)
		}
	}

	// The daemon comes back: a new manager and a new, empty bus.
	tsrv := &tmux.Server{Socket: "ccmux-runners-restart-test"}
	t.Cleanup(func() { _ = tsrv.KillServer() })
	mgr := manager.New(context.Background(), tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Skipf("no tmux to start a manager with: %v", err)
	}
	bus := peers.NewService(st, mgr, []byte("test-secret-test-secret-test-sec"))
	bus.RunnerPane = runnerPaneForBus(mgr)
	if bus.PeerForPane("pane-asleep1") != "" {
		t.Fatal("a bus that just started knows nobody")
	}
	loadRunnerPanes(mgr)
	registerRunnerPeers(mgr, bus)

	for _, id := range []string{"asleep1", "running1"} {
		if bus.PeerForPane("pane-"+id) == "" {
			t.Errorf("session %s is not back on the bus: a message could not reach it, or wake it", id)
		}
	}
	if info, ok := bus.RunnerPane("pane-asleep1"); !ok || info.Name != "asleep1" || !info.Asleep {
		t.Errorf("under the name its row gives it, and known to be asleep: %+v %v", info, ok)
	}
	// What must not get a peer from this: an ended session, and the repo's
	// tmux pane, whose own Claude registers itself or not at all.
	for _, paneID := range []string{"pane-ended1", "tmux1"} {
		if bus.PeerForPane(paneID) != "" {
			t.Errorf("pane %s was given a peer", paneID)
		}
	}
	if got := mgr.ResolvePane("pane-ended1", "/r/app"); got != "" {
		t.Errorf("an ended session's late hook lands on %q, want nowhere", got)
	}
}
