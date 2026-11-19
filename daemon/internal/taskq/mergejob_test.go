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

// builtRig is a rig with #1 built in the repos given, ready to merge.
func builtRig(t *testing.T, repos ...string) *rig {
	t.Helper()
	g := newRig(t)
	g.feature(1, 0, repos...)
	g.tick()
	g.doneParts(1)
	return g
}

// twoBuilt is a rig with #1 (in the repos given) and #2 (in admin) both
// built: #1 is first in line to merge.
func twoBuilt(t *testing.T, repos ...string) *rig {
	t.Helper()
	g := newRig(t)
	g.feature(1, 0, repos...)
	g.feature(2, 0, "admin")
	g.tick()
	g.doneParts(1)
	g.doneParts(2)
	return g
}

// restart is a new daemon over the same registry and jobs: everything the
// runner held in memory is gone.
func (g *rig) restart() {
	g.r = &Runner{Store: g.st, Host: g.host, Git: g.git, MergeGit: g.git, Jobs: g.jobs,
		Lock: g.r.Lock, Slots: g.r.Slots, Root: g.r.Root, Now: g.r.Now}
}

// A feature's merge is a job named after its task, with the parts kept on
// its row, and nothing of it runs in the runner's pass.
func TestMergeJob_IsStartedNotRun(t *testing.T) {
	g := builtRig(t, "app", "backend")
	g.r.Tick()
	job := g.jobs.the(MergeKind)
	in := g.jobs.inputs[job.ID].(LandInput)
	if job.Subject != "task:1" || !strings.HasPrefix(job.ID, "merge-task-1-") || in.Tag != "tasks" || in.PerLineSeconds != 1200 {
		t.Fatalf("the job is a task merge, named after its task, each check line with its 20 minutes: %+v %+v", job, in)
	}
	parts := g.partsOf(1)
	if len(in.Work) != 2 || in.Work[0].Worktree != parts[0].Worktree || in.Work[1].Branch != parts[1].Branch || in.Work[1].Label != "#1" {
		t.Fatalf("it lands every part's branch: %+v", in.Work)
	}
	ask, err := readAsk(job, func(a mergeAsk) int { return a.Format })
	if err != nil || ask.TaskID != 1 || len(ask.Parts) != 2 || ask.Parts[1].ID != parts[1].ID || ask.Parts[1].Worktree != parts[1].Worktree {
		t.Fatalf("its row keeps the parts as the merge found them: %+v %v", ask, err)
	}
	if got := g.st.task(1); got.Status != Merging || g.git.rebased != 0 || len(g.git.ffs) != 0 {
		t.Fatalf("the task reads as merging and nothing ran in this process: %+v, %d rebases", got, g.git.rebased)
	}
}

