// Package threads is threads with helpers: a thread is one piece of work
// with a team, a task-queue team a person talks to live. One runner session
// leads it, in its repo, and stands up a helper session of its own for every
// other repo the work needs, even when that repo has a live session.
//
// A member (the lead or a helper) starts lean in its repo's own folder,
// where it only reads, and is moved into its own git worktree by its first
// change. At the end the lead merges: every member's branch lands through
// the task queue's own merge (taskq.Lander: rebase, the repo's checks,
// fast-forward), run as a job in a service of its own (mergejob.go), and
// the helpers are stood down.
//
// The thread has a row (store/threads.go) and its members are session rows
// that carry the thread's id; this package owns the rules. The sessions
// themselves are internal/runner's, the bus routing that sends "backend" to
// the thread's own backend helper is internal/peers' (it asks Route here).
package threads

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// Store is the part of the registry a thread reads and writes.
type Store interface {
	AddThread(store.Thread) (store.Thread, error)
	SaveThread(store.Thread) error
	Thread(id int64) (*store.Thread, error)
	TaskThread(taskID int64) (*store.Thread, error)
	Threads() ([]store.Thread, error)
	ThreadMembers(threadID int64) ([]store.RunnerSession, error)
	RunnerSession(id string) (*store.RunnerSession, error)
}

// Sessions is the runner supervisor as a thread uses it.
type Sessions interface {
	Create(runner.NewSession) (store.RunnerSession, error)
	Place(sessionID string, p store.SessionPlace) error
	Relocate(sessionID string) error
	SetWanted(sessionID, wanted string) error
	SetModel(sessionID, model string) error
	Send(sessionID, text, sender string) (string, error)
	// SendAs is Send under an id the caller names: the same id sent again
	// queues nothing, which is how a notice that may be sent twice (a job's
	// end, settled again after a restart) is read once.
	SendAs(sessionID, promptID, text, sender string) (string, error)
}

// Host is the manager as a thread uses it: the repo sessions (workspaces)
// and the windows they sit in.
type Host interface {
	Workspace(wsID string) *model.Workspace
	WindowOfWorkspace(wsID string) (string, error)
	WindowPeers(paneID string) []*model.Workspace
}

// Service runs every thread on this daemon.
type Service struct {
	St       Store
	Sessions Sessions
	Host     Host
	// Lander is the task queue's own merge (taskq/land.go): its git makes a
	// member's worktree, and a thread's branches land through it.
	Lander taskq.Lander
	// Jobs runs a thread's merge outside this daemon (mergejob.go). Nil is a
	// daemon that runs no jobs: its threads are not merged, and JobsOff says
	// why.
	Jobs    Jobs
	JobsOff string
	// CheckLine is how long one line of a repo's checks may run in a merge;
	// zero is the usual (checkLine).
	CheckLine time.Duration
	// Changed hears that a thread looks different now (its merge started, or
	// ended), with its lead: whoever shows the thread reads it again. Nil
	// hears nothing.
	Changed func(lead store.RunnerSession)
	// Changes reads what a member's branch holds (taskq.ReadBranchChanges).
	Changes func(worktree, trunk string) (taskq.BranchChanges, error)
	// Root is where worktrees go: the task queue's own folder, a thread's
	// beside a repo's task worktrees.
	Root string
	Now  func() time.Time
	// MoveAfter is how long a member's restart into its worktree waits after
	// the daemon answered the call that asked for it: the answer has to
	// reach the runner before the runner is stopped.
	MoveAfter time.Duration

	locks sync.Map // session id → *sync.Mutex: one move of a member at a time
	// moving is the members whose restart into a new folder is on its way
	// (session id → true), so asking twice restarts once.
	moving sync.Map

	// members is who is in which thread, by pane: what the bus asks under
	// its own lock (Route), so it is kept in memory and read from nowhere
	// else. Rebuilt from the rows after every change (reindex).
	mu      sync.RWMutex
	members map[string]memberRef
}

