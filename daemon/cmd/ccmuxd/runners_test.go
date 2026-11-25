package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/runner"
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
	bus.RunnerInbox = func(string, string, string, string) error { return nil } // runners on: wireRunners and EnablePeers set both
	changed := sessionChanged(mgr, bus, nil)
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

// The manager's mark on a repo's main session reaches the bus through this
// wiring: a message to the repo's plain name goes to the main session's
// inbox, and never to a thread of the same repo, which answers only to its
// own name.
func TestARepoPlainNameReachesItsMainSessionThroughTheWiring(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tsrv := &tmux.Server{Socket: "ccmux-runners-main-test"}
	t.Cleanup(func() { _ = tsrv.KillServer() })
	if err := st.SaveWorkspace(&model.Workspace{ID: "ws", Name: "app", RepoPath: "/r/app", TmuxSession: "ccmux-app-main-wiring"}); err != nil {
		t.Fatal(err)
	}
	mgr := manager.New(context.Background(), tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Skipf("no tmux to start a manager with: %v", err)
	}
	bus := peers.NewService(st, mgr, []byte("test-secret-test-secret-test-sec"))
	bus.RunnerPane = runnerPaneForBus(mgr)
	var mu sync.Mutex
	var got []string // "pane|sender" of each prompt handed to a session
	bus.RunnerInbox = func(paneID, _, _, sender string) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, paneID+"|"+sender)
		return nil
	}
	prompts := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) }
	changed := sessionChanged(mgr, bus, nil)
	// A session outside ccmux in a sibling folder shares the repo's group.
	web := bus.Register(peers.RegisterReq{PID: os.Getpid(), CWD: "/r/web", GitRoot: "/r/web"}).PeerID

	changed(store.RunnerSession{ID: "0a1b2c3d-0000-0000-0000-000000000001", Cwd: "/r/app", Wanted: store.SessionRunning, PaneID: "thr1", WorkspaceID: "ws"})
	if resp := bus.Send(peers.SendReq{FromID: web, ToName: "app", Text: "anyone?"}); resp.OK || !strings.Contains(resp.Error, `Peer "app" not found`) {
		t.Fatalf("with only a thread in the repo, its plain name reaches nobody: %+v", resp)
	}
	changed(store.RunnerSession{ID: "11112222-0000-0000-0000-000000000002", Cwd: "/r/app", Wanted: store.SessionAsleep, PaneID: "main1", WorkspaceID: "ws", Kind: store.SessionLead})
	if info, ok := bus.RunnerPane("main1"); !ok || !info.Main || !info.Asleep || info.Name != "app-s-11112222" {
		t.Fatalf("the bus is told which pane is the repo's main session: %+v %v", info, ok)
	}
	if info, _ := bus.RunnerPane("thr1"); info.Main {
		t.Fatalf("a thread is no main session: %+v", info)
	}
	if resp := bus.Send(peers.SendReq{FromID: web, ToName: "app", Text: "what is the plan?"}); !resp.OK || resp.Spawning {
		t.Fatalf("the plain name reaches the asleep main session: %+v", resp)
	}
	for end := time.Now().Add(10 * time.Second); len(prompts()) == 0 && time.Now().Before(end); {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // a second hand-over would be on its way by now
	if p := prompts(); len(p) != 1 || p[0] != "main1|web" {
		t.Fatalf("prompts = %v, want the one message in the main session's inbox and none in the thread's", p)
	}
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
	bus.RunnerInbox = func(string, string, string, string) error { return nil } // runners on: wireRunners and EnablePeers set both
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

// A scratch daemon (CCMUXD_ROOT) runs sessions only under a unit name of
// its own: with the live name it would rewrite the live session template
// with its own binary, and once that binary's folder is gone every new
// session start fails (2026-11-26, a task builder's smoke test). The
// person's daemon keeps the live name.
func TestAScratchDaemonNeverTakesTheLiveSessionTemplate(t *testing.T) {
	cases := []struct {
		flag    string
		given   bool
		scratch string
		want    string
	}{
		{runner.DefaultPrefix, false, "", runner.DefaultPrefix},
		{runner.DefaultPrefix, false, "/run/user/1000/t1", ""},
		{runner.DefaultPrefix, true, "/run/user/1000/t1", ""},
		{"ccmux-session-t1", true, "/run/user/1000/t1", "ccmux-session-t1"},
		{"ccmux-session-t1", true, "", "ccmux-session-t1"},
	}
	for i, c := range cases {
		got, why := runnerPrefix(c.flag, c.given, c.scratch)
		if got != c.want {
			t.Errorf("case %d: prefix %q, want %q (%s)", i, got, c.want, why)
		}
		if got == "" && !strings.Contains(why, "-runner-unit-prefix") {
			t.Errorf("case %d: refusal should name the flag: %q", i, why)
		}
	}
}
