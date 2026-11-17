package taskq

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func commitIn(t *testing.T, dir, msg string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", msg}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

func TestBranchChangesAreWhatTheBranchHoldsThatItsTrunkDoesNot(t *testing.T) {
	repo := gitRepo(t)
	os.WriteFile(filepath.Join(repo, "old.txt"), []byte("one\ntwo\n"), 0o644)
	commitIn(t, repo, "on the trunk")
	wt := filepath.Join(t.TempDir(), "wt")
	if _, err := MakeWorktree(ExecGit{}, repo, wt, "thread/3-x"); err != nil {
		t.Fatal(err)
	}
	got, err := ReadBranchChanges(wt, "main")
	if err != nil || got.Commits != 0 || got.Uncommitted != 0 || len(got.Files) != 0 {
		t.Fatalf("a fresh worktree holds nothing: %+v %v", got, err)
	}
	os.WriteFile(filepath.Join(wt, "old.txt"), []byte("one\n2\nthree\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "new.txt"), []byte("a\nb\n"), 0o644)
	commitIn(t, wt, "on the branch")
	os.WriteFile(filepath.Join(wt, "draft.txt"), []byte("not yet\n"), 0o644)
	// The trunk moving on is not the branch's change.
	os.WriteFile(filepath.Join(repo, "elsewhere.txt"), []byte("x\n"), 0o644)
	commitIn(t, repo, "the trunk moved")
	got, err = ReadBranchChanges(wt, "main")
	if err != nil {
		t.Fatal(err)
	}
	want := []FileChange{{Path: "new.txt", Added: 2}, {Path: "old.txt", Added: 2, Removed: 1}}
	if got.Commits != 1 || got.Uncommitted != 1 || len(got.Files) != 2 || got.Files[0] != want[0] || got.Files[1] != want[1] {
		t.Fatalf("one commit with its two files, one file not committed, none of the trunk's own: %+v", got)
	}
	if _, err := ReadBranchChanges(filepath.Join(t.TempDir(), "gone"), "main"); err == nil {
		t.Fatal("a worktree that is not there is an error, never read as no changes")
	}
}
