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

	if _, err := mgr.TaskWorkspace(repo); err == nil {
		t.Fatal("a repo with no session gave a workspace")
	}
	agentWS := newWS("bot")
	mgr.Workspace(agentWS.ID).Agent = "scout"
	if id, err := mgr.TaskWorkspace(repo); err == nil {
		t.Fatalf("only an agent's session on the repo, got %s", id)
	}

	closed := newWS("admin")
	closeWS(closed)
	live := newWS("admin-2")
	id, err := mgr.TaskWorkspace(repo)
	if err != nil || id != live.ID {
		t.Fatalf("with a live session = %s, %v; want the live %s", id, err, live.ID)
	}
	if mgr.Controller(closed.ID) != nil {
		t.Fatal("a closed session was reopened while a live one existed")
	}

	closeWS(live)
	mgr.Workspace(live.ID).RepoPath = t.TempDir() // leave only the closed one on repo
	id, err = mgr.TaskWorkspace(repo)
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
