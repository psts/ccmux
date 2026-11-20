package threads

import (
	"errors"
	"log"
	"os"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/store"
)

// What could not be read is logged. Each of these reads fell back to
// "nothing there" with no line anywhere, so a page that showed no merge,
// or a task's thread that never closed, gave nobody a reason.

// logs captures the log for one test.
func logs(t *testing.T) *strings.Builder {
	t.Helper()
	var b strings.Builder
	log.SetOutput(&b)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &b
}

func TestAMergeJobThatCannotBeReadIsLoggedNotShownAsNoMerge(t *testing.T) {
	r, th, _, _, _ := team(t)
	job, _ := r.asksMerge(th, ByLead)
	logged := logs(t)
	r.jobs.mu.Lock()
	r.jobs.lastErr = errors.New("database is locked")
	r.jobs.mu.Unlock()
	d, err := r.svc.Detail(th.ID)
	if err != nil || d.Merge != nil {
		t.Fatalf("the page still loads, with no merge on it: %+v %v", d.Merge, err)
	}
	if got := logged.String(); !strings.Contains(got, "threads: #1: its merge job was not read") || !strings.Contains(got, "database is locked") {
		t.Fatalf("and the log says why it shows none: %s", got)
	}
	// Readable again, the same thread shows its merge.
	r.jobs.mu.Lock()
	r.jobs.lastErr = nil
	r.jobs.mu.Unlock()
	if d, _ := r.svc.Detail(th.ID); d.Merge == nil || d.Merge.Job != job.ID {
		t.Fatalf("readable again, the merge is shown: %+v", d.Merge)
	}
}

func TestAFailedMergesResultThatCannotBeReadIsLogged(t *testing.T) {
	r, th, _, _, _ := team(t)
	r.git.set(func(g *fakeGit) { g.red = map[string]string{"backend": "FAIL: TestExport"} })
	job, res := r.asksMerge(th, ByLead)
	r.jobs.done(job, store.JobApplied)
	if err := r.svc.MergeHandler().Apply(job, res); err != nil {
		t.Fatal(err)
	}
	logged := logs(t)
	set := func(res *jobs.Result) {
		r.jobs.mu.Lock()
		defer r.jobs.mu.Unlock()
		r.jobs.results = map[string]jobs.Result{}
		if res != nil {
			r.jobs.results[job.ID] = *res
		}
	}
	// The result file is gone.
	set(nil)
	if d, _ := r.svc.Detail(th.ID); d.Merge == nil || d.Merge.State != "failed" || d.Merge.CheckLog != "" {
		t.Fatalf("the merge still reads as failed, with no check log: %+v", d.Merge)
	}
	if got := logged.String(); !strings.Contains(got, "job "+job.ID+": its result was not read") {
		t.Fatalf("a result that cannot be read is logged: %s", got)
	}
	// The result is there and says nothing this daemon understands.
	logged.Reset()
	set(&jobs.Result{ID: job.ID, Output: []byte("{not json")})
	if d, _ := r.svc.Detail(th.ID); d.Merge == nil || d.Merge.State != "failed" {
		t.Fatalf("the merge still reads as failed: %+v", d.Merge)
	}
	if got := logged.String(); !strings.Contains(got, "job "+job.ID+": its result was not understood") {
		t.Fatalf("a result that cannot be understood is logged: %s", got)
	}
}

// threadFailsAfter is a registry whose thread rows can be read n times.
type threadFailsAfter struct {
	Store
	left int
	err  error
}

func (f *threadFailsAfter) Thread(id int64) (*store.Thread, error) {
	if f.left <= 0 {
		return nil, f.err
	}
	f.left--
	return f.Store.Thread(id)
}

func TestALastBuilderWhoseThreadCannotBeReadAgainIsLogged(t *testing.T) {
	r := newRig(t)
	app := r.build(r.builderFor("app"))
	if err := r.sess.SetWanted(app.ID, store.SessionDone); err != nil {
		t.Fatal(err)
	}
	app.Wanted = store.SessionDone
	logged := logs(t)
	r.svc.St = &threadFailsAfter{Store: r.st, left: 1, err: errors.New("database is locked")}
	r.svc.SessionEnded(app)
	if th, _ := r.st.Thread(app.ThreadID); th.State == store.ThreadEnded {
		t.Fatalf("the rig: the thread could not be closed: %+v", th)
	}
	if got := logged.String(); !strings.Contains(got, "was not read again, so it is not closed") || !strings.Contains(got, "database is locked") {
		t.Fatalf("the read that failed is logged, with what it leaves: %s", got)
	}
	// Read again without the fault, the thread closes with its last builder.
	r.svc.St = r.st
	r.svc.SessionEnded(app)
	if th, _ := r.st.Thread(app.ThreadID); th.State != store.ThreadEnded {
		t.Fatalf("the thread closes with its last builder: %+v", th)
	}
}