// The recover table on task rows: what a pass makes of a task that says
// merging, from what the registry says of its job. Only a plain "it has no
// open job" puts it back in line.
func TestMergeJob_TheRecoverTable(t *testing.T) {
	t.Run("its job still runs: left exactly as it is, by a new daemon too", func(t *testing.T) {
		g := builtRig(t, "app")
		g.r.Tick()
		g.restart()
		for i := 0; i < 3; i++ {
			g.r.Tick()
		}
		if got := g.st.task(1); got.Status != Merging || got.Note != "" {
			t.Fatalf("a task whose merge job runs stays merging: %+v", got)
		}
		if len(g.jobs.jobs) != 1 || g.git.rebased != 0 {
			t.Fatalf("no second job is started beside it, and nothing runs in the daemon: %d jobs", len(g.jobs.jobs))
		}
	})
	t.Run("its job ended while the daemon was down: the result is acted on, once", func(t *testing.T) {
		g := builtRig(t, "app")
		g.r.Tick()
		job := g.jobs.the(MergeKind)
		res := g.run(job) // the service ran to its end with no daemon there
		g.restart()
		g.r.Tick() // the manager has not looked yet: the row is not touched
		if got := g.st.task(1); got.Status != Merging {
			t.Fatalf("until its end is acted on the task stays merging: %+v", got)
		}
		if err := g.apply(job, res); err != nil {
			t.Fatal(err)
		}
		g.jobs.done(job.ID)
		g.runJobs()
		if got := g.st.task(1); got.Status != Done || got.Note != "merged app into dev" || len(g.git.ffs) != 1 {
			t.Fatalf("the merge that ran through the restart lands, once: %+v %v", got, g.git.ffs)
		}
	})
	t.Run("it has no job (the daemon stopped between the claim and the start): back in line", func(t *testing.T) {
		g := builtRig(t, "app")
		task := g.st.task(1)
		task.Status = Merging
		g.st.SaveTask(task)
		g.restart()
		g.r.Tick()
		if got := g.st.task(1); got.Status != Merging || len(g.jobs.open(MergeKind)) != 1 {
			t.Fatalf("it is put back in line and claimed again, now with a job: %+v, %d jobs", got, len(g.jobs.jobs))
		}
		g.runJobs()
		if got := g.st.task(1); got.Status != Done {
			t.Fatalf("and merges: %+v", got)
		}
	})
	t.Run("the jobs cannot be read: nothing is reset and nothing started", func(t *testing.T) {
		g := twoBuilt(t, "app")
		g.r.Tick() // #1 merges, #2 waits its turn
		g.jobs.openErr = errors.New("database is locked")
		g.restart()
		g.r.Tick()
		if one, two := g.st.task(1), g.st.task(2); one.Status != Merging || two.Status != ReadyToMerge || len(g.jobs.jobs) != 1 {
			t.Fatalf("\"I could not ask\" is never \"none runs\": #1 %s, #2 %s, %d jobs", one.Status, two.Status, len(g.jobs.jobs))
		}
	})
	t.Run("this daemon runs no jobs: a built task waits and says why, a merging one is not touched", func(t *testing.T) {
		g := twoBuilt(t, "app")
		two := g.st.task(2)
		two.Status = Merging
		g.st.SaveTask(two)
		g.r.Jobs, g.r.JobsOff = nil, "a scratch daemon that named no -job-unit-prefix of its own"
		g.r.Tick()
		one := g.st.task(1)
		if one.Status != ReadyToMerge || !strings.Contains(one.Note, "runs no jobs (a scratch daemon") || !strings.Contains(one.Note, "Mark merged") {
			t.Fatalf("a built task says why it does not merge here: %+v", one)
		}
		if got := g.st.task(2); got.Status != Merging || g.git.rebased != 0 {
			t.Fatalf("a merging task is left as it is, and nothing is merged in the daemon: %+v", got)
		}
	})
}

// The daemon is cut off after it acted on a failed merge and before the
// job was written down as done, so it acts on the same result again: the
// worker hears once, the automatic try is spent once, and a part that is
// back at work is not touched a second time.
func TestMergeJob_AFailureActedOnTwiceGoesBackOnce(t *testing.T) {
	g := builtRig(t, "app", "backend")
	g.checks.results["/p/backend"] = CheckResult{Configured: true, Output: "FAIL: TestExport"}
	g.r.Tick()
	job := g.jobs.the(MergeKind)
	res := g.run(job)
	for i := 0; i < 2; i++ {
		if err := g.apply(job, res); err != nil {
			t.Fatalf("acting on it, time %d: %v", i+1, err)
		}
	}
	task, app, backend := g.st.task(1), g.partsOf(1)[0], g.partsOf(1)[1]
	if task.Status != Running || task.AutoFixes != 1 || backend.Status != Running || app.Status != Ready {
		t.Fatalf("sent back once: task %+v, backend %s, app %s", task, backend.Status, app.Status)
	}
	if told := g.host.told[backend.PaneID]; len(told) != 1 || !strings.Contains(told[0], "TestExport") || g.host.tells != 1 {
		t.Fatalf("its worker heard once: %q (said %d times)", told, g.host.tells)
	}
	if len(g.st.notices) != 1 || g.st.notices[0].ID != job.ID+":part-2:send-back" {
		t.Fatalf("one notice, named after the job and the part: %+v", g.st.notices)
	}
	// And a third time, after the worker reported its fix and the feature
	// is built again: the old result must not fail the new build.
	g.doneParts(1)
	g.r.Lock.Lock()
	g.r.rollup(g.st.tasks, g.st.parts)
	g.r.Lock.Unlock()
	if err := g.apply(job, res); err != nil {
		t.Fatal(err)
	}
	if got := g.st.task(1); got.Status != ReadyToMerge || got.AutoFixes != 1 || len(g.st.notices) != 1 {
		t.Fatalf("a result that was acted on changes nothing later: %+v", got)
	}
}

