package taskq

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return repo
}

// The trunk is dev where there is one, main otherwise; a repo with neither
// (and no master) builds from what is checked out.
func TestExecGit_Trunk(t *testing.T) {
	repo := gitRepo(t)
	g := ExecGit{}
	if got, err := g.Trunk(repo); err != nil || got != "main" {
		t.Fatalf("main only = %q, %v", got, err)
	}
	if _, err := runGit(repo, "branch", "dev"); err != nil {
		t.Fatal(err)
	}
	if got, _ := g.Trunk(repo); got != "dev" {
		t.Fatalf("with dev = %q, want dev", got)
	}
	other := gitRepo(t)
	if _, err := runGit(other, "branch", "-m", "main", "trunk"); err != nil {
		t.Fatal(err)
	}
	if got, err := g.Trunk(other); err != nil || got != "trunk" {
		t.Fatalf("no dev, main or master = %q, %v; want the checked-out branch", got, err)
	}
}

// A worktree is made on a new branch; one that still exists is kept as it
// is; one deleted by hand comes back on its existing branch.
func TestExecGit_AddWorktree(t *testing.T) {
	repo := gitRepo(t)
	g := ExecGit{}
	wt := filepath.Join(t.TempDir(), "admin", "task-1")
	if err := g.AddWorktree(repo, wt, "task/1-x", "main"); err != nil {
		t.Fatal(err)
	}
	if got, _ := runGit(wt, "branch", "--show-current"); got != "task/1-x" {
		t.Fatalf("new worktree on %q", got)
	}
	marker := filepath.Join(wt, "work-in-progress")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.AddWorktree(repo, wt, "task/1-x", "main"); err != nil {
		t.Fatalf("existing worktree: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("an existing worktree was replaced, losing its work")
	}
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	if err := g.AddWorktree(repo, wt, "task/1-x", "main"); err != nil {
		t.Fatalf("worktree deleted by hand: %v", err)
	}
	if got, _ := runGit(wt, "branch", "--show-current"); got != "task/1-x" {
		t.Fatalf("re-added worktree on %q", got)
	}
}

// The repo's untracked Claude settings follow it into the worktree, and a
// repo without any copies nothing.
func TestCopyLocalSettings(t *testing.T) {
	repo, wt := t.TempDir(), t.TempDir()
	if err := copyLocalSettings(repo, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".claude")); !os.IsNotExist(err) {
		t.Fatal("settings made up for a repo with none")
	}
	os.MkdirAll(filepath.Join(repo, ".claude"), 0o755)
	os.WriteFile(filepath.Join(repo, ".claude", "settings.local.json"), []byte(`{"qa":1}`), 0o644)
	if err := copyLocalSettings(repo, wt); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(wt, ".claude", "settings.local.json")); string(got) != `{"qa":1}` {
		t.Fatalf("copied = %q", got)
	}
}
