package jobs

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// Run looks at every open job now, then on every tick and every kick, until
// ctx ends. The first look is the recovery after a restart: there is no
// other. A job the last daemon left running is still watched, one that
// ended meanwhile is settled, one that died is run again.
func (m *Manager) Run(ctx context.Context) {
	m.prune()
	t := time.NewTicker(m.Every)
	defer t.Stop()
	for {
		m.Tend()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.kick:
		}
	}
}

// Tend looks at every open job once. One that fails is logged and the
// others go on. The list only names the jobs: each is handled under its own
// lock from its row read fresh.
func (m *Manager) Tend() {
	open, err := m.St.OpenJobs()
	if err != nil {
		log.Printf("jobs: the open jobs were not read: %v", err)
		return
	}
	for _, listed := range open {
		m.tend(listed.ID)
	}
}

// action is what a look at one job leads to.
type action int

const (
	watch   action = iota // it runs, or nobody can say: look again later
	apply                 // it ended with a result
	rerun                 // it ended without one: start it again
	lost                  // it ended without one, for good
	stopped               // it was stopped on request
)

// verdict is an action with, for lost, the reason.
type verdict struct {
	act   action
	cause error
}

// Unread reports whether a lost job's cause is a result that is there and
// cannot be read (another format, or garbled), as against no result at all.
func Unread(cause error) bool {
	var version VersionError
	return errors.As(cause, &version) || errors.Is(cause, ErrGarbled)
}

// timedOut is systemd's result for a unit it stopped at RuntimeMaxSec.
const timedOut = "timeout"

// decide is the whole rule for a job that is not done, from what systemd
// says of its unit and what reading its result file gave.
//
// Only a unit systemd calls dead in so many words is acted on; a state that
// could not be read, or any other, is watched. That is what keeps a job the
// last daemon started from being run a second time beside itself. A result
// that is there wins over everything else, a stop that came late included.
// With no result: a job that was asked to stop is stopped; one its unit's
// time limit killed, or that has been started as often as it may be, is
// lost; any other is run again, and skips what its journal says is done.
func decide(job store.Job, unit UnitState, unitErr, resultErr error, maxAttempts int) verdict {
	if unitErr != nil || !unit.Dead() {
		return verdict{act: watch}
	}
	switch {
	case resultErr == nil:
		return verdict{act: apply}
	case Unread(resultErr):
		return verdict{lost, resultErr}
	case !errors.Is(resultErr, os.ErrNotExist):
		return verdict{act: watch} // the file may be there and unreadable right now: never a reason to run it again
	case job.State == store.JobStopping:
		return verdict{act: stopped}
	case unit.Result == timedOut:
		return verdict{lost, errors.New("it ran into its service's own time limit and was stopped there, with no result")}
	case job.Attempts >= maxAttempts:
		return verdict{lost, fmt.Errorf("it ended %d times without a result (systemd's word for the last: %s)", job.Attempts, unit.Result)}
	}
	return verdict{act: rerun}
}

// tend looks at one job and acts on what decide says.
func (m *Manager) tend(id string) {
	defer m.lock(id)()
	if m.waiting(id) {
		return
	}
	job, err := m.St.Job(id)
	if err != nil {
		// Listed as open and not readable: said, and looked at again later.
		// Never skipped in silence: nothing else would ever settle it.
		m.putOff(id, fmt.Sprintf("jobs: %s is open and its row was not read (%v); looking again in %s", id, err, m.RetryAfter))
		return
	}
	if job == nil || job.State == store.JobDone {
		return
	}
	unit, unitErr := m.Units.State(m.Paths.Unit(id))
	res, resultErr := ReadResult(m.Paths.ResultFile(id))
	v := decide(*job, unit, unitErr, resultErr, m.MaxAttempts)
	if v.act == watch {
		m.cannotTell(id, unitErr, resultErr)
		return
	}
	if v.act == rerun {
		m.rerun(*job, unit)
		return
	}
	h, ok := m.handlers[job.Kind]
	if !ok {
		m.retryLater(id, ErrNoHandler)
		return
	}
	outcome, note, err := settle(h, *job, v, res)
	if err != nil {
		m.retryLater(id, err)
		return
	}
	m.finish(*job, outcome, note)
}