// A merge that ended while the daemon was down is acted on in the daemon's
// first seconds, when no worker's Claude is back on the bus. That is not a
// failed send-back: the part goes back, the notice waits, and the worker
// hears once it is there.
func TestMergeJob_AFailureSettledBeforeAnyWorkerIsOnTheBus(t *testing.T) {
	g := builtRig(t, "app")
	g.checks.results["/p/app"] = CheckResult{Configured: true, Output: "FAIL: TestRoute"}
	g.r.Tick()
	job := g.jobs.the(MergeKind)
	res := g.run(job)
	g.restart()
	g.host.deaf = true
	g.host.prompted = func() int64 { return g.now.UnixMilli() }
	if err := g.apply(job, res); err != nil {
		t.Fatal(err)
	}
	g.jobs.done(job.ID)
	app := g.partsOf(1)[0]
	if task := g.st.task(1); task.Status != Running || task.AutoFixes != 1 || app.Status != Running || len(g.host.told[app.PaneID]) != 0 {
		t.Fatalf("with nobody on the bus the part still goes back: %+v, part %s", task, app.Status)
	}
	g.host.deaf = false
	g.tick()
	g.tick()
	if told := g.host.told[app.PaneID]; len(told) != 1 || !strings.Contains(told[0], "TestRoute") {
		t.Fatalf("its worker hears once it is on the bus, once: %q", told)
	}
	if task := g.st.task(1); task.Status != Running || task.AutoFixes != 1 {
		t.Fatalf("and the task is as it was: %+v", task)
	}
}

// A merge whose row cannot be written is not called settled: the handler
// says so, the task stays merging with its job open, and the next try
// settles it. Nothing is said or counted twice on the way.
func TestMergeJob_ARowThatCannotBeSavedIsTriedAgain(t *testing.T) {
	g := builtRig(t, "app")
	g.r.Tick()
	job := g.jobs.the(MergeKind)
	res := g.run(job)
	g.st.tasksErr = errors.New("database is locked")
	if err := g.apply(job, res); err == nil {
		t.Fatal("a merge whose row could not be read was called settled")
	}
	g.st.tasksErr = nil
	if got := g.st.task(1); got.Status != Merging || len(g.jobs.open(TidyKind)) != 0 {
		t.Fatalf("nothing happened yet: %+v", got)
	}
	g.r.Tick()
	if got := g.st.task(1); got.Status != Merging || len(g.jobs.jobs) != 1 {
		t.Fatalf("a pass meanwhile leaves it alone, its job is still open: %+v", got)
	}
	g.runJobs()
	if got := g.st.task(1); got.Status != Done || got.Note != "merged app into dev" {
		t.Fatalf("the next try settles it: %+v", got)
	}
}

// A job that does not start is said on the task, for a person, and not
// tried again on every pass.
func TestMergeJob_AJobThatDoesNotStartWaitsForAPerson(t *testing.T) {
	g := builtRig(t, "app")
	g.jobs.startErr[MergeKind] = errors.New("systemctl start: unit not found")
	for i := 0; i < 3; i++ {
		g.r.Tick()
	}
	got := g.st.task(1)
	if got.Status != MergeFailed || !strings.Contains(got.Note, "the merge did not start: systemctl start: unit not found") || got.AutoFixes != 0 {
		t.Fatalf("a merge that did not start: %+v", got)
	}
	if len(g.host.told) != 0 || g.host.changed == 0 {
		t.Fatalf("no worker is sent to fix it, and the lenses are told: told %v, changed %d", g.host.told, g.host.changed)
	}
}

