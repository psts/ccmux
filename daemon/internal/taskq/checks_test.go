package taskq

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// worktreeOf makes a task worktree of repo, as the runner does.
func worktreeOf(t *testing.T, repo string) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "task-1")
	if err := (ExecGit{}).AddWorktree(repo, wt, "task/1-x", "main"); err != nil {
		t.Fatal(err)
	}
	return wt
}

func writeGates(t *testing.T, dir, body string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, ".claude"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, gatesFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGateChecks(t *testing.T) {
	g := GateChecks{PerLine: 10 * time.Second}
	repo := gitRepo(t)
	wt := worktreeOf(t, repo)
	if got := g.Run(repo, wt, "main"); got.Configured || got.OK {
		t.Fatalf("no gates file = %+v, want not configured", got)
	}

	// A list the repo does not track is read from its folder.
	writeGates(t, repo, "# a comment\n\ntest -d .\n")
	if got := g.Run(repo, wt, "main"); !got.Configured || !got.OK {
		t.Fatalf("passing untracked gates = %+v", got)
	}

	// The trunk's list wins, and each line runs from the worktree's root,
	// stopping at the first that fails.
	writeGates(t, repo, "test -f marker\necho one; echo two; false\necho never-reached > ran\n")
	if _, err := runGit(repo, "add", gatesFile); err != nil {
		t.Fatal(err)
	}
	runGit(repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "gates")
	writeGates(t, repo, "true\n") // the folder's own copy is not what counts
	os.WriteFile(filepath.Join(wt, "marker"), nil, 0o644)
	got := g.Run(repo, wt, "main")
	if !got.Configured || got.OK || !strings.Contains(got.Output, "two") || !strings.Contains(got.Output, "failed: echo one") {
		t.Fatalf("failing gate = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(wt, "ran")); !os.IsNotExist(err) {
		t.Fatal("a line after the failing one ran")
	}
}

// A branch that edits the list does not grade itself by its own copy.
func TestGateChecks_BranchCannotWeakenTheList(t *testing.T) {
	repo := gitRepo(t)
	writeGates(t, repo, "false\n")
	runGit(repo, "add", gatesFile)
	runGit(repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "gates")
	wt := filepath.Join(t.TempDir(), "task-1")
	if err := (ExecGit{}).AddWorktree(repo, wt, "task/1-x", "main"); err != nil {
		t.Fatal(err)
	}
	writeGates(t, wt, "true\n")
	runGit(wt, "add", gatesFile)
	runGit(wt, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "weaker gates")
	if got := (GateChecks{PerLine: 10 * time.Second}).Run(repo, wt, "main"); got.OK {
		t.Fatalf("the branch's own list passed it: %+v", got)
	}
}

func TestTail(t *testing.T) {
	if got := tail("a\nb\nc\n", 2); got != "b\nc" {
		t.Fatalf("tail = %q", got)
	}
	if got := tail("a", 5); got != "a" {
		t.Fatalf("short tail = %q", got)
	}
}

// A list of comments only checks nothing, so it is no list at all.
func TestGateChecks_CommentsOnlyIsNotConfigured(t *testing.T) {
	repo, wt := t.TempDir(), t.TempDir()
	writeGates(t, repo, "# checks\n# go test ./...\n\n")
	if got := (GateChecks{PerLine: time.Second}).Run(repo, wt, "main"); got.Configured || got.OK {
		t.Fatalf("comments only = %+v, want not configured", got)
	}
}

// A line past its time is stopped with everything it started, not only
// its shell: a child left holding the output would hang the merge.
func TestGateChecks_TimeoutKillsTheWholeLine(t *testing.T) {
	repo := gitRepo(t)
	wt := worktreeOf(t, repo)
	writeGates(t, repo, "cd . && sleep 30\n")
	start := time.Now()
	got := (GateChecks{PerLine: 300 * time.Millisecond}).Run(repo, wt, "main")
	if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("a timed-out line took %v", took)
	}
	if got.OK || !got.Configured || !strings.Contains(got.Output, "failed: cd . && sleep 30") {
		t.Fatalf("timed out = %+v", got)
	}
}

// A branch that loosens a gate's data (a frozen list) or adds a gate file
// is checked with the trunk's copies, then gets its own files back as
// committed; a branch that leaves .claude/ alone is checked as it is.
func TestGateChecks_RunWithTheTrunksGateFiles(t *testing.T) {
	repo := gitRepo(t)
	commit := func(dir, msg string) {
		runGit(dir, "add", "-A")
		runGit(dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", msg)
	}
	writeGates(t, repo, "! grep -q newfunc .claude/gates/frozen.txt\ntest ! -f .claude/gates/extra.sh\n")
	os.MkdirAll(filepath.Join(repo, ".claude", "gates"), 0o755)
	os.WriteFile(filepath.Join(repo, ".claude", "gates", "frozen.txt"), []byte("oldfunc\n"), 0o644)
	commit(repo, "gates")
	wt := filepath.Join(t.TempDir(), "task-1")
	if err := (ExecGit{}).AddWorktree(repo, wt, "task/1-x", "main"); err != nil {
		t.Fatal(err)
	}
	g := GateChecks{PerLine: 10 * time.Second}
	if got := g.Run(repo, wt, "main"); !got.OK || len(got.TrunkCopies) != 0 {
		t.Fatalf("an untouched .claude/ = %+v", got)
	}

	os.WriteFile(filepath.Join(wt, ".claude", "gates", "frozen.txt"), []byte("oldfunc\nnewfunc\n"), 0o644)
	os.WriteFile(filepath.Join(wt, ".claude", "gates", "extra.sh"), []byte("exit 0\n"), 0o755)
	commit(wt, "loosen the gates")
	got := g.Run(repo, wt, "main")
	if !got.OK || strings.Join(got.TrunkCopies, ",") != ".claude/gates/extra.sh,.claude/gates/frozen.txt" {
		t.Fatalf("checked with the trunk's copies = %+v", got)
	}
	frozen, _ := os.ReadFile(filepath.Join(wt, ".claude", "gates", "frozen.txt"))
	if !strings.Contains(string(frozen), "newfunc") {
		t.Fatalf("the branch's frozen list was not put back: %q", frozen)
	}
	if _, err := os.Stat(filepath.Join(wt, ".claude", "gates", "extra.sh")); err != nil {
		t.Fatal("the branch's added gate file was not put back")
	}
	if left, _ := (ExecGit{}).Uncommitted(wt); left != "" {
		t.Fatalf("the worktree is not clean after the checks: %s", left)
	}
}
