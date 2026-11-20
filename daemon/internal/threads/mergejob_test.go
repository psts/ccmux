package threads

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// asksMerge starts a merge whose job the test ends by hand, and returns the
// job with the result its landing gives.
func (r *rig) asksMerge(th store.Thread, by string) (store.Job, jobs.Result) {
	r.t.Helper()
	r.jobs.manual = true
	if _, err := r.svc.Merge(th.ID, by); err != nil {
		r.t.Fatal(err)
	}
	job := r.jobs.job(th.ID)
	in := r.jobs.started[len(r.jobs.started)-1].Input.(taskq.LandInput)
	return job, r.jobs.land(job, in)
}

func (r *rig) removed() string {
	return strings.Join(r.git.did(func(g *fakeGit) []string { return g.removed }), "|")
}

func TestAMergeIsAJobWithTheTeamKeptOnItsRow(t *testing.T) {
	r, th, lead, backend, admin := team(t)
	r.jobs.manual = true
	if _, err := r.svc.Merge(th.ID, ByPerson); err != nil {
		t.Fatal(err)
	}
	n := r.jobs.started[0]
	in := n.Input.(taskq.LandInput)
	if n.Kind != MergeKind || n.Subject != "thread:1" || n.Name != "merge-thread-1" || n.Limit != mergeLimit {
		t.Fatalf("the job is a thread merge, named after its thread, with a time limit: %+v", n)
	}
	if len(in.Work) != 2 || in.Work[0].Worktree != lead.Worktree || in.Work[1].Branch != backend.Branch || in.Tag != "threads" || in.PerLineSeconds != 1200 {
		t.Fatalf("it lands the branches of the members that changed something, each check line with its 20 minutes: %+v", in)
	}
	ask, err := readAsk(r.jobs.job(th.ID))
	if err != nil || ask.By != ByPerson || len(ask.Members) != 3 || ask.lead().ID != lead.ID || ask.Members[2].ID != admin.ID {
		t.Fatalf("the row keeps who asked and the whole team, the one that only read too: %+v %v", ask, err)
	}
	if got := r.state(th.ID); got.State != store.ThreadMerging {
		t.Fatalf("the thread reads as merging: %+v", got)
	}
	if said := r.sess.prompts(lead.ID); len(said) != 0 || len(r.git.did(func(g *fakeGit) []string { return g.landed })) != 0 {
		t.Fatalf("and nothing of it ran in this process: %v", said)
	}
	if got := r.rereads(); len(got) != 1 || got[0] != "ws-app" {
		t.Fatalf("the lead's repo's page is told to read the thread again, by its workspace: %v", got)
	}
	// And again when the merge has ended, though what was kept of the team
	// names no workspace.
	job := r.jobs.job(th.ID)
	if err := r.svc.MergeHandler().Apply(job, r.jobs.land(job, in)); err != nil {
		t.Fatal(err)
	}
	if got := r.rereads(); len(got) != 2 || got[1] != "ws-app" {
		t.Fatalf("the end of the merge is said to the same page: %v", got)
	}
}

// The daemon restarts while a merge's job runs: the thread is left exactly
// as it is, and nobody is told anything.
func TestAMergeThatOutlivesTheDaemonIsLeftAlone(t *testing.T) {
	r, th, lead, _, _ := team(t)
	job, res := r.asksMerge(th, ByLead)
	r.svc.members = nil // a fresh daemon
	r.svc.Recover()
	if got := r.state(th.ID); got.State != store.ThreadMerging || got.Note != "" {
		t.Fatalf("a thread whose merge job still runs stays merging: %+v", got)
	}
	if said := r.sess.prompts(lead.ID); len(said) != 0 {
		t.Fatalf("its lead is not told to ask again: %v", said)
	}
	if _, err := r.svc.Merge(th.ID, ByLead); err == nil || !strings.Contains(err.Error(), "being merged already") {
		t.Fatalf("and a second merge is still refused: %v", err)
	}
	// When the job ends, under the new daemon, the merge is settled as ever.
	if err := r.svc.MergeHandler().Apply(job, res); err != nil {
		t.Fatal(err)
	}
	if got := r.state(th.ID); got.State != store.ThreadMerged {
		t.Fatalf("the merge that ran through the restart lands: %+v", got)
	}
	if said := r.sess.prompts(lead.ID); len(said) != 1 || !strings.Contains(said[0], "merged app into main, backend into main") {
		t.Fatalf("and the lead hears once: %v", said)
	}
}