// A merge whose job is lost (it died as often as it is started again) or
// was stopped waits for a person, and its note says which trunks had moved:
// a merge that died between two trunks is half in.
func TestMergeJob_LostSaysWhatHadLanded(t *testing.T) {
	g := builtRig(t, "app", "backend")
	g.git.ffErr["/p/backend"] = errors.New("killed")
	cleared := len(g.host.cleared)
	g.r.Tick()
	job := g.jobs.the(MergeKind)
	g.run(job) // moved app's trunk, died at backend's, left no result
	if err := g.r.MergeHandler().Lost(job, errors.New("it ended 3 times without a result (systemd's word for the last: signal)")); err != nil {
		t.Fatal(err)
	}
	got := g.st.task(1)
	if got.Status != MergeFailed || !strings.Contains(got.Note, "its merge did not finish: it ended 3 times without a result") ||
		!strings.Contains(got.Note, "Already merged, and staying so: app into dev.") || !strings.Contains(got.Note, "journalctl --user -u ccmux-job-test@"+job.ID) {
		t.Fatalf("a lost merge: %+v", got)
	}
	if len(g.host.told) != 0 || len(g.host.cleared) != cleared {
		t.Fatalf("no worker is told or closed: told %v, cleared %v", g.host.told, g.host.cleared)
	}

	// A result in a format this daemon does not read: nothing is assumed.
	g2 := builtRig(t, "app")
	g2.r.Tick()
	job2 := g2.jobs.the(MergeKind)
	if err := g2.r.MergeHandler().Lost(job2, jobs.VersionError{File: "result.json", Have: 2, Want: 1}); err != nil {
		t.Fatal(err)
	}
	if got := g2.st.task(1); got.Status != MergeFailed || !strings.Contains(got.Note, "cannot read how") || !strings.Contains(got.Note, "It may have landed") {
		t.Fatalf("an unreadable result: %+v", got)
	}
	// What was kept on the job's row is in another format: the same.
	g3 := builtRig(t, "app")
	g3.r.Tick()
	job3 := g3.jobs.the(MergeKind)
	res := g3.run(job3)
	job3.Ctx = `{"format":7,"taskId":1}`
	if err := g3.apply(job3, res); err != nil {
		t.Fatal(err)
	}
	if got := g3.st.task(1); got.Status != MergeFailed || !strings.Contains(got.Note, "format 7") {
		t.Fatalf("an unreadable row is never settled as merged, and never left merging: %+v", got)
	}
}

// The tidy-up is a job of its own, started once: the daemon closes the
// workers, the job removes the worktrees, and the note says the tidy-up is
// not finished until the job's end is acted on. Meanwhile the merge line
// stays taken.
func TestTidyJob_StartsOnceAndHoldsTheMergeLine(t *testing.T) {
	g := twoBuilt(t, "app", "backend")
	cleared := len(g.host.cleared)
	g.r.Tick()
	job := g.jobs.the(MergeKind)
	res := g.run(job)
	// Cut off right after the task was saved as done: the first try fails
	// where it asks whether a tidy-up runs already.
	g.jobs.openErr = errors.New("database is locked")
	if err := g.apply(job, res); err == nil {
		t.Fatal("a merge whose tidy-up was not started was called settled")
	}
	g.jobs.openErr = nil
	if got := g.st.task(1); got.Status != Done || got.Note != "merged app into dev, backend into dev; tidy-up not finished" {
		t.Fatalf("merged work is marked as such before the tidy-up: %+v", got)
	}
	for i := 0; i < 2; i++ { // the next try, and one more after a cut that lost nothing
		if err := g.apply(job, res); err != nil {
			t.Fatal(err)
		}
	}
	g.jobs.done(job.ID)
	tidy := g.jobs.the(TidyKind)
	in := g.jobs.inputs[tidy.ID].(TidyInput)
	if tidy.Subject != "task-tidy:1" || len(in.Work) != 2 || len(g.git.removed) != 0 {
		t.Fatalf("one tidy-up job for both worktrees, and nothing removed in the daemon: %+v %+v", tidy, in)
	}
	if len(g.host.cleared)-cleared != 2 {
		t.Fatalf("the daemon closed each part's worker once: %v", g.host.cleared[cleared:])
	}
	g.r.Tick()
	if got := g.st.task(2); got.Status != ReadyToMerge || len(g.jobs.open(MergeKind)) != 0 {
		t.Fatalf("the next feature waits while the tidy-up runs: %+v", got)
	}
	g.runJobs()
	if got := g.st.task(1); got.Note != "merged app into dev, backend into dev" || len(g.git.removed) != 2 {
		t.Fatalf("the tidy-up's end takes the words away: %+v, removed %v", got, g.git.removed)
	}
	g.tick()
	if got := g.st.task(2); got.Status != Done {
		t.Fatalf("and the line is free: %+v", got)
	}
}

