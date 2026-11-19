package jobs

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// The recover table: what a look at a job that is not done leads to, from
// what systemd says of its unit and what its result file gave.
func TestTheRecoverTable(t *testing.T) {
	none := os.ErrNotExist
	running, stopping := store.Job{State: store.JobRunning, Attempts: 1}, store.Job{State: store.JobStopping, Attempts: 1}
	tired := store.Job{State: store.JobRunning, Attempts: 3}
	cases := []struct {
		name    string
		job     store.Job
		unit    UnitState
		unitErr error
		result  error
		want    action
		why     string
	}{
		{"the unit runs: left alone", running, UnitState{Active: "active"}, nil, none, watch, ""},
		{"the unit runs and the result is there already: wait for it to end", running, UnitState{Active: "active"}, nil, nil, watch, ""},
		{"the unit is starting: left alone", running, UnitState{Active: "activating"}, nil, none, watch, ""},
		{"the unit is stopping: not dead yet", stopping, UnitState{Active: "deactivating"}, nil, none, watch, ""},
		{"systemd gave no answer: never taken as dead", running, UnitState{}, errors.New("systemctl: failed to connect"), none, watch, ""},
		{"systemd answered with no state: never taken as dead", running, UnitState{}, nil, none, watch, ""},
		{"a state nobody knows: never taken as dead", running, UnitState{Active: "maintenance"}, nil, none, watch, ""},
		{"gone with a result: apply it", running, UnitState{Active: "inactive", Result: "success"}, nil, nil, apply, ""},
		{"gone with a result after a kill of a later run: apply it", running, UnitState{Active: "failed", Result: "signal"}, nil, nil, apply, ""},
		{"gone without a result (never started, or the host rebooted): run it again", running, UnitState{Active: "inactive", Result: "success"}, nil, none, rerun, ""},
		{"killed without a result: run it again", running, UnitState{Active: "failed", Result: "signal"}, nil, none, rerun, ""},
		{"killed without a result for the third time: lost", tired, UnitState{Active: "failed", Result: "signal"}, nil, none, lost, "ended 3 times without a result"},
		{"stopped by the unit's time limit: lost, not run again", running, UnitState{Active: "failed", Result: "timeout"}, nil, none, lost, "time limit"},
		{"asked to stop, and gone: stopped", stopping, UnitState{Active: "inactive", Result: "success"}, nil, none, stopped, ""},
		{"asked to stop, but it had finished first: its result counts", stopping, UnitState{Active: "inactive", Result: "success"}, nil, nil, apply, ""},
		{"a result in another format: lost, loudly, never guessed", running, UnitState{Active: "inactive"}, nil, VersionError{File: "result.json", Have: 2, Want: 1}, lost, "format 2"},
		{"a garbled result: lost", running, UnitState{Active: "inactive"}, nil, fmt.Errorf("x: %w", ErrGarbled), lost, "garbled"},
		{"a result that cannot be read right now: look again, never run again", running, UnitState{Active: "inactive"}, nil, errors.New("input/output error"), watch, ""},
	}
	for _, c := range cases {
		got := decide(c.job, c.unit, c.unitErr, c.result, 3)
		why := ""
		if got.cause != nil {
			why = got.cause.Error()
		}
		if got.act != c.want || !strings.Contains(why, c.why) || (c.why == "") != (got.cause == nil) {
			t.Errorf("%s: got %v %q", c.name, got.act, why)
		}
		if unread := strings.Contains(c.name, "format") || strings.Contains(c.name, "garbled"); got.act == lost && Unread(got.cause) != unread {
			t.Errorf("%s: a result that is there and unreadable is told apart from none: Unread=%v", c.name, Unread(got.cause))
		}
	}
}

// fakeUnits is systemd as the manager sees it: what each unit's state is,
// and what was asked of it.
type fakeUnits struct {
	mu       sync.Mutex
	state    map[string]UnitState
	stateErr error
	startErr error
	asked    []string
}

