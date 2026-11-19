package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/walls"
)

// Store is the part of the registry the jobs read and write
// (store/jobs.go).
type Store interface {
	AddJob(store.Job) error
	SaveJob(store.Job) error
	Job(id string) (*store.Job, error)
	OpenJob(subject string) (*store.Job, error)
	LastJob(subject string) (*store.Job, error)
	OpenJobs() ([]store.Job, error)
	JobsEndedBefore(millis int64) ([]store.Job, error)
}

// Handler is the daemon's side of one kind of job: what is done with how a
// job ended. Each is called again after a crash, and again later when it
// returned an error, until it returns nil; so each must be safe to repeat
// (Manager.Applied is a journal for exactly that).
type Handler interface {
	// Apply acts on a job's result.
	Apply(job store.Job, res Result) error
	// Lost settles a job that ended with no result the daemon reads: it
	// died more often than it is started again, its unit's time limit
	// stopped it, or its result is there and not readable (a VersionError,
	// or ErrGarbled). cause says which. The two are not the same to whoever
	// hears of it: a job with no result did not finish, one whose result
	// cannot be read may well have.
	Lost(job store.Job, cause error) error
	// Stopped settles a job that was stopped on request (Manager.Stop).
	Stopped(job store.Job) error
}

// Manager starts jobs, watches them and hands each one's end to its kind's
// handler. It holds nothing a restart would lose: a new manager over the
// same rows and files picks up where the last one stopped.
type Manager struct {
	St    Store
	Units Units
	Paths Paths
	// Env is the environment a job's work runs in (Spec.Env).
	Env func() []string
	Now func() time.Time
	// Every is how often the open jobs are looked at.
	Every time.Duration
	// MaxAttempts is how often a job that ends without a result is started
	// before it counts as lost.
	MaxAttempts int
	// RetryAfter is how long a job whose handler failed waits for the next
	// try.
	RetryAfter time.Duration
	// Walls is the walls a new job runs behind: nil (the func, or its
	// answer) for a job that runs open, an error for one that must not
	// start at all (the sandbox is on and the host cannot wall it). UnitDir
	// is the user's systemd folder, where a walled job's drop-in goes.
	Walls   func(New) (*walls.Plan, error)
	UnitDir string

	handlers map[string]Handler
	locks    sync.Map // job id → *sync.Mutex
	kick     chan struct{}
	mu       sync.Mutex
	later    map[string]time.Time // job id → not before
}

// NewManager makes a manager with the usual pace: a look every two
// seconds, three starts before a job is lost.
func NewManager(st Store, units Units, paths Paths) *Manager {
	return &Manager{
		St: st, Units: units, Paths: paths, Env: os.Environ, Now: time.Now,
		Every: 2 * time.Second, MaxAttempts: 3, RetryAfter: 30 * time.Second,
		handlers: map[string]Handler{}, kick: make(chan struct{}, 1), later: map[string]time.Time{},
	}
}

// Handle names the handler of one kind. Called before Run.
func (m *Manager) Handle(kind string, h Handler) { m.handlers[kind] = h }