// A tidy-up job that is killed is run again: a worktree that is gone is not
// removed a second time, one that stayed is tried again, and acting on its
// end twice changes nothing.
func TestTidyJob_RunAgainSkipsWhatIsGone(t *testing.T) {
	g := builtRig(t, "app", "backend")
	g.r.Tick()
	merge := g.jobs.the(MergeKind)
	if err := g.apply(merge, g.run(merge)); err != nil {
		t.Fatal(err)
	}
	g.jobs.done(merge.ID)
	tidy := g.jobs.the(TidyKind)
	g.git.rmErr["/p/backend"] = errors.New("left half removed: git timed out")
	g.run(tidy) // killed before its result was written
	if len(g.git.removed) != 1 || !strings.Contains(g.git.removed[0], "app-") {
		t.Fatalf("the first run removed app's worktree: %v", g.git.removed)
	}
	if got := g.st.task(1); !strings.HasSuffix(got.Note, noteTidying) {
		t.Fatalf("a tidy-up that was cut off still reads as not finished: %+v", got)
	}
	delete(g.git.rmErr, "/p/backend")
	res := g.run(tidy)
	if len(g.git.removed) != 2 || !strings.Contains(g.git.removed[1], "backend-") {
		t.Fatalf("the second run removes only what the first left: %v", g.git.removed)
	}
	for i := 0; i < 2; i++ {
		if err := g.apply(tidy, res); err != nil {
			t.Fatal(err)
		}
	}
	if got := g.st.task(1); got.Note != "merged app into dev, backend into dev" {
		t.Fatalf("its end, acted on twice: %+v", got)
	}
}

// What a tidy-up could not do stays said: a job that is lost, one that did
// not start, and a task that was removed from the list meanwhile.
func TestTidyJob_WhatDidNotFinishStaysSaid(t *testing.T) {
	landed := func(t *testing.T, g *rig) {
		t.Helper()
		g.r.Tick()
		merge := g.jobs.the(MergeKind)
		if err := g.apply(merge, g.run(merge)); err != nil {
			t.Fatal(err)
		}
		g.jobs.done(merge.ID)
	}
	t.Run("lost", func(t *testing.T) {
		g := builtRig(t, "app")
		landed(t, g)
		tidy := g.jobs.the(TidyKind)
		if err := g.r.TidyHandler().Lost(tidy, errors.New("it ended 3 times without a result")); err != nil {
			t.Fatal(err)
		}
		got := g.st.task(1)
		if got.Status != Done || !strings.HasPrefix(got.Note, "merged app into dev; tidy-up not finished: it ended 3 times without a result (journalctl --user -u ccmux-job-test@"+tidy.ID) {
			t.Fatalf("a lost tidy-up: %+v", got)
		}
	})
	t.Run("did not start", func(t *testing.T) {
		g := builtRig(t, "app")
		g.jobs.startErr[TidyKind] = errors.New("systemctl start: unit not found")
		g.tick()
		got := g.st.task(1)
		if got.Status != Done || !strings.Contains(got.Note, "tidy-up not finished: it did not start (systemctl start: unit not found); left for you: "+g.partsOf(1)[0].Worktree) {
			t.Fatalf("a tidy-up that did not start names the worktree it leaves: %+v", got)
		}
		g.tick()
		if len(g.jobs.jobs) != 1 {
			t.Fatalf("and is not tried again on every pass: %d jobs", len(g.jobs.jobs))
		}
	})
	t.Run("the task was removed from the list", func(t *testing.T) {
		g := builtRig(t, "app")
		g.feature(2, 0, "backend")
		landed(t, g)
		g.st.tasks = g.st.tasks[1:] // a done task can be removed while its tidy-up runs
		before := g.st.task(2)
		g.runJobs()
		if len(g.git.removed) != 1 {
			t.Fatalf("its worktree still goes: %v", g.git.removed)
		}
		if got := g.st.task(2); got.Note != before.Note || got.Status != before.Status || len(g.st.tasks) != 1 {
			t.Fatalf("and no other task is touched or put back: %+v", g.st.tasks)
		}
	})
}