func (f *fakeUnits) Start(unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, "start "+unit)
	if f.startErr != nil {
		return f.startErr
	}
	f.state[unit] = UnitState{Active: "active"}
	return nil
}

func (f *fakeUnits) Stop(unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, "stop "+unit)
	f.state[unit] = UnitState{Active: "inactive", Result: "success"}
	return nil
}

func (f *fakeUnits) State(unit string) (UnitState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state[unit], f.stateErr
}
func (f *fakeUnits) Reload() error { return nil }

func (f *fakeUnits) set(unit string, st UnitState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state[unit] = st
}

func (f *fakeUnits) starts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, a := range f.asked {
		if strings.HasPrefix(a, "start ") {
			n++
		}
	}
	return n
}

// fakeHandler notes how each job's end reached it.
type fakeHandler struct {
	heard []string
	err   error
}

func (h *fakeHandler) Apply(job store.Job, res Result) error {
	h.heard = append(h.heard, "apply "+job.ID+" "+string(res.Output)+res.Error)
	return h.err
}

func (h *fakeHandler) Lost(job store.Job, cause error) error {
	h.heard = append(h.heard, "lost "+job.ID+" "+cause.Error())
	return h.err
}

func (h *fakeHandler) Stopped(job store.Job) error {
	h.heard = append(h.heard, "stopped "+job.ID+" "+job.Note)
	return h.err
}

type managerRig struct {
	t     *testing.T
	st    *store.SQLite
	units *fakeUnits
	h     *fakeHandler
	m     *Manager
	now   time.Time
}

func newManagerRig(t *testing.T) *managerRig {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r := &managerRig{t: t, st: st, units: &fakeUnits{state: map[string]UnitState{}}, h: &fakeHandler{}, now: time.UnixMilli(1000)}
	r.m = NewManager(st, r.units, Paths{State: filepath.Join(root, "jobs"), Locks: filepath.Join(root, "locks"), Prefix: "ccmux-job-test"})
	r.m.Now = func() time.Time { return r.now }
	r.m.Env = func() []string { return []string{"PATH=/daemon/bin"} }
	r.m.Handle("merge", r.h)
	return r
}

func (r *managerRig) start() store.Job {
	r.t.Helper()
	job, err := r.m.Start(New{Kind: "merge", Subject: "thread:3", Name: "merge-thread-3", Input: map[string]int{"n": 1}, Ctx: map[string]string{"by": "lead"}, Limit: time.Hour})
	if err != nil {
		r.t.Fatal(err)
	}
	return job
}

func (r *managerRig) row(id string) store.Job {
	r.t.Helper()
	job, err := r.st.Job(id)
	if err != nil || job == nil {
		r.t.Fatalf("job %s: %+v %v", id, job, err)
	}
	return *job
}

func (r *managerRig) ends(job store.Job, st UnitState, res *Result) {
	r.t.Helper()
	if res != nil {
		if err := WriteResult(r.m.Paths.ResultFile(job.ID), *res); err != nil {
			r.t.Fatal(err)
		}
	}
	r.units.set(r.m.Unit(job.ID), st)
}