// lock takes one job's lock and returns its release. Everything that
// changes a job (its start, a stop, a look at it) runs under it.
//
// A handler runs under the lock of the job it settles, and may take locks
// of its own. So whoever holds one of those must not call Stop for that
// job: let go first.
func (m *Manager) lock(id string) func() {
	l, _ := m.locks.LoadOrStore(id, &sync.Mutex{})
	mu := l.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// New is what a job is started from. Name opens its id ("merge-thread-3"),
// which is what its unit and its log are called. Input is the kind's own,
// read by the job; Ctx is what the one who starts it wants back at its end,
// kept on its row and never shown to the job. Limit is how long one run may
// take.
type New struct {
	Kind, Subject, Name string
	Input, Ctx          any
	Limit               time.Duration
}

// Start records a job and starts its service. A subject that has a job
// that has not ended is store.ErrJobOpen. A service that would not start is
// an error, and the job is recorded as lost: nothing runs, and nothing is
// handed to the kind's handler.
func (m *Manager) Start(n New) (store.Job, error) {
	suffix := make([]byte, 3)
	if _, err := rand.Read(suffix); err != nil {
		return store.Job{}, err
	}
	id := n.Name + "-" + hex.EncodeToString(suffix)
	input, err := json.Marshal(n.Input)
	if err != nil {
		return store.Job{}, err
	}
	kept, err := json.Marshal(n.Ctx)
	if err != nil {
		return store.Job{}, err
	}
	defer m.lock(id)()
	now := m.Now().UnixMilli()
	sp := Spec{ID: id, Kind: n.Kind, Subject: n.Subject, Input: input, Env: m.Env(), MaxSeconds: int(n.Limit.Seconds()), Locks: m.Paths.Locks, CreatedAt: now}
	if sp.Walls, err = m.wall(id, n); err != nil {
		os.RemoveAll(m.Paths.Dir(id))
		return store.Job{}, err
	}
	if err := WriteSpec(m.Paths.SpecFile(id), sp); err != nil {
		os.RemoveAll(m.Paths.Dir(id))
		m.unwall(id)
		return store.Job{}, fmt.Errorf("the job's spec was not written: %w", err)
	}
	job := store.Job{ID: id, Kind: n.Kind, Subject: n.Subject, State: store.JobRunning, Ctx: string(kept), Attempts: 1, StartedAt: now}
	if err := m.St.AddJob(job); err != nil {
		os.RemoveAll(m.Paths.Dir(id))
		m.unwall(id)
		return store.Job{}, err
	}
	if err := m.Units.Start(m.Paths.Unit(id)); err != nil {
		m.finish(job, store.JobLost, "its service did not start: "+err.Error())
		return job, fmt.Errorf("the job's service did not start: %w", err)
	}
	log.Printf("jobs: %s started for %s (journalctl --user -u %s)", id, n.Subject, m.Paths.Unit(id))
	m.Kick()
	return job, nil
}

// Stop asks for a running job to be stopped, with note saying who asked.
// It returns once systemd has the request; the job's handler hears
// (Stopped) when its processes are gone, or (Apply) when the job had its
// result written first: work that is done is not undone by a late stop.
func (m *Manager) Stop(id, note string) error {
	defer m.lock(id)()
	job, err := m.St.Job(id)
	if err != nil {
		return err
	}
	if job == nil || job.State == store.JobDone {
		return store.ErrNoJob
	}
	job.State, job.Note = store.JobStopping, note
	if err := m.St.SaveJob(*job); err != nil {
		return err
	}
	if err := m.Units.Stop(m.Paths.Unit(id)); err != nil {
		return err
	}
	m.Kick()
	return nil
}

// OpenJob is the job of subject that has not ended, or nil.
func (m *Manager) OpenJob(subject string) (*store.Job, error) { return m.St.OpenJob(subject) }

// Open lists the jobs of the kinds given that have not ended, oldest first.
func (m *Manager) Open(kinds ...string) ([]store.Job, error) {
	all, err := m.St.OpenJobs()
	if err != nil {
		return nil, err
	}
	var out []store.Job
	for _, job := range all {
		if slices.Contains(kinds, job.Kind) {
			out = append(out, job)
		}
	}
	return out, nil
}

// LastJob is the newest job of subject, ended or not, or nil.
func (m *Manager) LastJob(subject string) (*store.Job, error) { return m.St.LastJob(subject) }

// Unit is the name of a job's service, for whoever reads its log.
func (m *Manager) Unit(id string) string { return m.Paths.Unit(id) }

// Steps lists what a job's own journal holds: what it finished, and what
// it is in the middle of (Doing).
func (m *Manager) Steps(id string) ([]Entry, error) { return ReadJournal(m.Paths.JournalFile(id)) }

// Result reads a job's result file.
func (m *Manager) Result(id string) (Result, error) { return ReadResult(m.Paths.ResultFile(id)) }

// Applied opens the daemon's own journal of what it did with a job's end:
// a handler records each step there, and skips it when it is called again.
func (m *Manager) Applied(id string) (*Journal, error) { return OpenJournal(m.Paths.AppliedFile(id)) }

// finish records how a job ended. A row that cannot be written leaves the
// job open, and it is settled again on the next look: which is why a
// handler must be safe to repeat.
func (m *Manager) finish(job store.Job, outcome, note string) {
	defer m.unwall(job.ID)
	job.State, job.Outcome, job.EndedAt = store.JobDone, outcome, m.Now().UnixMilli()
	if note != "" {
		job.Note = note
	}
	if err := m.St.SaveJob(job); err != nil {
		log.Printf("jobs: %s ended (%s) and its row does not say so yet: %v", job.ID, outcome, err)
		return
	}
	log.Printf("jobs: %s is done: %s%s", job.ID, outcome, errorNote(job.Note))
}

// Kick asks for a look at the open jobs soon, without waiting for it.
func (m *Manager) Kick() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// ErrNoHandler is a job of a kind this daemon has no handler for.
var ErrNoHandler = errors.New("this daemon handles no job of that kind")
