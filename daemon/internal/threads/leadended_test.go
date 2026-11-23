package threads

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// endLead ends the lead's session as a person does on its page, and the
// daemon hears of it.
func (r *rig) endLead(lead store.RunnerSession) {
	r.t.Helper()
	if err := r.sess.SetWanted(lead.ID, store.SessionDone); err != nil {
		r.t.Fatal(err)
	}
	r.svc.SessionEnded(r.row(lead.ID))
}

// untouched fails unless the thread still merges with its whole team.
func (r *rig) untouched(th store.Thread, helpers ...store.RunnerSession) {
	r.t.Helper()
	if got := r.state(th.ID); got.State != store.ThreadMerging {
		r.t.Fatalf("the thread still reads as merging: %+v", got)
	}
	for _, h := range helpers {
		if r.row(h.ID).Wanted != store.SessionRunning {
			r.t.Fatalf("helper %s is not stood down under the merge", h.Name)
		}
	}
	if r.removed() != "" {
		r.t.Fatalf("no worktree is removed under the merge: %s", r.removed())
	}
}

// takenApart fails unless the thread reads as ended with a note holding
// want, its helpers are stood down, both worktrees are gone, and nobody
// was told or moved.
func (r *rig) takenApart(th store.Thread, lead store.RunnerSession, want string, helpers ...store.RunnerSession) store.Thread {
	r.t.Helper()
	got := r.state(th.ID)
	if got.State != store.ThreadEnded || !strings.HasPrefix(got.Note, "its lead ended during the merge; ") || !strings.Contains(got.Note, want) {
		r.t.Fatalf("the thread is ended and says what the merge did (%q): %+v", want, got)
	}
	for _, h := range helpers {
		if r.row(h.ID).Wanted != store.SessionDone {
			r.t.Fatalf("helper %s is stood down", h.Name)
		}
	}
	if r.removed() != "backend|app" {
		r.t.Fatalf("the helper's worktree and the lead's are removed, once: %s", r.removed())
	}
	if said := r.sess.prompts(lead.ID); len(said) != 0 || len(r.sess.moves()) != 2 {
		r.t.Fatalf("nobody is told, and the ended lead is not moved: %v %v", said, r.sess.moves())
	}
	return got
}