// memberRef is a member's thread and the repo it works for.
type memberRef struct {
	thread int64
	repo   string
}

// sender is who a thread's own notices are from.
const sender = "ccmux"

// Who asked for something to be done to a thread: its lead, through the
// thread tool, or a person, on the thread's page. The lead is told of what a
// person did to its team: it did not see it happen.
const (
	ByLead   = "lead"
	ByPerson = "person"
)

// ErrNotMember is a call about a session that is in no thread.
var ErrNotMember = errors.New("this session is not in a thread")

func (s *Service) millis() int64 { return s.Now().UnixMilli() }

// lock takes one member's lock and returns its release.
func (s *Service) lock(sessionID string) func() {
	m, _ := s.locks.LoadOrStore(sessionID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// member reads a session that is in a thread, with its thread.
func (s *Service) member(sessionID string) (store.RunnerSession, store.Thread, error) {
	sess, err := s.St.RunnerSession(sessionID)
	if err != nil {
		return store.RunnerSession{}, store.Thread{}, err
	}
	if sess == nil || sess.ThreadID == 0 {
		return store.RunnerSession{}, store.Thread{}, ErrNotMember
	}
	th, err := s.St.Thread(sess.ThreadID)
	if err != nil {
		return *sess, store.Thread{}, err
	}
	if th == nil {
		return *sess, store.Thread{}, fmt.Errorf("thread #%d of session %s has no row", sess.ThreadID, sessionID)
	}
	return *sess, *th, nil
}

// New is what a thread is started from: the repo session (workspace) that
// leads it, what it is called, and the model its members start on.
type New struct {
	WorkspaceID, Title, Model string
}

// Create starts a thread: its row, and its lead session in the repo's own
// folder, reading only. The lead's first message is the caller's to send.
func (s *Service) Create(n New) (store.Thread, store.RunnerSession, error) {
	ws := s.Host.Workspace(n.WorkspaceID)
	if ws == nil || ws.Agent != "" {
		return store.Thread{}, store.RunnerSession{}, fmt.Errorf("workspaceId is not a repo session on this host: %s", n.WorkspaceID)
	}
	if _, err := os.Stat(filepath.Join(ws.RepoPath, ".git")); err != nil {
		return store.Thread{}, store.RunnerSession{}, fmt.Errorf("%s is not a git repo, and a thread's work lands through git", ws.RepoPath)
	}
	window, err := s.Host.WindowOfWorkspace(ws.ID)
	if err != nil {
		return store.Thread{}, store.RunnerSession{}, fmt.Errorf("reading the repo's window: %w", err)
	}
	now := s.millis()
	th, err := s.St.AddThread(store.Thread{Group: window, Title: n.Title, State: store.ThreadOpen, Model: n.Model, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return th, store.RunnerSession{}, err
	}
	lead, err := s.Sessions.Create(memberSession(th, ws, store.RoleLead))
	if err != nil {
		return th, lead, s.dropUnled(th, err)
	}
	th.LeadSession = lead.ID
	if err := s.St.SaveThread(th); err != nil {
		return th, lead, err
	}
	s.reindex()
	return th, lead, nil
}

// memberSession is the session a thread's member starts as: in its repo's
// own folder, reading only, under the bus name its teammates can work out.
func memberSession(th store.Thread, ws *model.Workspace, role string) runner.NewSession {
	return runner.NewSession{
		Cwd: ws.RepoPath, Name: taskq.ThreadBusName(ws.RepoPath, th.ID), Model: th.Model, WorkspaceID: ws.ID, Title: th.Title,
		ThreadID: th.ID, Role: role, Repo: ws.RepoPath, ReadOnly: true,
	}
}

// dropUnled ends a thread whose lead could not be made: nothing leads it,
// so nothing would ever close it.
func (s *Service) dropUnled(th store.Thread, cause error) error {
	th.State, th.Note, th.UpdatedAt = store.ThreadEnded, "its lead could not be started: "+cause.Error(), s.millis()
	if err := s.St.SaveThread(th); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}
