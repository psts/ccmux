package manager

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// TestManager_GitStatusCollector drives the daemon-side git dashboard: a live
// workspace on a real repo gets its status collected + published, and a
// working-tree change produces a fresh "workspace-git" event with the new file.
func TestManager_GitStatusCollector(t *testing.T) {
	for _, bin := range []string{"tmux", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	const socket = "ccmux-git-itest"
	tsrv := &tmux.Server{Socket: socket, ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })

	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()

	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-m", "base")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	id, ch := mgr.SubscribeEvents()
	defer mgr.UnsubscribeEvents(id)

	ws, err := mgr.CreateWorkspace("t", repo, repo, "", "tester", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	mgr.StartGitStatus(50 * time.Millisecond)

	waitGitEvent := func(what string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case ev := <-ch:
				if ev.Kind == "workspace-git" && ev.WorkspaceID == ws.ID {
					return
				}
			case <-deadline:
				t.Fatalf("no workspace-git event: %s", what)
			}
		}
	}

	// First collection: clean repo on main.
	waitGitEvent("initial collection")
	got := mgr.Workspace(ws.ID).Git
	if got == nil || !got.IsGitRepo || got.Branch != "main" || len(got.UntrackedFiles) != 0 {
		t.Fatalf("initial git = %+v, want clean main", got)
	}

	// Dirty the tree → change detected → new event with the file listed.
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitGitEvent("after working-tree change")
	got = mgr.Workspace(ws.ID).Git
	if got == nil || len(got.UntrackedFiles) != 1 || got.UntrackedFiles[0].Path != "new.txt" {
		t.Fatalf("git after change = %+v, want untracked new.txt", got)
	}
}