func TestAJobIsStartedWatchedAndItsResultHandedOverOnce(t *testing.T) {
	r := newManagerRig(t)
	job := r.start()
	if !strings.HasPrefix(job.ID, "merge-thread-3-") || r.m.Unit(job.ID) != "ccmux-job-test@"+job.ID+".service" {
		t.Fatalf("the job is named after what it is for, and its unit after it: %+v", job)
	}
	sp, err := ReadSpec(r.m.Paths.SpecFile(job.ID))
	if err != nil || sp.Kind != "merge" || sp.MaxSeconds != 3600 || sp.Env[0] != "PATH=/daemon/bin" || sp.Locks != r.m.Paths.Locks || strings.Contains(string(sp.Input), "lead") {
		t.Fatalf("the spec carries the job's input, limit, environment and locks, and not what its starter kept: %+v %v", sp, err)
	}
	if got := r.row(job.ID); got.State != store.JobRunning || got.Attempts != 1 || !strings.Contains(got.Ctx, "lead") {
		t.Fatalf("the row says it runs, and keeps what its starter wants back: %+v", got)
	}
	if _, err := r.m.Start(New{Kind: "merge", Subject: "thread:3", Name: "merge-thread-3"}); !errors.Is(err, store.ErrJobOpen) {
		t.Fatalf("a second job for the same subject is refused: %v", err)
	}
	r.m.Tend()
	if len(r.h.heard) != 0 || r.units.starts() != 1 {
		t.Fatalf("a running job is left alone: %v", r.h.heard)
	}
	// A daemon restart in between changes nothing: a new manager over the
	// same rows finds the job running and leaves it.
	fresh := NewManager(r.st, r.units, r.m.Paths)
	fresh.Handle("merge", r.h)
	fresh.Tend()
	if len(r.h.heard) != 0 || r.units.starts() != 1 {
		t.Fatalf("a job that outlived the daemon is not started again: %v", r.h.heard)
	}
	r.ends(job, UnitState{Active: "inactive", Result: "success"}, &Result{ID: job.ID, Output: []byte(`"landed"`)})
	fresh.Tend()
	fresh.Tend()
	if len(r.h.heard) != 1 || r.h.heard[0] != `apply `+job.ID+` "landed"` {
		t.Fatalf("its result is handed over, once: %v", r.h.heard)
	}
	if got := r.row(job.ID); got.State != store.JobDone || got.Outcome != store.JobApplied || got.EndedAt == 0 {
		t.Fatalf("and the row says it is done: %+v", got)
	}
	if again := r.start(); again.ID == job.ID {
		t.Fatal("a new job for the subject is a new job")
	}
}

func TestAJobThatDiesIsRunAgainAndThenGivenUp(t *testing.T) {
	r := newManagerRig(t)
	job := r.start()
	for want := 2; want <= 3; want++ {
		r.ends(job, UnitState{Active: "failed", Result: "signal"}, nil)
		r.m.Tend()
		if got := r.row(job.ID); got.Attempts != want || got.State != store.JobRunning || r.units.starts() != want {
			t.Fatalf("killed without a result, it is started again (try %d): %+v, %d starts", want, got, r.units.starts())
		}
		r.m.Tend() // it runs again: left alone
	}
	r.ends(job, UnitState{Active: "failed", Result: "signal"}, nil)
	r.m.Tend()
	r.m.Tend()
	if r.units.starts() != 3 || len(r.h.heard) != 1 || !strings.HasPrefix(r.h.heard[0], "lost "+job.ID+" it ended 3 times without a result") {
		t.Fatalf("after three starts it is lost, said once, and not started again: %d starts, %v", r.units.starts(), r.h.heard)
	}
	if got := r.row(job.ID); got.State != store.JobDone || got.Outcome != store.JobLost || !strings.Contains(got.Note, "3 times") {
		t.Fatalf("the row says why: %+v", got)
	}
}

func TestSystemdNotAnsweringNeverStartsASecondRun(t *testing.T) {
	r := newManagerRig(t)
	job := r.start()
	r.units.mu.Lock()
	r.units.stateErr = errors.New("failed to connect to bus")
	r.units.state[r.m.Unit(job.ID)] = UnitState{}
	r.units.mu.Unlock()
	r.m.Tend()
	if r.units.starts() != 1 || len(r.h.heard) != 0 || r.row(job.ID).Attempts != 1 {
		t.Fatalf("no answer is not a dead job: %d starts, %v", r.units.starts(), r.h.heard)
	}
}

