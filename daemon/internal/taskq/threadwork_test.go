package taskq

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

func TestAThreadMemberIsNamedAndPlacedLikeATasksPart(t *testing.T) {
	if got := ThreadBusName("/r/backend", 12); got != "backend-thread-12" {
		t.Fatalf("a member's bus name is its repo and its thread: %q", got)
	}
	wt, branch := ThreadPlace("/wt", "/r/backend", 12, "Trade CSV export!")
	task := layoutOf("/wt", store.Task{ID: 12, Title: "x"}, store.Part{RepoPath: "/r/backend"})
	if filepath.Dir(wt) != task.dir || filepath.Base(wt) != "thread-12" {
		t.Fatalf("its worktree sits beside the repo's task worktrees: %q vs %q", wt, task.dir)
	}
	if branch != "thread/12-trade-csv-export" {
		t.Fatalf("its branch is named after the thread: %q", branch)
	}
	if _, untitled := ThreadPlace("/wt", "/r/backend", 12, ""); untitled != "thread/12-task" {
		t.Fatalf("a thread with no title still has a branch name: %q", untitled)
	}
}

func TestMakeWorktreeCutsFromTheTrunkAndCopiesTheLocalSettings(t *testing.T) {
	repo := gitRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".claude", "settings.local.json"), []byte(`{"gates":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt", "thread-3")
	trunk, err := MakeWorktree(ExecGit{}, repo, wt, "thread/3-x")
	if err != nil || trunk == "" {
		t.Fatalf("the worktree is made from the trunk: %q %v", trunk, err)
	}
	if head, _ := runGit(wt, "symbolic-ref", "--short", "HEAD"); head != "thread/3-x" {
		t.Fatalf("it is on its own branch: %q", head)
	}
	if data, err := os.ReadFile(filepath.Join(wt, ".claude", "settings.local.json")); err != nil || !strings.Contains(string(data), "gates") {
		t.Fatalf("the repo's local settings came along: %q %v", data, err)
	}
	if _, err := MakeWorktree(ExecGit{}, repo, wt, "thread/3-x"); err != nil {
		t.Fatalf("making it again keeps it: %v", err)
	}
}