// The daemon is cut off after it settled a merge and before it wrote the
// job down as done, so it settles the same job again: the lead hears once,
// is restarted once, and a thread it has opened again since is not touched.
func TestSettlingTheSameMergeTwiceDoesEverythingOnce(t *testing.T) {
	r, th, lead, _, _ := team(t)
	job, res := r.asksMerge(th, ByLead)
	h := r.svc.MergeHandler()
	if err := h.Apply(job, res); err != nil {
		t.Fatal(err)
	}
	moves := len(r.sess.moves())
	if r.removed() != "backend|app" {
		t.Fatalf("the worktrees went: %s", r.removed())
	}
	// The lead reads the message and starts new work at once, with a new
	// helper for it.
	again := r.edits(lead.ID)
	moves++
	if again.Worktree == "" || r.state(th.ID).State != store.ThreadOpen {
		t.Fatalf("the rig: the lead has new work: %+v", again)
	}
	next := r.edits(r.helper(th, "admin").ID)
	moves++
	if err := h.Apply(job, res); err != nil {
		t.Fatal(err)
	}
	if said := r.sess.prompts(lead.ID); len(said) != 1 {
		t.Fatalf("the lead is told once: %v", said)
	}
	if len(r.sess.moves()) != moves || r.removed() != "backend|app" {
		t.Fatalf("it is not restarted again, and nothing is removed again: %v %s", r.sess.moves(), r.removed())
	}
	if got := r.row(lead.ID); got.Worktree != again.Worktree || got.Cwd != again.Worktree || got.ReadOnly {
		t.Fatalf("its new worktree is its own: %+v", got)
	}
	if got := r.state(th.ID); got.State != store.ThreadOpen {
		t.Fatalf("and the thread it opened again stays open: %+v", got)
	}
	if got := r.row(next.ID); got.Wanted != store.SessionRunning || got.Worktree != next.Worktree {
		t.Fatalf("a helper stood up since the merge is not this merge's to stand down: %+v", got)
	}
}

// What used to strand a worktree: the daemon stopped between ending a
// helper (its row says done, so it is in no list any more) and removing its
// worktree, or between moving the lead out and removing the lead's. The
// tidy-up run again works from the team as the merge found it.
func TestATidyUpCutOffHalfWayStrandsNoWorktree(t *testing.T) {
	r, th, lead, backend, _ := team(t)
	job, res := r.asksMerge(th, ByLead)
	// The first try got this far and died, with nothing written down:
	r.sess.SetWanted(backend.ID, store.SessionDone)
	r.sess.Place(lead.ID, store.SessionPlace{Cwd: lead.Repo, ReadOnly: true})
	if err := r.svc.MergeHandler().Apply(job, res); err != nil {
		t.Fatal(err)
	}
	if r.removed() != "backend|app" {
		t.Fatalf("both worktrees are removed, though no row names them any more: %q", r.removed())
	}
	if got := r.row(lead.ID); got.Cwd != r.repo("app") || !got.ReadOnly || got.Worktree != "" {
		t.Fatalf("the lead is in its repo's folder, reading: %+v", got)
	}
	if got := r.state(th.ID); got.State != store.ThreadMerged || strings.Contains(got.Note, "left for you") {
		t.Fatalf("the thread is merged with nothing left over: %+v", got)
	}
}