func TestAStoppedJobIsSettledAsStoppedAndNotRunAgain(t *testing.T) {
	r := newManagerRig(t)
	job := r.start()
	if err := r.m.Stop(job.ID, "stopped by a person"); err != nil {
		t.Fatal(err)
	}
	r.m.Tend()
	r.m.Tend()
	if r.units.starts() != 1 || len(r.h.heard) != 1 || r.h.heard[0] != "stopped "+job.ID+" stopped by a person" {
		t.Fatalf("a stop is a stop: %d starts, %v", r.units.starts(), r.h.heard)
	}
	if got := r.row(job.ID); got.Outcome != store.JobStopped || got.Note != "stopped by a person" {
		t.Fatalf("the row says who stopped it: %+v", got)
	}
	if err := r.m.Stop(job.ID, "again"); !errors.Is(err, store.ErrNoJob) {
		t.Fatalf("a job that is done has nothing to stop: %v", err)
	}
}

func TestAHandlerThatFailsIsAskedAgainLaterAndTheJobStaysOpen(t *testing.T) {
	r := newManagerRig(t)
	job := r.start()
	r.ends(job, UnitState{Active: "inactive", Result: "success"}, &Result{ID: job.ID})
	r.h.err = errors.New("the registry is locked")
	r.m.Tend()
	r.m.Tend()
	if len(r.h.heard) != 1 || r.row(job.ID).State != store.JobRunning {
		t.Fatalf("a failed hand-over leaves the job open and is not hammered: %v", r.h.heard)
	}
	r.h.err = nil
	r.now = r.now.Add(r.m.RetryAfter + time.Second)
	r.m.Tend()
	if len(r.h.heard) != 2 || r.row(job.ID).State != store.JobDone {
		t.Fatalf("it is handed over again once the wait is over: %v", r.h.heard)
	}
}

// unreadable is a registry in which one job's row cannot be read, though
// the list of open jobs still names it.
type unreadable struct {
	Store
	id  string
	err error
}

func (u unreadable) Job(id string) (*store.Job, error) {
	if id == u.id {
		return nil, u.err
	}
	return u.Store.Job(id)
}

func TestAJobWhoseRowCannotBeReadIsSaidAndTheOthersGoOn(t *testing.T) {
	r := newManagerRig(t)
	stuck := r.start()
	other, err := r.m.Start(New{Kind: "merge", Subject: "thread:4", Name: "merge-thread-4"})
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range []store.Job{stuck, other} {
		r.ends(job, UnitState{Active: "inactive", Result: "success"}, &Result{ID: job.ID})
	}
	r.m.St = unreadable{Store: r.st, id: stuck.ID, err: errors.New("database is locked")}
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	r.m.Tend()
	r.m.Tend()
	if n := strings.Count(logged.String(), stuck.ID+" is open and its row was not read (database is locked)"); n != 1 {
		t.Fatalf("a job that is open and not readable is said, once per wait and not on every look (%d times):\n%s", n, logged.String())
	}
	if len(r.h.heard) != 1 || !strings.HasPrefix(r.h.heard[0], "apply "+other.ID) {
		t.Fatalf("it is not settled on a guess, and the job beside it is settled as ever: %v", r.h.heard)
	}
	if r.units.starts() != 2 {
		t.Fatalf("and it is not started again: %d starts", r.units.starts())
	}
	// Readable again, and its wait over: it is settled.
	r.m.St = r.st
	r.now = r.now.Add(r.m.RetryAfter + time.Second)
	r.m.Tend()
	if len(r.h.heard) != 2 || !strings.HasPrefix(r.h.heard[1], "apply "+stuck.ID) || r.row(stuck.ID).State != store.JobDone {
		t.Fatalf("once its row reads again the job is settled: %v", r.h.heard)
	}
}

func TestAServiceThatWillNotStartIsAnErrorAndNoOpenJob(t *testing.T) {
	r := newManagerRig(t)
	r.units.startErr = errors.New("unit not found")
	job, err := r.m.Start(New{Kind: "merge", Subject: "thread:3", Name: "merge-thread-3"})
	if err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Fatalf("the start fails with why: %v", err)
	}
	if got := r.row(job.ID); got.State != store.JobDone || got.Outcome != store.JobLost {
		t.Fatalf("and leaves no open job behind: %+v", got)
	}
	r.m.Tend()
	if len(r.h.heard) != 0 {
		t.Fatalf("whoever asked heard the error; the handler hears nothing: %v", r.h.heard)
	}
}

