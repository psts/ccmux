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
	writeGates(t, repo, "true\n")   // the folder's own copy is not what counts
	commitFile(t, wt, "marker", "") // the checks see committed work only
	got := g.Run(repo, wt, "main")
	if !got.Configured || got.OK || !strings.Contains(got.Output, "two") || !strings.Contains(got.Output, "failed twice: echo one") {
		t.Fatalf("failing gate = %+v", got)
	}
	if !strings.Contains(got.Full, "one\ntwo") {
		t.Fatalf("the whole output was not kept: %q", got.Full)
	}
	if _, err := os.Stat(filepath.Join(wt, "ran")); !os.IsNotExist(err) {
		t.Fatal("a line after the failing one ran")
	}
}

// A trunk git cannot read is an error, never "the trunk has no list": the
// folder's own untracked copy must not stand in for it.
func TestGateChecks_UnreadableTrunkIsNoFallback(t *testing.T) {
	repo := gitRepo(t)
	wt := worktreeOf(t, repo)
	writeGates(t, repo, "true\n")
	got := (GateChecks{PerLine: 10 * time.Second}).Run(repo, wt, "no-such-trunk")
	if !got.Configured || got.OK || !strings.Contains(got.Output, "no-such-trunk") {
		t.Fatalf("unreadable trunk = %+v", got)
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
	repo := gitRepo(t)
	wt := worktreeOf(t, repo)
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
	if got.OK || !got.Configured || !strings.Contains(got.Output, "failed: cd . && sleep 30") || !strings.Contains(got.Output, "timed out") {
		t.Fatalf("timed out = %+v", got)
	}
}

// A line that fails is run once more: a test that only loses to a busy
// machine passes then, and the result names the line and keeps what its
// failed run printed (the line alone does not say which test it was). A
// line that fails both times fails the checks with both runs kept, and one
// that timed out is not run again (it would only take its whole time twice).
func TestGateChecks_AFailedLineRunsOnceMore(t *testing.T) {
	repo := gitRepo(t)
	wt := worktreeOf(t, repo)
	// Fails the first time it runs in a folder, passes the second.
	writeGates(t, repo, "true\ntest -f seen || { touch seen; echo which-test-flaked; false; }\n")
	got := (GateChecks{PerLine: 10 * time.Second}).Run(repo, wt, "main")
	if !got.OK || len(got.Flaked) != 1 || !strings.HasPrefix(got.Flaked[0].Line, "test -f seen") ||
		!strings.Contains(got.Flaked[0].Output, "which-test-flaked") {
		t.Fatalf("flaky line = %+v, want green with the line named and its failed output kept", got)
	}

	writeGates(t, repo, "echo run >> runs; echo out-of-run-$(wc -l < runs); false\n")
	got = (GateChecks{PerLine: 10 * time.Second}).Run(repo, wt, "main")
	runs, _ := os.ReadFile(filepath.Join(wt, "runs"))
	if got.OK || len(got.Flaked) != 0 || strings.Count(string(runs), "run") != 2 || !strings.Contains(got.Output, "failed twice") {
		t.Fatalf("always-failing line = %+v after %q, want red after two runs", got, runs)
	}
	if !strings.Contains(got.Full, "out-of-run-1") || !strings.Contains(got.Full, "out-of-run-2") {
		t.Fatalf("both runs' output must be kept: %q", got.Full)
	}

	os.Remove(filepath.Join(wt, "runs"))
	writeGates(t, repo, "echo run >> runs; sleep 30\n")
	got = (GateChecks{PerLine: 300 * time.Millisecond}).Run(repo, wt, "main")
	runs, _ = os.ReadFile(filepath.Join(wt, "runs"))
	if got.OK || strings.Count(string(runs), "run") != 1 {
		t.Fatalf("timed-out line = %+v after %q, want red after one run", got, runs)
	}
}

// A line that fails and then only times out when run again is reported by
// its first run, the one that says what is wrong, and not as two failures.
func TestGateChecks_ARealFailureThenATimeoutShowsTheFailure(t *testing.T) {
	repo := gitRepo(t)
	wt := worktreeOf(t, repo)
	writeGates(t, repo, "if test -f seen; then sleep 30; else touch seen; echo the-real-failure; false; fi\n")
	got := (GateChecks{PerLine: 400 * time.Millisecond}).Run(repo, wt, "main")
	if got.OK || !strings.Contains(got.Output, "the-real-failure") || !strings.Contains(got.Output, "timed out") ||
		strings.Contains(got.Output, "failed twice") || !strings.Contains(got.Full, "the-real-failure") {
		t.Fatalf("failure then timeout = %+v, want the first run shown and the timeout said", got)
	}
}

// A line that flaked is still named when a later line fails the checks.
func TestGateChecks_AFlakeBeforeAFailureIsKept(t *testing.T) {
	repo := gitRepo(t)
	wt := worktreeOf(t, repo)
	writeGates(t, repo, "test -f seen || { touch seen; echo flaked-here; false; }\necho broken; false\n")
	got := (GateChecks{PerLine: 10 * time.Second}).Run(repo, wt, "main")
	if got.OK || len(got.Flaked) != 1 || !strings.Contains(got.Flaked[0].Output, "flaked-here") || !strings.Contains(got.Output, "broken") {
		t.Fatalf("flake then failure = %+v, want red with the flake kept", got)
	}
}
