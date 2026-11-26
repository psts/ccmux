package threads

import (
	"errors"
	"fmt"
	"log"

	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// A queued task is a thread the task queue leads (task #176). The thread
// has a row like any other, with the task's number and no lead session;
// its members are builders, one per part of the task, each a Claude
// session in its part's worktree from its first moment.
//
// The queue does what a lead does, and more: it starts each part's builder
// (StartBuilder here), watches it, runs the merge as its own job and ends
// the builders when the task is merged or cancelled (it ends their
// sessions; this package hears of it in SessionEnded). So nothing of a
// led thread applies to a task's: no lead to ask, no thread tool, no
// routing of a repo's plain name to a thread member (a builder's teammates
// go by <repo>-task-<n>, as its brief says, and a repo's plain name stays
// the repo's own session), no merge of the thread's own. store.Threads
// leaves such a thread out, so the member index, the routing and the merge
// recovery never meet one, and led refuses the rest with the reason.

// builderMode is the permission mode every builder runs in: nobody is
// there to answer a prompt.
const builderMode = "auto"

// taskKey names the lock of one task's thread: its builders are started,
// and it is closed after its last one, one at a time.
func taskKey(taskID int64) string { return fmt.Sprintf("task:%d", taskID) }

// StartBuilder starts the builder of one part of a queued task, as a
// member of the task's thread (made here for the task's first part, opened
// again for a part built again), and sends it its first prompt. It returns
// the session's pane id, which is how the queue knows it from then on.
//
// A builder that could not be given its first prompt is ended again and
// the start fails: one left running with nothing to do would sit until the
// watchdog gave up on it, holding its worktree.
func (s *Service) StartBuilder(b taskq.Builder) (string, error) {
	defer s.lock(taskKey(b.TaskID))()
	th, err := s.taskThread(b)
	if err != nil {
		return "", fmt.Errorf("the task's thread: %w", err)
	}
	sess, err := s.Sessions.Create(builderSession(th, b))
	if err != nil {
		return "", fmt.Errorf("starting its session: %w", err)
	}
	if err := s.firstPrompt(sess, b); err != nil {
		if endErr := s.Sessions.SetWanted(sess.ID, store.SessionDone); endErr != nil {
			err = errors.Join(err, fmt.Errorf("and its session %s was not ended: %w", sess.ID, endErr))
		}
		return "", err
	}
	log.Printf("threads: task #%d: %s builds in %s as session %s (thread #%d)", b.TaskID, b.Name, b.Worktree, sess.ID, th.ID)
	return sess.PaneID, nil
}

// firstPrompt gives a new builder what the queue knows it by and its first
// prompt. A session with no pane is on no bus: it could never report.
func (s *Service) firstPrompt(sess store.RunnerSession, b taskq.Builder) error {
	if sess.PaneID == "" {
		return fmt.Errorf("its session has no pane (no ccmux session for %s), so it could not report", b.Repo)
	}
	if _, err := s.Sessions.Send(sess.ID, b.Prompt, sender); err != nil {
		return fmt.Errorf("its first prompt was not queued: %w", err)
	}
	return nil
}

// taskThread is the thread of b's task: its newest one, opened again when
// it had closed (a part is built again after its builders ended), or a new
// one for a task that starts for the first time.
func (s *Service) taskThread(b taskq.Builder) (store.Thread, error) {
	th, err := s.St.TaskThread(b.TaskID)
	if err != nil {
		return store.Thread{}, err
	}
	now := s.millis()
	if th == nil {
		return s.St.AddThread(store.Thread{Group: b.WindowID, Title: b.Title, State: store.ThreadOpen, TaskID: b.TaskID, CreatedAt: now, UpdatedAt: now})
	}
	if th.State == store.ThreadOpen {
		return *th, nil
	}
	th.State, th.Note, th.UpdatedAt = store.ThreadOpen, "", now
	return *th, s.St.SaveThread(*th)
}

// builderSession is the session a builder starts as: in its worktree, on
// its branch, under its task name on the bus, on its part's model and
// effort, in auto mode.
func builderSession(th store.Thread, b taskq.Builder) runner.NewSession {
	return runner.NewSession{
		Cwd: b.Worktree, Name: b.Name, Model: b.Run.Model, WorkspaceID: b.WorkspaceID,
		Title:    fmt.Sprintf("Task #%d: %s", b.TaskID, b.Title),
		ThreadID: th.ID, Role: store.RoleBuilder, Repo: b.Repo, Worktree: b.Worktree, Branch: b.Branch,
		Effort: b.Run.Effort, PermissionMode: builderMode,
	}
}

// SessionBrief is the standing text any session starts with
// (runner.Config.Brief): a builder's is its part's whole brief, read from
// the file the queue wrote beside its worktree, and one that cannot be
// read fails the start; a repo's planner gets plannerBrief; a led thread's
// member gets Brief; any other session in no thread gets none.
func (s *Service) SessionBrief(sess store.RunnerSession) (string, error) {
	if sess.Role == store.RoleBuilder {
		return taskq.ReadBrief(sess.Worktree)
	}
	if sess.Kind == store.SessionPlanner {
		return s.plannerBrief(sess), nil
	}
	return s.Brief(sess), nil
}

// builderEnded hears that a builder's session has ended (the queue ended
// it: its task merged or was cancelled, its part is built again; or a
// person did). Its teammates stay as they are, each part is the queue's to
// end. The task's thread closes with its last builder, and opens again if
// a part is built again (taskThread).
func (s *Service) builderEnded(sess store.RunnerSession) {
	th, err := s.St.Thread(sess.ThreadID)
	if err != nil || th == nil {
		log.Printf("threads: #%d: a builder ended and its thread was not read: %v", sess.ThreadID, err)
		return
	}
	defer s.lock(taskKey(th.TaskID))()
	left, err := s.St.ThreadMembers(th.ID)
	if err != nil {
		log.Printf("threads: #%d: a builder ended and its teammates were not read: %v", th.ID, err)
		return
	}
	id := th.ID
	if th, err = s.St.Thread(id); err != nil {
		log.Printf("threads: #%d: a builder ended and its thread was not read again, so it is not closed: %v", id, err)
		return
	}
	if th == nil || len(left) > 0 || th.State == store.ThreadEnded {
		return
	}
	th.State, th.UpdatedAt = store.ThreadEnded, s.millis()
	if err := s.St.SaveThread(*th); err != nil {
		log.Printf("threads: #%d ended with its last builder but is not marked so: %v", th.ID, err)
	}
}

// taskOnly refuses what only a led thread has, for a task's thread.
func taskOnly(th store.Thread) error {
	return fmt.Errorf("thread #%d is queued task #%d: the task queue starts, merges and ends it, so do this to the task in the Tasks view", th.ID, th.TaskID)
}
