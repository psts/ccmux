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

// A task runs in its repo's own session: never an agent's on the same
// folder, a live one before a closed one (nothing is reopened then), and a
// closed one reopened only when it is the only one there is. A repo with no
// session at all is an error the task can show.
func TestTaskWorkspace(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	tsrv := &tmux.Server{Socket: "ccmux-tasks-itest", ConfigPath: "../../config/tmux.conf"}
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
	repo := t.TempDir()
	newWS := func(name string) *model.Workspace {
		ws, err := mgr.CreateWorkspace(name, repo, repo, "", "tester", "")
		if err != nil {
			t.Fatal(err)
		}
		return ws
	}
	closeWS := func(ws *model.Workspace) {
		if _, err := mgr.ArchiveWorkspace(ws.ID); err != nil {
			t.Fatal(err)
		}
		if !waitStatus(mgr, ws.ID, model.StatusCold, 3*time.Second) {
			t.Fatal("not closed")
		}
	}

	if _, err := mgr.TaskWorkspace(repo, ""); err == nil {
		t.Fatal("a repo with no session gave a workspace")
	}
	agentWS := newWS("bot")
	mgr.Workspace(agentWS.ID).Agent = "scout"
	if id, err := mgr.TaskWorkspace(repo, ""); err == nil {
		t.Fatalf("only an agent's session on the repo, got %s", id)
	}

	closed := newWS("admin")
	closeWS(closed)
	live := newWS("admin-2")
	id, err := mgr.TaskWorkspace(repo, "")
	if err != nil || id != live.ID {
		t.Fatalf("with a live session = %s, %v; want the live %s", id, err, live.ID)
	}
	if mgr.Controller(closed.ID) != nil {
		t.Fatal("a closed session was reopened while a live one existed")
	}

	// The task's own window wins over being live: the closed admin is in
	// window w1, so a w1 task reopens it rather than run in admin-2.
	if err := mgr.SeedWindowMembership(closed.ID, "w1"); err != nil {
		t.Fatal(err)
	}
	members, _ := st.WindowMembers()
	id, err = mgr.TaskWorkspace(repo, members[closed.ID])
	if err != nil || id != closed.ID || mgr.Controller(closed.ID) == nil {
		t.Fatalf("a task of w1 = %s, %v; want its window's session %s, reopened", id, err, closed.ID)
	}
	if got, _ := mgr.WindowOfWorkspace(closed.ID); got != members[closed.ID] || got == "" {
		t.Fatalf("window of the w1 session = %q", got)
	}
	if got, _ := mgr.WindowOfWorkspace(live.ID); got != "" {
		t.Fatalf("a session in no window is in %q", got)
	}
	closeWS(closed)

	closeWS(live)
	mgr.Workspace(live.ID).RepoPath = t.TempDir() // leave only the closed one on repo
	id, err = mgr.TaskWorkspace(repo, "")
	if err != nil || id != closed.ID || mgr.Controller(closed.ID) == nil {
		t.Fatalf("only a closed session = %s, %v (live=%v); want it reopened", id, err, mgr.Controller(closed.ID) != nil)
	}
}

// Clearing a worktree closes the task workers started there and nothing
// else: not a person's own pane in the same folder, not a worker elsewhere.
func TestClearTaskFolder(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	tsrv := &tmux.Server{Socket: "ccmux-tasks-clear-itest", ConfigPath: "../../config/tmux.conf"}
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
	repo, wt, other := t.TempDir(), t.TempDir(), t.TempDir()
	ws, err := mgr.CreateWorkspace("admin", repo, repo, "", "tester", "")
	if err != nil {
		t.Fatal(err)
	}
	old, _ := mgr.StartTaskPane(ws.ID, wt, "true")
	elsewhere, _ := mgr.StartTaskPane(ws.ID, other, "true")
	human, err := mgr.SpawnPane(ws.ID, wt, "", "tester")
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.ClearTaskFolder(wt); err != nil {
		t.Fatal(err)
	}
	has := func(id string) bool { return mgr.WorkspaceForPane(id) != "" }
	if has(old) || !has(elsewhere) || !has(human.ID) {
		t.Fatalf("after clearing: old=%v elsewhere=%v human=%v; want only the old worker gone", has(old), has(elsewhere), has(human.ID))
	}
}

// A worker's window peers are the sessions in its own ccmux window, never
// another window's; a session in no window has only itself.
func TestWindowPeers(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	tsrv := &tmux.Server{Socket: "ccmux-tasks-peers-itest", ConfigPath: "../../config/tmux.conf"}
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
	mk := func(name, window string) *model.Workspace {
		dir := t.TempDir()
		ws, err := mgr.CreateWorkspace(name, dir, dir, "", "tester", "")
		if err != nil {
			t.Fatal(err)
		}
		if window != "" {
			if err := mgr.SeedWindowMembership(ws.ID, window); err != nil {
				t.Fatal(err)
			}
		}
		return ws
	}
	app := mk("app", "ChartLabs")
	mk("backend", "ChartLabs")
	mk("blog", "Personal")
	loner := mk("scratch", "")
	names := func(list []*model.Workspace) map[string]bool {
		out := map[string]bool{}
		for _, ws := range list {
			out[ws.Name] = true
		}
		return out
	}
	got := names(mgr.WindowPeers(app.Panes[0].ID))
	if len(got) != 2 || !got["app"] || !got["backend"] {
		t.Fatalf("app's window peers = %v, want app and backend", got)
	}
	if got := names(mgr.WindowPeers(loner.Panes[0].ID)); len(got) != 1 || !got["scratch"] {
		t.Fatalf("a session in no window = %v, want only itself", got)
	}
	if mgr.WindowPeers("no-such-pane") != nil {
		t.Fatal("an unknown pane has peers")
	}

}
