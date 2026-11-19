package taskq

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/store"
)

// fakeJobs is the job manager as the queue uses it, with no systemd. A
// started job only stands in the list, as a real one runs in its own
// service: nothing of it happens in the runner's pass. The rig then plays
// the service and the manager by hand (rig.runJobs, or step by step with
// run, apply and done), which is what lets a test stop anywhere a daemon
// could be cut off.
type fakeJobs struct {
	t        *testing.T
	dir      string
	n        int
	jobs     []store.Job
	inputs   map[string]any // job id → what it was started from
	startErr map[string]error
	openErr  error // what Open and OpenJob fail with, when set
}

func (f *fakeJobs) Start(n jobs.New) (store.Job, error) {
	if err := f.startErr[n.Kind]; err != nil {
		return store.Job{}, err
	}
	for _, j := range f.jobs {
		if j.Subject == n.Subject && j.State != store.JobDone {
			return store.Job{}, store.ErrJobOpen
		}
	}
	f.n++
	kept, err := json.Marshal(n.Ctx)
	if err != nil {
		return store.Job{}, err
	}
	job := store.Job{ID: fmt.Sprintf("%s-%d", n.Name, f.n), Kind: n.Kind, Subject: n.Subject, State: store.JobRunning, Ctx: string(kept), Attempts: 1, StartedAt: int64(f.n)}
	f.jobs = append(f.jobs, job)
	if f.inputs == nil {
		f.inputs = map[string]any{}
	}
	f.inputs[job.ID] = n.Input
	return job, nil
}

func (f *fakeJobs) Open(kinds ...string) ([]store.Job, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	var out []store.Job
	for _, j := range f.jobs {
		if j.State != store.JobDone && slices.Contains(kinds, j.Kind) {
			out = append(out, j)
		}
	}
	return out, nil
}

func (f *fakeJobs) OpenJob(subject string) (*store.Job, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	for _, j := range f.jobs {
		if j.State != store.JobDone && j.Subject == subject {
			return &j, nil
		}
	}
	return nil, nil
}

func (f *fakeJobs) Unit(id string) string { return "ccmux-job-test@" + id + ".service" }

func (f *fakeJobs) journal(id string) string { return filepath.Join(f.dir, id, "journal.jsonl") }

func (f *fakeJobs) Steps(id string) ([]jobs.Entry, error) { return jobs.ReadJournal(f.journal(id)) }

func (f *fakeJobs) Applied(id string) (*jobs.Journal, error) {
	return jobs.OpenJournal(filepath.Join(f.dir, id, "applied.jsonl"))
}

// done marks a job as ended, as the manager does once its handler returned
// nil.
func (f *fakeJobs) done(id string) {
	for i := range f.jobs {
		if f.jobs[i].ID == id {
			f.jobs[i].State = store.JobDone
		}
	}
}

// open is the open jobs of a kind, oldest first.
func (f *fakeJobs) open(kind string) []store.Job {
	f.t.Helper()
	list, err := f.Open(kind)
	if err != nil {
		f.t.Fatal(err)
	}
	return list
}

// the is the one open job of a kind.
func (f *fakeJobs) the(kind string) store.Job {
	f.t.Helper()
	list := f.open(kind)
	if len(list) != 1 {
		f.t.Fatalf("want one open %s job, have %d", kind, len(list))
	}
	return list[0]
}

// run is the job's service: the landing or the tidy-up, over the rig's own
// git and checks, with the job's journal. It returns the result file.
func (g *rig) run(job store.Job) jobs.Result {
	g.t.Helper()
	j, err := jobs.OpenJournal(g.jobs.journal(job.ID))
	if err != nil {
		g.t.Fatal(err)
	}
	steps := runSteps{&jobs.Run{Journal: j}}
	l := Lander{Git: g.git, MergeGit: g.git, Checks: g.checks, Tag: "tasks", Steps: steps}
	var out any
	switch in := g.jobs.inputs[job.ID].(type) {
	case LandInput:
		note, err := l.Land(in.Work)
		out = LandResult(in.Work, note, err, landedSoFar(j))
	case TidyInput:
		out, _ = tidyWork(l, in.Work, steps, func() error { return nil })
	default:
		g.t.Fatalf("job %s was started from %T", job.ID, in)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		g.t.Fatal(err)
	}
	return jobs.Result{ID: job.ID, Kind: job.Kind, Output: raw}
}

// handler is the daemon's side of a job's kind.
func (g *rig) handler(kind string) jobs.Handler {
	if kind == TidyKind {
		return g.r.TidyHandler()
	}
	return g.r.MergeHandler()
}

// apply hands a job's result to its handler, as the manager does when the
// job's service has ended.
func (g *rig) apply(job store.Job, res jobs.Result) error {
	return g.handler(job.Kind).Apply(job, res)
}

// runJobs runs every open job to its end and settles it, the jobs those
// start too (a merge's tidy-up): the queue as it is once nothing runs.
func (g *rig) runJobs() {
	g.t.Helper()
	for {
		open, err := g.jobs.Open(MergeKind, TidyKind)
		if err != nil {
			g.t.Fatal(err)
		}
		if len(open) == 0 {
			return
		}
		job := open[0]
		if err := g.apply(job, g.run(job)); err != nil {
			g.t.Fatalf("job %s was not settled: %v", job.ID, err)
		}
		g.jobs.done(job.ID)
	}
}
