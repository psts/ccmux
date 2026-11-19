package taskq

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
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

// A thread's page reads a member's changes every few seconds while its
// merge runs. That read must not write the worktree's index: a plain `git
// status` rewrites it, under index.lock, when a file's timestamp changed,
// and a merge's rebase starting in that moment fails on the lock.
func TestReadingAWorktreesChangesNeverRewritesItsIndex(t *testing.T) {
	repo := gitRepo(t)
	os.WriteFile(filepath.Join(repo, "old.txt"), []byte("one\n"), 0o644)
	commitIn(t, repo, "on the trunk")
	wt := filepath.Join(t.TempDir(), "wt")
	if _, err := MakeWorktree(ExecGit{}, repo, wt, "thread/3-x"); err != nil {
		t.Fatal(err)
	}
	index, err := runGit(wt, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		t.Fatal(err)
	}
	// A tracked file touched and not changed: the index's record of it is
	// stale, which is what makes a plain status write the index anew.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(wt, "old.txt"), later, later); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt, "draft.txt"), []byte("not yet\n"), 0o644)
	before, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadBranchChanges(wt, "main")
	if err != nil || got.Uncommitted != 1 || got.Commits != 0 {
		t.Fatalf("the read still says what is there: one file not committed, and the touched one is no change: %+v %v", got, err)
	}
	after, err := os.Stat(index)
	if err != nil || !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("the index was written by a read (%v): a rebase starting then would find it locked", err)
	}
	// The rig is honest: a plain status does write it.
	if _, err := runGit(wt, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	if plain, _ := os.Stat(index); os.SameFile(before, plain) && plain.ModTime().Equal(before.ModTime()) {
		t.Skip("this git does not rewrite the index on a plain status; the check above proves nothing here")
	}
}