// The order of the lead's way home: its row names the worktree until the
// worktree is gone.
func TestTheLeadsRowNamesItsWorktreeUntilTheWorktreeIsGone(t *testing.T) {
	r, th, lead, _, _ := team(t)
	job, res := r.asksMerge(th, ByLead)
	r.git.set(func(g *fakeGit) { g.kept = map[string]string{"app": "kept, it holds uncommitted files"} })
	r.sess.failMoves(errors.New("systemd refused"))
	if err := r.svc.MergeHandler().Apply(job, res); err != nil {
		t.Fatal(err)
	}
	if got := r.row(lead.ID); got.Worktree != lead.Worktree || got.Branch != lead.Branch {
		t.Fatalf("a lead that could not be restarted out of its worktree still has it on its row: %+v", got)
	}
	if r.removed() != "backend" {
		t.Fatalf("and the worktree it may still be in is not removed: %q", r.removed())
	}
	if said := r.sess.prompts(lead.ID); len(said) != 1 || !strings.Contains(said[0], "the lead was not restarted out of it") {
		t.Fatalf("the lead is told what stayed: %v", said)
	}
}

func TestAMergeThatEndedWithNoResultOpensTheThreadAndSaysWhatLanded(t *testing.T) {
	r, th, lead, backend, _ := team(t)
	job, _ := r.asksMerge(th, ByLead)
	// Its journal says one trunk had moved when it died for good.
	j, err := jobs.OpenJournal(filepath.Join(r.jobs.dir, job.ID, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	j.Record("landed:"+r.repo("app")+"@rev-app", "app into main")
	h := r.svc.MergeHandler()
	for range 2 { // settled twice, as after a restart
		if err := h.Lost(job, errors.New("it ended 3 times without a result (systemd's word for the last: signal)")); err != nil {
			t.Fatal(err)
		}
	}
	got := r.state(th.ID)
	if got.State != store.ThreadOpen || !strings.Contains(got.Note, "merge did not finish: it ended 3 times") || !strings.Contains(got.Note, "already merged: app into main") {
		t.Fatalf("the thread is open and says why, and which half is in: %+v", got)
	}
	said := r.sess.prompts(lead.ID)
	if len(said) != 1 {
		t.Fatalf("the lead is told once: %v", said)
	}
	for _, want := range []string{"did not finish", "Already merged, and staying so: app into main", "journalctl --user -u ccmux-job-test@" + job.ID, "team as it was"} {
		if !strings.Contains(said[0], want) {
			t.Fatalf("the lead is told %q: %s", want, said[0])
		}
	}
	if r.row(backend.ID).Wanted != store.SessionRunning || r.removed() != "" || r.row(lead.ID).Worktree != lead.Worktree {
		t.Fatal("nothing of the team is touched")
	}
}

func TestAMergeCanBeStoppedByThePersonOrTheLead(t *testing.T) {
	r, th, lead, _, _ := team(t)
	if _, err := r.svc.StopMerge(th.ID, ByPerson); err == nil || !strings.Contains(err.Error(), "is not being merged") {
		t.Fatalf("there is nothing to stop before a merge: %v", err)
	}
	job, _ := r.asksMerge(th, ByLead)
	said, err := r.svc.StopMerge(th.ID, ByPerson)
	if err != nil || !strings.Contains(said, "Stopping the merge of thread #1") {
		t.Fatalf("the stop is asked for: %q %v", said, err)
	}
	if len(r.jobs.stopped) != 1 || r.jobs.stopped[0] != job.ID {
		t.Fatalf("the job's service is told to stop: %v", r.jobs.stopped)
	}
	if _, err := r.svc.StopMerge(th.ID, ByLead); err == nil || !strings.Contains(err.Error(), "being stopped already") {
		t.Fatalf("asked twice, the second is told so: %v", err)
	}
	if got := r.state(th.ID); got.State != store.ThreadMerging {
		t.Fatalf("until its processes are gone the thread still merges: %+v", got)
	}
	// The service is gone: the manager says so.
	if err := r.svc.MergeHandler().Stopped(r.jobs.job(th.ID)); err != nil {
		t.Fatal(err)
	}
	if got := r.state(th.ID); got.State != store.ThreadOpen || got.Note != "merge stopped" {
		t.Fatalf("the thread is open again: %+v", got)
	}
	told := r.sess.prompts(lead.ID)
	if len(told) != 1 || !strings.Contains(told[0], "was stopped by a person, on the thread's page") || !strings.Contains(told[0], "Nothing had landed") {
		t.Fatalf("the lead is told who stopped it and that nothing landed: %v", told)
	}
	// The lead stops its own through the tool.
	r.jobs.done(r.jobs.job(th.ID), store.JobStopped)
	r.asksMerge(th, ByLead)
	if _, err := r.svc.Tool(lead.ID, "stop_merge", ""); err != nil {
		t.Fatal(err)
	}
	r.svc.MergeHandler().Stopped(r.jobs.job(th.ID))
	if told := r.sess.prompts(lead.ID); len(told) != 2 || !strings.Contains(told[1], "was stopped as you asked") {
		t.Fatalf("a lead that stopped its own merge is told it is done: %v", told)
	}
}

// A job started by another version of ccmuxd: what was kept of it, or its
// result, is in a format this one does not read. Nothing is guessed.
func TestAMergeThisDaemonCannotReadIsSaidLoudlyAndNotGuessed(t *testing.T) {
	r, th, lead, backend, _ := team(t)
	job, res := r.asksMerge(th, ByLead)
	other := job
	other.Ctx = strings.Replace(job.Ctx, `"format":1`, `"format":2`, 1)
	if other.Ctx == job.Ctx {
		t.Fatalf("the rig: the kept team carries its format: %s", job.Ctx)
	}
	if err := r.svc.MergeHandler().Apply(other, res); err != nil {
		t.Fatal(err)
	}
	got := r.state(th.ID)
	if got.State != store.ThreadOpen || !strings.Contains(got.Note, "could not be read") {
		t.Fatalf("the thread is open and says its merge's end was not read: %+v", got)
	}
	said := r.sess.prompts(lead.ID)
	if len(said) != 1 || !strings.Contains(said[0], "format 2") || !strings.Contains(said[0], "It may have landed, or half of it") {
		t.Fatalf("the lead is told to look, not that it landed or failed: %v", said)
	}
	if r.row(backend.ID).Wanted != store.SessionRunning || r.removed() != "" {
		t.Fatal("and the team is not taken apart on a guess")
	}
	// The same for a result file in another format, which the job manager
	// cannot even hand over: it is not called a merge that did not finish.
	r.jobs.done(r.jobs.job(th.ID), store.JobApplied)
	job, _ = r.asksMerge(th, ByLead)
	if err := r.svc.MergeHandler().Lost(job, jobs.VersionError{File: "result.json", Have: 2, Want: 1}); err != nil {
		t.Fatal(err)
	}
	said = r.sess.prompts(lead.ID)
	if got := r.state(th.ID); got.State != store.ThreadOpen || len(said) != 2 || !strings.Contains(said[1], "It may have landed, or half of it") || strings.Contains(said[1], "did not finish") {
		t.Fatalf("a result in another format is not guessed at either: %+v %v", got, said)
	}
	// The same for a result that is not a landing's.
	r.jobs.done(r.jobs.job(th.ID), store.JobApplied)
	job, _ = r.asksMerge(th, ByLead)
	if err := r.svc.MergeHandler().Apply(job, jobs.Result{Output: json.RawMessage(`{"something":"else"}`)}); err != nil {
		t.Fatal(err)
	}
	if got := r.state(th.ID); got.State != store.ThreadOpen || len(r.sess.prompts(lead.ID)) != 3 {
		t.Fatalf("a result that says neither landed nor why not is not taken for either: %+v", got)
	}
}

// A job that could not do its work at all (it ran out of time, its spec
// was in another format) is a failed merge, with the job's own words.
func TestAJobsOwnErrorIsAFailedMerge(t *testing.T) {
	r, th, lead, _, _ := team(t)
	job, _ := r.asksMerge(th, ByLead)
	if err := r.svc.MergeHandler().Apply(job, jobs.Result{Error: "it ran past its time limit of 3h0m0s and was stopped"}); err != nil {
		t.Fatal(err)
	}
	if got := r.state(th.ID); got.State != store.ThreadOpen || got.Note != "merge failed: it ran past its time limit of 3h0m0s and was stopped" {
		t.Fatalf("the thread is open and says why: %+v", got)
	}
	if said := r.sess.prompts(lead.ID); len(said) != 1 || !strings.Contains(said[0], "past its time limit") || !strings.Contains(said[0], "Fix it, then ask for the merge again") {
		t.Fatalf("the lead is told: %v", said)
	}
}

func TestALeadThatWasNotToldIsToldOnTheNextTry(t *testing.T) {
	r, th, lead, _, _ := team(t)
	job, res := r.asksMerge(th, ByLead)
	r.sess.sendErr = errors.New("database is locked")
	if err := r.svc.MergeHandler().Apply(job, res); err == nil {
		t.Fatal("a lead that could not be told leaves the job to be settled again")
	}
	r.sess.sendErr = nil
	if err := r.svc.MergeHandler().Apply(job, res); err != nil {
		t.Fatal(err)
	}
	if said := r.sess.prompts(lead.ID); len(said) != 1 || !strings.Contains(said[0], "merged app into main") {
		t.Fatalf("the second try tells it, once: %v", said)
	}
	if r.removed() != "backend|app" {
		t.Fatalf("and the tidy-up was not done twice: %s", r.removed())
	}
}

func TestAMergeThatCannotStartLeavesTheThreadOpenAndSaysWhy(t *testing.T) {
	r, th, _, _, _ := team(t)
	r.jobs.startErr = errors.New("the job's service did not start: unit not found")
	if _, err := r.svc.Merge(th.ID, ByLead); err == nil || !strings.Contains(err.Error(), "the merge did not start") {
		t.Fatalf("the asker is told: %v", err)
	}
	if got := r.state(th.ID); got.State != store.ThreadOpen || !strings.Contains(got.Note, "did not start") {
		t.Fatalf("the thread is open: %+v", got)
	}
	// A daemon that runs no jobs at all says so.
	r.svc.Jobs, r.svc.JobsOff = nil, "a scratch daemon that named no -job-unit-prefix"
	if _, err := r.svc.Merge(th.ID, ByLead); err == nil || !strings.Contains(err.Error(), "runs no jobs (a scratch daemon") {
		t.Fatalf("no jobs, no merge, and why: %v", err)
	}
	if _, err := r.svc.Detail(th.ID); err != nil {
		t.Fatalf("its page still reads: %v", err)
	}
}

func TestTheThreadsPageSeesItsMergeRunningAndWhereAFailedOnesOutputIs(t *testing.T) {
	r, th, _, backend, _ := team(t)
	if d, _ := r.svc.Detail(th.ID); d.Merge != nil {
		t.Fatalf("no merge yet, nothing to show: %+v", d.Merge)
	}
	job, _ := r.asksMerge(th, ByLead)
	j, _ := jobs.OpenJournal(filepath.Join(r.jobs.dir, job.ID, "journal.jsonl"))
	j.Begin("checked:x@rev", "backend: checks running")
	d, err := r.svc.Detail(th.ID)
	if err != nil || d.Merge == nil || d.Merge.State != "running" || d.Merge.Doing != "backend: checks running" || d.Merge.Job != job.ID || d.Merge.Unit != "ccmux-job-test@"+job.ID+".service" || d.Merge.StartedAt == 0 {
		t.Fatalf("a running merge shows what it is doing, since when, and where its log is: %+v %v", d.Merge, err)
	}
	if said, _ := r.svc.Describe(th.ID); !strings.Contains(said, "Its merge is running: backend: checks running") {
		t.Fatalf("the lead reads the same from the tool: %s", said)
	}
	// It fails on backend's checks, which left their output in a file.
	log := backend.Worktree + ".check.log"
	raw, _ := json.Marshal(taskq.LandOutput{Error: "backend: checks failed:\nFAIL\n(the whole output is in " + log + ")", Fixable: 1, CheckLog: log})
	res := jobs.Result{Output: raw}
	r.jobs.mu.Lock()
	r.jobs.results = map[string]jobs.Result{job.ID: res}
	r.jobs.mu.Unlock()
	r.jobs.done(job, store.JobApplied)
	if err := r.svc.MergeHandler().Apply(job, res); err != nil {
		t.Fatal(err)
	}
	d, _ = r.svc.Detail(th.ID)
	if d.Merge == nil || d.Merge.State != "failed" || d.Merge.CheckLog != log || d.Merge.Running() {
		t.Fatalf("a failed merge shows where its check's whole output is: %+v", d.Merge)
	}
	if _, err := os.Stat(filepath.Join(r.jobs.dir, job.ID, "applied.jsonl")); err != nil {
		t.Fatalf("what the daemon did about the job is written beside it: %v", err)
	}
}

// One merge of a thread at a time, by its job as well as by its row; and
// when the jobs cannot be read, the asker hears that, not "wait for the
// message" for a merge that is not running.
func TestASecondMergeIsRefusedByTheJobAndAnUnreadableOneIsSaidAsThat(t *testing.T) {
	r, th, lead, _, _ := team(t)
	r.jobs.manual = true
	r.jobs.openErr = errors.New("database is locked")
	_, err := r.svc.Merge(th.ID, ByLead)
	if err == nil || !strings.Contains(err.Error(), "could not be read") || !strings.Contains(err.Error(), "database is locked") || strings.Contains(err.Error(), "being merged already") {
		t.Fatalf("jobs that cannot be read are said as that: %v", err)
	}
	if got := r.state(th.ID); got.State != store.ThreadOpen || len(r.jobs.started) != 0 {
		t.Fatalf("nothing was started and the thread is as it was: %+v, %d jobs", got, len(r.jobs.started))
	}
	// Readable again: the merge starts.
	r.jobs.openErr = nil
	if _, err := r.svc.Merge(th.ID, ByLead); err != nil || len(r.jobs.started) != 1 {
		t.Fatalf("with the jobs readable the same ask starts the merge: %v", err)
	}
	// Its row is put back to open behind its back (an old daemon's reset,
	// say) while the job still runs: the job alone still refuses a second.
	open := r.state(th.ID)
	open.State = store.ThreadOpen
	if err := r.st.SaveThread(open); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Merge(th.ID, ByLead); err == nil || !strings.Contains(err.Error(), "being merged already") {
		t.Fatalf("a job that has not ended refuses a second merge, whatever the row says: %v", err)
	}
	if len(r.jobs.started) != 1 || len(r.sess.prompts(lead.ID)) != 0 {
		t.Fatalf("no second job, and nobody was told anything: %d jobs", len(r.jobs.started))
	}
}

// A merge that failed after it had moved a trunk (it was cut off between
// two repos, run again, and the second repo then failed): the lead must
// learn which half is in, once.
func TestAFailedMergeSaysWhichTrunksItHadAlreadyMoved(t *testing.T) {
	apply := func(out taskq.LandOutput) string {
		t.Helper()
		r, th, lead, _, _ := team(t)
		job, _ := r.asksMerge(th, ByLead)
		raw, _ := json.Marshal(out)
		if err := r.svc.MergeHandler().Apply(job, jobs.Result{Output: raw}); err != nil {
			t.Fatal(err)
		}
		if got := r.state(th.ID); got.State != store.ThreadOpen {
			t.Fatalf("a failed merge leaves the thread open: %+v", got)
		}
		return r.said(lead)
	}
	half := apply(taskq.LandOutput{Error: "backend: checks failed:\nFAIL", Fixable: 1, Landed: []string{"app into main"}})
	if !strings.Contains(half, "Already merged, and staying so: app into main.") || !strings.Contains(half, "backend-thread-1's worktree") {
		t.Fatalf("the lead is told which trunk had moved, and whose the failure is: %s", half)
	}
	// The failure's own words name the half already: it is not said twice.
	named := apply(taskq.LandOutput{Error: "backend did not merge: index.lock exists (already merged: app into main)", Fixable: -1, Landed: []string{"app into main"}})
	if strings.Count(named, "app into main") != 1 || strings.Contains(named, "Already merged, and staying so") {
		t.Fatalf("a half the failure names itself is named once: %s", named)
	}
	// Nothing had landed: nothing is said of a half.
	none := apply(taskq.LandOutput{Error: "backend: checks failed:\nFAIL", Fixable: 1})
	if strings.Contains(none, "Already merged") || strings.Contains(none, "already merged") {
		t.Fatalf("a merge that moved no trunk says nothing of one: %s", none)
	}
}

// failingSaves is the registry with a thread's row refusing to be written
// a number of times.
type failingSaves struct {
	Store
	left *int
}

func (f failingSaves) SaveThread(th store.Thread) error {
	if *f.left > 0 {
		*f.left--
		return errors.New("database is locked")
	}
	return f.Store.SaveThread(th)
}

// The thread's row cannot be written when its merge is settled: the job is
// not done, and the next try writes it. A thread never keeps saying
// "merging" behind a job that counts as settled.
func TestAThreadWhoseEndCouldNotBeWrittenIsSettledAgain(t *testing.T) {
	for _, landed := range []bool{false, true} {
		r, th, lead, _, _ := team(t)
		other, otherLead := r.thread("Second")
		job, res := r.asksMerge(th, ByLead)
		if !landed {
			raw, _ := json.Marshal(taskq.LandOutput{Error: "backend: checks failed:\nFAIL", Fixable: 1})
			res = jobs.Result{Output: raw}
		}
		fails := 1
		r.svc.St = failingSaves{Store: r.st, left: &fails}
		if err := r.svc.MergeHandler().Apply(job, res); err == nil || !strings.Contains(err.Error(), "was not recorded") {
			t.Fatalf("landed=%v: a row that was not written leaves the job to be settled again: %v", landed, err)
		}
		if got := r.state(th.ID); got.State != store.ThreadMerging || len(r.sess.prompts(lead.ID)) != 0 {
			t.Fatalf("landed=%v: nothing is said of an end that is not on record yet: %+v", landed, got)
		}
		if err := r.svc.MergeHandler().Apply(job, res); err != nil {
			t.Fatal(err)
		}
		want := store.ThreadOpen
		if landed {
			want = store.ThreadMerged
		}
		if got := r.state(th.ID); got.State != want || len(r.sess.prompts(lead.ID)) != 1 {
			t.Fatalf("landed=%v: the second try records it and tells the lead once: %+v %v", landed, got, r.sess.prompts(lead.ID))
		}
		if landed && r.removed() != "backend|app" {
			t.Fatalf("and the team was taken apart once, not twice: %s", r.removed())
		}
		if !landed && r.removed() != "" {
			t.Fatalf("a failed merge takes nothing apart, however often it is settled: %s", r.removed())
		}
		if r.state(other.ID).State != store.ThreadOpen || len(r.sess.prompts(otherLead.ID)) != 0 {
			t.Fatal("another thread is not touched")
		}
	}
}

// At the daemon's start the jobs cannot be read (the registry is locked):
// a merging thread is left exactly as it is. Its merge may well be running
// in its own service, and "cut off, ask again" would be a lie about it.
func TestAMergingThreadIsNotResetWhenItsJobCannotBeRead(t *testing.T) {
	r, th, lead, _, _ := team(t)
	idle, idleLead := r.thread("Second")
	job, _ := r.asksMerge(th, ByLead)
	r.jobs.openErr = errors.New("database is locked")
	r.svc.members = nil // a fresh daemon
	r.svc.Recover()
	if got := r.state(th.ID); got.State != store.ThreadMerging || got.Note != "" {
		t.Fatalf("not knowing is not a no: the thread still merges: %+v", got)
	}
	if said := r.sess.prompts(lead.ID); len(said) != 0 {
		t.Fatalf("and its lead is told nothing: %v", said)
	}
	if r.state(idle.ID).State != store.ThreadOpen || len(r.sess.prompts(idleLead.ID)) != 0 {
		t.Fatal("a thread that was not merging is as it was")
	}
	// Readable, and the job is gone for good with the thread never settled
	// (its row from before merges were jobs, say): now it is a plain no.
	r.jobs.openErr = nil
	r.jobs.done(job, store.JobLost)
	r.svc.Recover()
	if got := r.state(th.ID); got.State != store.ThreadOpen || !strings.Contains(got.Note, "daemon restart") || len(r.sess.prompts(lead.ID)) != 1 {
		t.Fatalf("with no job at all the thread is opened and its lead told, once: %+v", got)
	}
	r.svc.Recover()
	if len(r.sess.prompts(lead.ID)) != 1 {
		t.Fatal("and a second start says nothing more")
	}
}

// A helper that joins while the merge job runs (after the team was kept on
// the job's row, before its end is acted on) is stood down with the rest:
// it is on no list the merge made.
func TestAHelperThatJoinsDuringTheMergeIsStoodDownWithTheRest(t *testing.T) {
	r := newRig(t)
	th, lead := r.thread("Trade CSV export")
	lead, backend := r.edits(lead.ID), r.edits(r.helper(th, "backend").ID)
	job, res := r.asksMerge(th, ByLead)
	ask, err := readAsk(job)
	if err != nil || len(ask.Members) != 2 {
		t.Fatalf("the rig: the merge found the lead and one helper: %+v %v", ask, err)
	}
	late := r.edits(r.helper(th, "admin").ID)
	if err := r.svc.MergeHandler().Apply(job, res); err != nil {
		t.Fatal(err)
	}
	for _, h := range []store.RunnerSession{backend, late} {
		if got := r.row(h.ID); got.Wanted != store.SessionDone {
			t.Fatalf("every helper is stood down, the one that joined during the merge too: %+v", got)
		}
	}
	if removed := r.removed(); !strings.Contains(removed, "backend") || !strings.Contains(removed, "admin") {
		t.Fatalf("the late helper's worktree is tidied like the others: %s", removed)
	}
	if got := r.row(lead.ID); got.Wanted != store.SessionRunning {
		t.Fatalf("the lead stays: %+v", got)
	}
}

// membersFail is a registry whose members cannot be read.
type membersFail struct {
	Store
	err error
}

func (m membersFail) ThreadMembers(int64) ([]store.RunnerSession, error) { return nil, m.err }

// When the members cannot be read at the merge's end, the helpers the merge
// found are still stood down, from its own copy, and what is left says that
// one who joined since may still be there.
func TestHelpersThatCannotBeReadAtTheMergesEndAreSaidAsLeft(t *testing.T) {
	r, th, _, backend, admin := team(t)
	job, _ := r.asksMerge(th, ByLead)
	ask, err := readAsk(job)
	if err != nil {
		t.Fatal(err)
	}
	r.svc.St = membersFail{Store: r.st, err: errors.New("database is locked")}
	helpers, left := r.svc.helpersToEnd(ask)
	if len(helpers) != 2 || helpers[0].ID != backend.ID || helpers[1].ID != admin.ID || helpers[0].Worktree != backend.Worktree {
		t.Fatalf("the merge's own copy of its helpers stands, with their worktrees: %+v", helpers)
	}
	if len(left) != 1 || !strings.Contains(left[0], "joined during the merge") || !strings.Contains(left[0], "database is locked") {
		t.Fatalf("what is left names the helpers that could not be read, and why: %v", left)
	}
}