// The brief's case: the lead ends while its merge runs. The merge is
// stopped and nothing is removed under it; when it has stopped, the thread
// is taken apart and says what had landed; settled again, nothing changes.
func TestALeadEndingMidMergeStopsItAndItsEndTakesTheThreadApart(t *testing.T) {
	r, th, lead, backend, admin := team(t)
	job, _ := r.asksMerge(th, ByLead)
	r.endLead(lead)
	if len(r.jobs.stopped) != 1 || r.jobs.stopped[0] != job.ID || r.jobs.job(th.ID).Note != stoppedLeadEnded {
		t.Fatalf("the merge is stopped, and its row says why: %v %+v", r.jobs.stopped, r.jobs.job(th.ID))
	}
	r.untouched(th, backend, admin)
	// Its journal says one trunk had moved when it stopped.
	j, err := jobs.OpenJournal(filepath.Join(r.jobs.dir, job.ID, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	j.Record("landed:"+r.repo("app")+"@rev-app", "app into main")
	h := r.svc.MergeHandler()
	if err := h.Stopped(r.jobs.job(th.ID)); err != nil {
		t.Fatal(err)
	}
	first := r.takenApart(th, lead, "merge stopped (already merged: app into main)", backend, admin)
	if err := h.Stopped(r.jobs.job(th.ID)); err != nil {
		t.Fatal(err)
	}
	if again := r.takenApart(th, lead, "merge stopped", backend, admin); again != first {
		t.Fatalf("settled twice, the second changes nothing: %+v %+v", first, again)
	}
}

// The merge landed before the stop took: it is applied, and the thread
// ends with what landed. The lead is not moved home: it has ended.
func TestAMergeThatLandedAsItsLeadEndedEndsTheThreadWithWhatLanded(t *testing.T) {
	r, th, lead, backend, admin := team(t)
	job, res := r.asksMerge(th, ByLead)
	r.endLead(lead)
	h := r.svc.MergeHandler()
	for range 2 {
		if err := h.Apply(job, res); err != nil {
			t.Fatal(err)
		}
		r.takenApart(th, lead, "merged app into main, backend into main", backend, admin)
	}
	if got := r.row(lead.ID); got.Worktree != lead.Worktree {
		t.Fatalf("the ended lead's row is not rewritten: %+v", got)
	}
}

// A failed merge of a thread whose lead ended says what had landed.
func TestAFailedMergeOfALeadlessThreadSaysWhatLanded(t *testing.T) {
	r, th, lead, backend, admin := team(t)
	job, _ := r.asksMerge(th, ByLead)
	r.endLead(lead)
	raw, _ := json.Marshal(taskq.LandOutput{Error: "backend: checks failed:\nFAIL", Fixable: 1, Landed: []string{"app into main"}})
	if err := r.svc.MergeHandler().Apply(job, jobs.Result{Output: raw}); err != nil {
		t.Fatal(err)
	}
	r.takenApart(th, lead, "merge failed: backend: checks failed: (already merged: app into main)", backend, admin)
}

// The lead stopped its own merge and then ended: the stop that is under
// way is left to finish, and its end takes the thread apart.
func TestALeadThatStoppedItsMergeAndEndedLeavesItToTheStop(t *testing.T) {
	r, th, lead, backend, admin := team(t)
	r.asksMerge(th, ByLead)
	if _, err := r.svc.StopMerge(th.ID, ByLead); err != nil {
		t.Fatal(err)
	}
	r.endLead(lead)
	if len(r.jobs.stopped) != 1 {
		t.Fatalf("it is not stopped twice: %v", r.jobs.stopped)
	}
	r.untouched(th, backend, admin)
	if err := r.svc.MergeHandler().Stopped(r.jobs.job(th.ID)); err != nil {
		t.Fatal(err)
	}
	r.takenApart(th, lead, "merge stopped", backend, admin)
}

// The merge ended just as the lead's end asked for its stop, and its end
// saw the lead gone: the thread is not taken apart a second time, and its
// note keeps what landed.
func TestAStopThatComesTooLateLeavesWhatTheMergesEndDid(t *testing.T) {
	r, th, lead, backend, admin := team(t)
	job, res := r.asksMerge(th, ByLead)
	r.jobs.beforeStop = func() {
		if err := r.svc.MergeHandler().Apply(job, res); err != nil {
			t.Error(err)
		}
		r.jobs.done(job, store.JobApplied)
	}
	r.endLead(lead)
	r.takenApart(th, lead, "merged app into main", backend, admin)
}

// The daemon heard the merge's end before it heard the lead's: the thread
// was taken apart by the merge's end, and the lead's end keeps its note.
func TestALeadsEndHeardAfterItsMergesEndKeepsTheNote(t *testing.T) {
	r, th, lead, backend, admin := team(t)
	job, res := r.asksMerge(th, ByLead)
	if err := r.sess.SetWanted(lead.ID, store.SessionDone); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.MergeHandler().Apply(job, res); err != nil {
		t.Fatal(err)
	}
	r.jobs.done(job, store.JobApplied)
	r.svc.SessionEnded(r.row(lead.ID))
	got := r.state(th.ID)
	if got.State != store.ThreadEnded || !strings.Contains(got.Note, "merged app into main") {
		t.Fatalf("the note of what landed stays: %+v", got)
	}
	for _, h := range []store.RunnerSession{backend, admin} {
		if r.row(h.ID).Wanted != store.SessionDone {
			t.Fatalf("helper %s is stood down", h.Name)
		}
	}
}

// A thread that reads as merging with no job to stop is taken apart at
// once, as before; one whose jobs cannot be read is left as it is.
func TestALeadEndingOnAMergingThreadWithNoJobTakesItApartAndAnUnreadOneNot(t *testing.T) {
	r, th, lead, backend, admin := team(t)
	job, _ := r.asksMerge(th, ByLead)
	r.jobs.openErr = errors.New("database is locked")
	r.endLead(lead)
	r.untouched(th, backend, admin)
	if len(r.jobs.stopped) != 0 {
		t.Fatalf("nothing is stopped on a guess: %v", r.jobs.stopped)
	}
	r.jobs.openErr = nil
	r.jobs.done(job, store.JobApplied) // gone, its end never heard
	r.svc.SessionEnded(r.row(lead.ID))
	got := r.state(th.ID)
	if got.State != store.ThreadEnded || r.row(backend.ID).Wanted != store.SessionDone || r.removed() != "backend|app" {
		t.Fatalf("with no job, the thread is taken apart now: %+v %s", got, r.removed())
	}
	// And with no jobs at all.
	r2, th2, lead2, backend2, _ := team(t)
	r2.svc.Jobs = nil
	th2.State = store.ThreadMerging
	if err := r2.st.SaveThread(th2); err != nil {
		t.Fatal(err)
	}
	r2.endLead(lead2)
	if r2.state(th2.ID).State != store.ThreadEnded || r2.row(backend2.ID).Wanted != store.SessionDone {
		t.Fatalf("a daemon with no jobs takes it apart: %+v", r2.state(th2.ID))
	}
}

// A lead that ends on an open thread is heard as ever: nothing is stopped.
func TestALeadEndingOnAnOpenThreadStopsNothing(t *testing.T) {
	r, th, lead, backend, _ := team(t)
	r.endLead(lead)
	if len(r.jobs.stopped) != 0 || r.state(th.ID).State != store.ThreadEnded || r.row(backend.ID).Wanted != store.SessionDone {
		t.Fatalf("the thread is taken apart at once: %v %+v", r.jobs.stopped, r.state(th.ID))
	}
	if r.removed() != "backend|app" {
		t.Fatalf("both worktrees go: %s", r.removed())
	}
}

// A merge this daemon cannot read, on a thread whose lead ended: what is
// left of the team goes, forcing nothing, and the thread ends saying so.
func TestAnUnreadMergeOfALeadlessThreadEndsIt(t *testing.T) {
	r, th, lead, backend, admin := team(t)
	job, res := r.asksMerge(th, ByLead)
	r.endLead(lead)
	job.Ctx = strings.Replace(job.Ctx, `"format":1`, `"format":2`, 1)
	for range 2 {
		if err := r.svc.MergeHandler().Apply(job, res); err != nil {
			t.Fatal(err)
		}
	}
	got := r.state(th.ID)
	if got.State != store.ThreadEnded || !strings.Contains(got.Note, "could not be read") {
		t.Fatalf("the thread ends and says its merge was not read: %+v", got)
	}
	if r.row(backend.ID).Wanted != store.SessionDone || r.row(admin.ID).Wanted != store.SessionDone || r.removed() != "backend|app" {
		t.Fatalf("its helpers are stood down and the worktrees go once: %s", r.removed())
	}
	if said := r.sess.prompts(lead.ID); len(said) != 0 {
		t.Fatalf("nobody is told: %v", said)
	}
}

// The thread of an unread merge whose lead ended could not be saved as
// ended: the job is not settled, and the next try ends it.
func TestAnUnreadMergeOfALeadlessThreadNotSavedIsSettledAgain(t *testing.T) {
	r, th, lead, backend, _ := team(t)
	job, res := r.asksMerge(th, ByLead)
	r.endLead(lead)
	job.Ctx = strings.Replace(job.Ctx, `"format":1`, `"format":2`, 1)
	fails := 1
	r.svc.St = failingSaves{Store: r.st, left: &fails}
	if err := r.svc.MergeHandler().Apply(job, res); err == nil {
		t.Fatal("a thread not saved as ended leaves the job to be settled again")
	}
	if got := r.state(th.ID); got.State != store.ThreadMerging {
		t.Fatalf("the thread still reads as merging: %+v", got)
	}
	if err := r.svc.MergeHandler().Apply(job, res); err != nil {
		t.Fatal(err)
	}
	if got := r.state(th.ID); got.State != store.ThreadEnded || r.row(backend.ID).Wanted != store.SessionDone {
		t.Fatalf("the next try ends it: %+v", got)
	}
}

// The thread's row cannot be read as its lead ends: its jobs say a merge
// runs, so the merge is stopped and nothing is removed under it. When the
// merge ends, its end takes the thread apart.
func TestALeadEndingOnAThreadWhoseRowIsNotReadAsksItsJobs(t *testing.T) {
	r, th, lead, backend, admin := team(t)
	job, res := r.asksMerge(th, ByLead)
	r.svc.St = &threadFailsAfter{Store: r.st, left: 0, err: errors.New("database is locked")}
	r.endLead(lead)
	r.svc.St = r.st
	r.untouched(th, backend, admin)
	if len(r.jobs.stopped) != 1 || r.jobs.stopped[0] != job.ID {
		t.Fatalf("the merge the jobs name is stopped: %v", r.jobs.stopped)
	}
	if err := r.svc.MergeHandler().Apply(job, res); err != nil {
		t.Fatal(err)
	}
	r.takenApart(th, lead, "merged app into main, backend into main", backend, admin)
}

// The row is never read and no merge job is open: the helpers go at once,
// and the sweep removes the lead's worktree and marks the thread ended.
func TestALeadEndingWithNoRowAndNoMergeStandsItsTeamDown(t *testing.T) {
	r, th, lead, backend, admin := team(t)
	r.svc.St = &threadFailsAfter{Store: r.st, left: 0, err: errors.New("database is locked")}
	r.endLead(lead)
	r.svc.St = r.st
	r.dropped(th, backend, admin)
	r.svc.SweepLeadless()
	if got := r.state(th.ID); got.State != store.ThreadEnded {
		t.Fatalf("the sweep ends the thread: %+v", got)
	}
	if r.removed() != "backend|app" {
		t.Fatalf("each worktree is removed once: %s", r.removed())
	}
}

// The row was read once (no merge) and not the second time: the same.
// (The sweep's half is the test above.)
func TestALeadEndingWhoseRowIsNotReadAgainStillStandsItsTeamDown(t *testing.T) {
	r, th, lead, backend, admin := team(t)
	r.svc.St = &threadFailsAfter{Store: r.st, left: 1, err: errors.New("database is locked")}
	r.endLead(lead)
	r.svc.St = r.st
	r.dropped(th, backend, admin)
}

// Neither the row nor the jobs are read: nothing is touched on a guess,
// and the sweep takes the thread apart once they read again.
func TestALeadEndingWithNothingReadIsTakenApartByTheSweep(t *testing.T) {
	r, th, lead, backend, admin := team(t)
	r.svc.St = &threadFailsAfter{Store: r.st, left: 0, err: errors.New("database is locked")}
	r.jobs.openErr = errors.New("database is locked")
	r.endLead(lead)
	r.svc.St, r.jobs.openErr = r.st, nil
	if r.row(backend.ID).Wanted != store.SessionRunning || r.removed() != "" || r.state(th.ID).State != store.ThreadOpen {
		t.Fatalf("nothing is touched: %+v %s", r.state(th.ID), r.removed())
	}
	r.svc.SweepLeadless()
	if got := r.state(th.ID); got.State != store.ThreadEnded || r.row(backend.ID).Wanted != store.SessionDone || r.row(admin.ID).Wanted != store.SessionDone {
		t.Fatalf("the sweep takes it apart: %+v", got)
	}
	if r.removed() != "backend|app" {
		t.Fatalf("both worktrees go: %s", r.removed())
	}
}

// dropped fails unless the helpers are stood down and their worktrees gone,
// while the lead's worktree stays and the thread's row still reads open.
func (r *rig) dropped(th store.Thread, helpers ...store.RunnerSession) {
	r.t.Helper()
	for _, h := range helpers {
		if r.row(h.ID).Wanted != store.SessionDone {
			r.t.Fatalf("helper %s is stood down", h.Name)
		}
	}
	if r.removed() != "backend" {
		r.t.Fatalf("only the helper's worktree goes: %s", r.removed())
	}
	if got := r.state(th.ID); got.State != store.ThreadOpen {
		r.t.Fatalf("a row that was not read is not written: %+v", got)
	}
}

// The stop is asked and not heard: the job runs on, nothing is removed
// under it, and its end takes the thread apart.
func TestAStopThatFailsLeavesTheMergeToItsEnd(t *testing.T) {
	r, th, lead, backend, admin := team(t)
	job, res := r.asksMerge(th, ByLead)
	r.jobs.stopErr = errors.New("unit not reached")
	r.endLead(lead)
	r.untouched(th, backend, admin)
	r.jobs.stopErr = nil
	if err := r.svc.MergeHandler().Apply(job, res); err != nil {
		t.Fatal(err)
	}
	r.takenApart(th, lead, "merged app into main, backend into main", backend, admin)
}
