package walls

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// repoWithWorktree makes a real repo with one commit and one worktree.
func repoWithWorktree(t *testing.T) (repo, worktree string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	repo, worktree = filepath.Join(dir, "repo"), filepath.Join(dir, "wt", "task-1")
	git := func(in string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		cmd.Dir = in
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(repo, "init", "-q", "-b", "main")
	git(repo, "commit", "-q", "--allow-empty", "-m", "init")
	git(repo, "worktree", "add", "-q", "-b", "task/1", worktree)
	return repo, worktree
}

func TestAWorktreesGitDataIsFoundWithoutRunningGit(t *testing.T) {
	repo, worktree := repoWithWorktree(t)
	got, err := GitOf(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if got.Common != filepath.Join(repo, ".git") || got.Dir != filepath.Join(repo, ".git", "worktrees", "task-1") {
		t.Fatalf("worktree: %+v", got)
	}
	own, err := GitOf(repo)
	if err != nil || own.Dir != own.Common || own.Dir != filepath.Join(repo, ".git") {
		t.Fatalf("repo: %+v %v", own, err)
	}
	if _, err := GitOf(t.TempDir()); err == nil {
		t.Fatal("a folder with no .git is not a git folder")
	}
}

func TestASessionIsWalledIntoItsWorktreeAndTheNarrowPartsOfGit(t *testing.T) {
	repo, worktree := repoWithWorktree(t)
	var p Plan
	if err := p.WorktreeWalls(worktree); err != nil {
		t.Fatal(err)
	}
	dot := filepath.Join(repo, ".git")
	entry := filepath.Join(dot, "worktrees", "task-1")
	for _, want := range []string{worktree, entry, filepath.Join(dot, "objects"), filepath.Join(dot, "refs")} {
		if !slices.Contains(p.Write, want) {
			t.Errorf("not opened: %s (%v)", want, p.Write)
		}
	}
	for _, never := range []string{dot, repo, filepath.Join(dot, "hooks"), filepath.Join(dot, "config")} {
		if slices.Contains(p.Write, never) || slices.Contains(p.Caches, never) {
			t.Errorf("%s must stay closed: opening it lets the session plant what runs outside the walls", never)
		}
	}
	for _, want := range []string{filepath.Join(dot, "hooks"), filepath.Join(dot, "config")} {
		if !slices.Contains(p.Outside, want) {
			t.Errorf("the check must try %s: %v", want, p.Outside)
		}
	}
	for _, want := range []string{filepath.Join(worktree, ".git"), filepath.Join(entry, "commondir"), filepath.Join(entry, "gitdir")} {
		if !slices.Contains(p.Sealed, want) {
			t.Errorf("%s says where the worktree's git data is and must be sealed: %v", want, p.Sealed)
		}
	}
	if err := p.Validate(); err != nil && !strings.Contains(err.Error(), "temporary folder") {
		t.Fatal(err) // the test's own folders are under /tmp, which is all Validate may say
	}
}

func TestASessionIsNeverWalledIntoARepoItself(t *testing.T) {
	repo, _ := repoWithWorktree(t)
	var p Plan
	if err := p.WorktreeWalls(repo); err == nil {
		t.Fatal("a repo's own folder is not a worktree")
	}
}

func TestAJobsRepoIsOpenWithItsHooksAndConfigSealed(t *testing.T) {
	repo, _ := repoWithWorktree(t)
	dot := filepath.Join(repo, ".git")
	if err := os.RemoveAll(filepath.Join(dot, "hooks")); err != nil {
		t.Fatal(err)
	}
	var p Plan
	if err := p.RepoWalls(repo); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(p.Write, repo) || !slices.Contains(p.Write, dot) {
		t.Errorf("the repo and its .git (a mount of its own) are opened: %v", p.Write)
	}
	if !slices.Contains(p.Sealed, filepath.Join(dot, "hooks")) || !slices.Contains(p.Sealed, filepath.Join(dot, "config")) {
		t.Errorf("sealed: %v", p.Sealed)
	}
	if _, err := os.Stat(filepath.Join(dot, "hooks")); err != nil {
		t.Errorf("a missing hooks folder is made, so it can be sealed and not made by the job: %v", err)
	}
}