// A thread's merge and a queued task's merge are the same landing job under
// the same lock per repo: while one of them holds a repo's lock, the other
// waits, says so in its journal, and lands only after.
func TestMergeJob_SharesTheRepoLockWithAThreadsMerge(t *testing.T) {
	repo := gitRepo(t)
	os.MkdirAll(filepath.Join(repo, ".claude"), 0o755)
	commitFile(t, repo, ".claude/qa-gates", "test -f feature.txt\n")
	wt := filepath.Join(t.TempDir(), "task-1")
	if err := (ExecGit{}).AddWorktree(repo, wt, "task/1-x", "main"); err != nil {
		t.Fatal(err)
	}
	commitFile(t, wt, "feature.txt", "new")
	dir := t.TempDir()
	locks := filepath.Join(dir, "locks")
	// Another landing job (a thread's merge) works in the repo.
	held, err := jobs.LockRepos(context.Background(), locks, []string{repo}, nil)
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(dir, "journal.jsonl")
	j, err := jobs.OpenJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(LandInput{Work: []Work{{RepoPath: repo, Worktree: wt, Branch: "task/1-x", Label: "#1"}}, Tag: "tasks", PerLineSeconds: 60})
	done := make(chan LandOutput, 1)
	go func() {
		out, err := RunLand(&jobs.Run{Spec: jobs.Spec{ID: "merge-task-1", Kind: MergeKind, Input: input, Locks: locks}, Journal: j, Ctx: context.Background()})
		if err != nil {
			t.Error(err)
		}
		res, _ := out.(LandOutput)
		done <- res
	}()
	waiting := func() bool {
		steps, _ := jobs.ReadJournal(journal)
		return strings.Contains(jobs.Doing(steps), "waiting for another merge of "+filepath.Base(repo))
	}
	for deadline := time.Now().Add(10 * time.Second); !waiting(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the task's merge never said it waits for the repo's lock")
		}
	}
	select {
	case out := <-done:
		t.Fatalf("the task's merge ran while another job held the repo: %+v", out)
	case <-time.After(300 * time.Millisecond):
	}
	if head, _ := runGit(repo, "log", "--format=%s", "-1", "main"); head != ".claude/qa-gates" {
		t.Fatalf("the trunk moved under the other job: %q", head)
	}
	held.Release()
	select {
	case out := <-done:
		if !out.OK {
			t.Fatalf("with the repo free it lands: %+v", out)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the task's merge did not go on once the lock was free")
	}
}

// The tidy-up job itself, over real git: it removes a landed worktree, its
// branch and its brief under the repo's lock, and asked again it has
// nothing left to do.
func TestTheTidyJobRemovesALandedWorktreeAndCanRunAgain(t *testing.T) {
	repo := gitRepo(t)
	wt := filepath.Join(t.TempDir(), "task-1")
	g := ExecGit{}
	if err := g.AddWorktree(repo, wt, "task/1-x", "main"); err != nil {
		t.Fatal(err)
	}
	commitFile(t, wt, "feature.txt", "new")
	rev, _ := runGit(wt, "rev-parse", "HEAD")
	if err := g.FastForward(repo, rev, "main"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(wt+".md", []byte("the brief"), 0o644)
	os.WriteFile(wt+".flaky.log", []byte("a flake"), 0o644)
	dir := t.TempDir()
	input, _ := json.Marshal(TidyInput{Work: []Work{{RepoPath: repo, Worktree: wt, Branch: "task/1-x", Label: "#1"}}, Tag: "tasks"})
	run := func() TidyOutput {
		t.Helper()
		j, err := jobs.OpenJournal(filepath.Join(dir, "journal.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		out, err := RunTidy(&jobs.Run{Spec: jobs.Spec{ID: "tidy-task-1", Input: input, Locks: filepath.Join(dir, "locks")}, Journal: j, Ctx: context.Background()})
		if err != nil {
			t.Fatal(err)
		}
		return out.(TidyOutput)
	}
	if out := run(); len(out.Left) != 0 {
		t.Fatalf("a clean landed worktree goes whole: %+v", out)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) || hasBranch(t, repo, "task/1-x") {
		t.Fatal("the worktree and its merged branch are gone")
	}
	if _, err := os.Stat(wt + ".md"); !os.IsNotExist(err) {
		t.Fatal("the brief is gone")
	}
	if _, err := os.Stat(wt + ".flaky.log"); err != nil {
		t.Fatalf("what the flaky checks printed stays: %v", err)
	}
	if _, err := os.Stat(jobs.LockFile(filepath.Join(dir, "locks"), repo)); err != nil {
		t.Fatalf("the job held the repo's lock file: %v", err)
	}
	if out := run(); len(out.Left) != 0 {
		t.Fatalf("asked again, it has nothing to do and nothing to say: %+v", out)
	}
}