func TestOldJobsFilesGoAndTheirRowsStay(t *testing.T) {
	r := newManagerRig(t)
	job := r.start()
	r.ends(job, UnitState{Active: "inactive"}, &Result{ID: job.ID})
	r.m.Tend()
	r.m.prune()
	if _, err := os.Stat(r.m.Paths.Dir(job.ID)); err != nil {
		t.Fatalf("a job that just ended keeps its files: %v", err)
	}
	r.now = r.now.Add(keepFiles + time.Hour)
	r.m.prune()
	if _, err := os.Stat(r.m.Paths.Dir(job.ID)); !os.IsNotExist(err) {
		t.Fatalf("after two weeks they go: %v", err)
	}
	if r.row(job.ID).Outcome != store.JobApplied {
		t.Fatal("the row stays")
	}
}

func TestTheTemplateIsWrittenOnceAndNeverOverAnotherDaemons(t *testing.T) {
	dir := t.TempDir()
	units := &fakeUnits{state: map[string]UnitState{}}
	mine := Paths{State: "/state/mine/jobs", Prefix: "ccmux-job-test"}
	if err := EnsureTemplate(dir, "/bin/ccmuxd", mine, DefaultLimits(), units); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(filepath.Join(dir, "ccmux-job-test@.service"))
	for _, want := range []string{"ExecStart=/bin/ccmuxd job-exec /state/mine/jobs/%i/spec.json", "KillMode=control-group", "RuntimeMaxSec=21600", "MemoryMax=75%", "CPUQuota=", "TasksMax=8192", "# ccmux-job-state=/state/mine/jobs"} {
		if !strings.Contains(string(text), want) {
			t.Fatalf("the template has %q:\n%s", want, text)
		}
	}
	if strings.Contains(string(text), "Restart=") {
		t.Fatalf("systemd never restarts a job by itself: that is the daemon's call:\n%s", text)
	}
	other := Paths{State: "/state/other/jobs", Prefix: "ccmux-job-test"}
	err := EnsureTemplate(dir, "/bin/ccmuxd", other, DefaultLimits(), units)
	if err == nil || !strings.Contains(err.Error(), "belongs to another daemon") || !strings.Contains(err.Error(), "/state/mine/jobs") {
		t.Fatalf("a second daemon does not take the first one's template: %v", err)
	}
	if again, _ := os.ReadFile(filepath.Join(dir, "ccmux-job-test@.service")); string(again) != string(text) {
		t.Fatal("and the file is as it was")
	}
}

// Open lists the jobs of the kinds asked for that have not ended: what a
// task queue asks to know whether its one merge line is taken. A job of
// another kind, and one that is done, are not in it.
func TestOpenListsTheUnfinishedJobsOfAKind(t *testing.T) {
	r := newManagerRig(t)
	merge := r.start()
	tidy, err := r.m.Start(New{Kind: "tidy", Subject: "task-tidy:3", Name: "tidy-task-3"})
	if err != nil {
		t.Fatal(err)
	}
	ended, err := r.m.Start(New{Kind: "tidy", Subject: "task-tidy:4", Name: "tidy-task-4"})
	if err != nil {
		t.Fatal(err)
	}
	r.m.finish(ended, store.JobApplied, "")
	ids := func(kinds ...string) string {
		t.Helper()
		list, err := r.m.Open(kinds...)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, j := range list {
			out = append(out, j.ID)
		}
		return strings.Join(out, " ")
	}
	if got := ids("tidy"); got != tidy.ID {
		t.Fatalf("the open jobs of one kind: %q", got)
	}
	if got := ids("merge", "tidy"); got != merge.ID+" "+tidy.ID {
		t.Fatalf("of two kinds, oldest first: %q", got)
	}
	if got := ids("landing"); got != "" {
		t.Fatalf("of a kind nothing runs: %q", got)
	}
}
