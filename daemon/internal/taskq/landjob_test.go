package taskq

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/jobs"
)

// countedChecks counts the runs of each repo's checks.
type countedChecks struct{ runs map[string]int }

func (c *countedChecks) Run(repo, _, _ string) CheckResult {
	c.runs[repo]++
	return CheckResult{Configured: true, OK: true}
}

func journalAt(t *testing.T, path string) runSteps {
	t.Helper()
	j, err := jobs.OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	return runSteps{&jobs.Run{Journal: j}}
}

// A landing cut off after its checks passed (the job was killed, the host
// rebooted) and run again: the checks that passed are not run a second
// time, and the trunks move.
func TestALandingRunAgainSkipsTheChecksThatPassed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	git, checks := &fakeGit{}, &countedChecks{runs: map[string]int{}}
	work := []Work{{RepoPath: "/p/app", Worktree: "/wt/app-1/thread-3", Label: "#3"}, {RepoPath: "/p/backend", Worktree: "/wt/backend-1/thread-3", Label: "#3"}}
	asked := 0
	first := Lander{Git: git, MergeGit: git, Checks: checks, Tag: "test", Steps: journalAt(t, path), Halt: func() error {
		if asked++; asked == 3 { // both prepared and checked: cut off before the first trunk moves
			return errors.New("killed")
		}
		return nil
	}}
	if _, err := first.Land(work); err == nil || len(git.ffs) != 0 {
		t.Fatalf("the first run is cut off before anything landed: %v %v", err, git.ffs)
	}
	second := Lander{Git: git, MergeGit: git, Checks: checks, Tag: "test", Steps: journalAt(t, path)}
	note, err := second.Land(work)
	if err != nil || note != "merged app into dev, backend into dev" {
		t.Fatalf("the second run lands: %q %v", note, err)
	}
	if checks.runs["/p/app"] != 1 || checks.runs["/p/backend"] != 1 {
		t.Fatalf("each repo's checks ran once across both runs: %v", checks.runs)
	}
	if strings.Join(git.ffs, " ") != "app@rev-app-1 backend@rev-backend-1" {
		t.Fatalf("both trunks moved, to the commits that were checked: %v", git.ffs)
	}
	j, _ := jobs.OpenJournal(path)
	if landed := landedSoFar(j); strings.Join(landed, "|") != "app into dev|backend into dev" {
		t.Fatalf("the journal says what landed: %v", landed)
	}
}

// Cut off between two trunks: the run after it goes through the repo that
// already landed (moving a trunk to where it is changes nothing) and lands
// the other. A failure there still names the half that is in.
func TestALandingCutOffBetweenTwoTrunksFinishesOrSaysWhichHalfIsIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	git, checks := &fakeGit{ffErr: map[string]error{"/p/backend": errors.New("index.lock exists")}}, &countedChecks{runs: map[string]int{}}
	work := []Work{{RepoPath: "/p/app", Worktree: "/wt/app-1/thread-3"}, {RepoPath: "/p/backend", Worktree: "/wt/backend-1/thread-3"}}
	first := Lander{Git: git, MergeGit: git, Checks: checks, Steps: journalAt(t, path)}
	_, err := first.Land(work)
	if err == nil || !strings.Contains(err.Error(), "backend did not merge") || !strings.Contains(err.Error(), "(already merged: app into dev)") {
		t.Fatalf("a half merge says which half is in: %v", err)
	}
	j, _ := jobs.OpenJournal(path)
	out := LandResult(work, "", err, landedSoFar(j))
	if out.OK || out.Fixable != -1 || len(out.Landed) != 1 || out.Landed[0] != "app into dev" {
		t.Fatalf("and so does the job's result: %+v", out)
	}
	git.ffErr = nil
	second := Lander{Git: git, MergeGit: git, Checks: checks, Steps: journalAt(t, path)}
	if note, err := second.Land(work); err != nil || note != "merged app into dev, backend into dev" {
		t.Fatalf("asked again, it finishes: %q %v", note, err)
	}
	if checks.runs["/p/app"] != 1 || checks.runs["/p/backend"] != 1 {
		t.Fatalf("with no check run twice: %v", checks.runs)
	}
}