// settle hands a job's end to its handler and says how the row should read.
func settle(h Handler, job store.Job, v verdict, res Result) (outcome, note string, err error) {
	switch v.act {
	case apply:
		return store.JobApplied, "", h.Apply(job, res)
	case stopped:
		return store.JobStopped, "", h.Stopped(job)
	}
	return store.JobLost, v.cause.Error(), h.Lost(job, v.cause)
}

// rerun starts a job's service again, on the same unit: systemd runs one
// instance of a unit at a time, so even a wrong "dead" could not put two
// runs side by side. A start that fails counts as a try, so a service that
// cannot start ends as lost and not as a loop.
func (m *Manager) rerun(job store.Job, unit UnitState) {
	job.Attempts++
	if err := m.St.SaveJob(job); err != nil {
		log.Printf("jobs: %s: its next start was not counted, so it is not started: %v", job.ID, err)
		return
	}
	log.Printf("jobs: %s ended without a result (systemd: %s, %s); starting it again, try %d of %d", job.ID, unit.Active, unit.Result, job.Attempts, m.MaxAttempts)
	if err := m.Units.Start(m.Paths.Unit(job.ID)); err != nil {
		log.Printf("jobs: %s: %v", job.ID, err)
	}
}

// retryLater puts off the next look at a job whose handler failed, so a
// failure that lasts is said once per RetryAfter and not on every tick.
func (m *Manager) retryLater(id string, cause error) {
	m.putOff(id, fmt.Sprintf("jobs: %s has ended and was not settled (%v); trying again in %s", id, cause, m.RetryAfter))
}

// cannotTell says why a job is watched when it is watched because its state
// could not be read (systemd not answering, a result file there and not
// readable), once per wait. A job that is simply still running, with no
// result yet, is watched in silence. Without this a job that systemd never
// answers for again would read "running" for good, with nothing in the log.
func (m *Manager) cannotTell(id string, unitErr, resultErr error) {
	if resultErr != nil && errors.Is(resultErr, os.ErrNotExist) {
		resultErr = nil
	}
	if unitErr == nil && resultErr == nil {
		return
	}
	m.putOff(id, fmt.Sprintf("jobs: %s: its state cannot be told (unit: %v, result: %v); watching it, and looking again in %s", id, unitErr, resultErr, m.RetryAfter))
}

// putOff says why a job is not dealt with now and puts off the next look
// at it by RetryAfter: a trouble that lasts is in the log once per wait,
// not on every tick, and never not at all.
func (m *Manager) putOff(id, said string) {
	log.Print(said)
	m.mu.Lock()
	m.later[id] = m.Now().Add(m.RetryAfter)
	m.mu.Unlock()
}

// waiting reports whether a job is put off (retryLater), and forgets a
// wait that is over.
func (m *Manager) waiting(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	until, ok := m.later[id]
	if ok && m.Now().Before(until) {
		return true
	}
	delete(m.later, id)
	return false
}

// keepFiles is how long a done job's folder (spec, journals, result) is
// kept: long enough to look into a merge that went wrong last week.
const keepFiles = 14 * 24 * time.Hour

// prune removes the folders of jobs that ended long ago. Their rows stay.
func (m *Manager) prune() {
	old, err := m.St.JobsEndedBefore(m.Now().Add(-keepFiles).UnixMilli())
	if err != nil {
		log.Printf("jobs: old jobs were not read, their files stay: %v", err)
		return
	}
	for _, job := range old {
		dir := m.Paths.Dir(job.ID)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("jobs: %s not removed: %v", dir, err)
		}
	}
}