// A failure crosses from the job to the daemon as text, and is the same
// kind of failure on the other side.
func TestALandingsFailureKeepsItsKindAcrossTheResultFile(t *testing.T) {
	work := []Work{{RepoPath: "/p/app", Worktree: "/wt/app-1/thread-3"}, {RepoPath: "/p/backend", Worktree: "/wt/backend-1/thread-3"}}
	through := func(err error) LandOutput {
		t.Helper()
		raw, _ := json.Marshal(LandResult(work, "", err, nil))
		var out LandOutput
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	red := errors.New("backend: " + (fixable{1, errors.New("checks failed:\nFAIL\n(the whole output is in /wt/backend-1/thread-3.check.log)")}).Error())
	out := through(errors.Join(fixable{1, red}))
	if i, ok := Fixable(out.Err()); !ok || i != 1 || out.Err().Error() != out.Error || out.CheckLog != "/wt/backend-1/thread-3.check.log" {
		t.Fatalf("a failure in one worktree still names it, and the file with the check's output: %+v", out)
	}
	var blocked folderBlocked
	if out := through(folderBlocked{why: "waiting to merge: app has uncommitted changes on dev"}); !errors.As(out.Err(), &blocked) || blocked.why != "waiting to merge: app has uncommitted changes on dev" {
		t.Fatalf("a folder in the way is still that: %+v", out)
	}
	if out := through(errors.New("app: lists no checks")); out.Fixable != -1 || out.Blocked || out.Err() == nil {
		t.Fatalf("any other failure is nobody's in particular: %+v", out)
	}
	if out := through(nil); !out.OK || out.Err() != nil {
		t.Fatalf("a landing that worked has no error: %+v", out)
	}
}

// The job itself, over real git: it lands a branch, and a job that finds
// the mark of a rebase an earlier job was killed in undoes that rebase
// first instead of failing on it.
func TestTheLandingJobLandsAndUndoesARebaseAnEarlierJobWasKilledIn(t *testing.T) {
	repo := gitRepo(t)
	os.MkdirAll(filepath.Join(repo, ".claude"), 0o755)
	commitFile(t, repo, ".claude/qa-gates", "test -f feature.txt\n")
	g := ExecGit{}
	wt := filepath.Join(t.TempDir(), "thread-3")
	if err := g.AddWorktree(repo, wt, "thread/3-x", "main"); err != nil {
		t.Fatal(err)
	}
	commitFile(t, wt, "feature.txt", "new")
	commitFile(t, wt, "shared.txt", "theirs")
	commitFile(t, repo, "shared.txt", "ours") // the trunk moved: the rebase will clash
	// An earlier job was killed in the middle of that rebase.
	runGit(wt, "rebase", "main")
	if _, err := os.Stat(filepath.Join(repo, ".git", "worktrees", "thread-3", "rebase-merge")); err != nil {
		t.Fatalf("the rig: the worktree is mid-rebase: %v", err)
	}
	os.WriteFile(RebaseMark(wt), []byte("then\n"), 0o644)

	dir := t.TempDir()
	input, _ := json.Marshal(LandInput{Work: []Work{{RepoPath: repo, Worktree: wt, Branch: "thread/3-x", Label: "#3"}}, Tag: "test", PerLineSeconds: 60})
	run := func() LandOutput {
		t.Helper()
		j, err := jobs.OpenJournal(filepath.Join(dir, "journal.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		out, err := RunLand(&jobs.Run{Spec: jobs.Spec{ID: "j1", Input: input, Locks: filepath.Join(dir, "locks")}, Journal: j, Ctx: context.Background()})
		if err != nil {
			t.Fatal(err)
		}
		return out.(LandOutput)
	}
	out := run()
	if out.OK || out.Fixable != 0 || !strings.Contains(out.Error, "clashes with main") {
		t.Fatalf("the cut rebase is undone and the real clash is what is said: %+v", out)
	}
	if _, err := os.Stat(RebaseMark(wt)); !os.IsNotExist(err) {
		t.Fatal("no mark is left beside a worktree that is not mid-rebase")
	}
	if status, _ := runGit(wt, "status", "--porcelain"); status != "" {
		t.Fatalf("the worktree is as its worker left it: %q", status)
	}
	// The worker fixes the clash; the next job lands it.
	runGit(wt, "-c", "user.email=t@t", "-c", "user.name=t", "rebase", "-X", "theirs", "main")
	out = run()
	if !out.OK || out.Note != "merged "+filepath.Base(repo)+" into main" || len(out.Landed) != 1 {
		t.Fatalf("the branch lands: %+v", out)
	}
	if head, _ := runGit(repo, "rev-parse", "main"); head == "" {
		t.Fatal("no trunk")
	} else if tip, _ := runGit(wt, "rev-parse", "HEAD"); tip != head {
		t.Fatalf("the trunk is at the commit that was checked: %s vs %s", head, tip)
	}
	if _, err := os.Stat(jobs.LockFile(filepath.Join(dir, "locks"), repo)); err != nil {
		t.Fatalf("the job held the repo's lock file: %v", err)
	}
}

// A landing the job's time limit cuts off in the middle of a check: the
// check is killed, which from inside looks like a check that failed. It
// must come out as the job's error, never as a result that says the
// worker's checks failed. A check that really fails, with time to spare,
// still comes out as exactly that.
func TestALandingCutOffByItsTimeLimitIsNotAFailedCheck(t *testing.T) {
	land := func(gates string, limit time.Duration) (any, error, time.Duration) {
		t.Helper()
		repo := gitRepo(t)
		os.MkdirAll(filepath.Join(repo, ".claude"), 0o755)
		commitFile(t, repo, ".claude/qa-gates", gates)
		wt := filepath.Join(t.TempDir(), "thread-3")
		if err := (ExecGit{}).AddWorktree(repo, wt, "thread/3-x", "main"); err != nil {
			t.Fatal(err)
		}
		commitFile(t, wt, "feature.txt", "new")
		dir := t.TempDir()
		j, err := jobs.OpenJournal(filepath.Join(dir, "journal.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()
		input, _ := json.Marshal(LandInput{Work: []Work{{RepoPath: repo, Worktree: wt, Branch: "thread/3-x", Label: "#3"}}, Tag: "test", PerLineSeconds: 60})
		start := time.Now()
		out, err := RunLand(&jobs.Run{Spec: jobs.Spec{ID: "j1", Input: input, Locks: filepath.Join(dir, "locks")}, Journal: j, Ctx: ctx})
		if head, _ := runGit(repo, "log", "--format=%s", "-1", "main"); head != ".claude/qa-gates" {
			t.Fatalf("nothing landed: main is at %q", head)
		}
		return out, err, time.Since(start)
	}
	out, err, took := land("sleep 30\n", time.Second)
	if !errors.Is(err, context.DeadlineExceeded) || out != nil {
		t.Fatalf("cut off in a check, the landing is the job's error and no result: %+v %v", out, err)
	}
	if took > 15*time.Second {
		t.Fatalf("and the check was ended with it, not waited for: %s", took)
	}
	out, err, _ = land("test -f no-such-file\n", time.Minute)
	res, ok := out.(LandOutput)
	if err != nil || !ok || res.OK || res.Fixable != 0 || !strings.Contains(res.Error, "checks failed") {
		t.Fatalf("a check that fails by itself is the worker's to fix, as ever: %+v %v", out, err)
	}
}
