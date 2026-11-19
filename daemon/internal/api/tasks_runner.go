package api

import (
	"context"
	"errors"
	"time"

	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// taskHost is the manager, and the peers bus, as the task runner sees them.
type taskHost struct {
	mgr   *manager.Manager
	peers *peers.Service // nil when this daemon runs no bus
	// settle runs after each change the runner makes to the queue
	// (todos_tasks.go); nil for none.
	settle func()
	// build starts a part's builder as a session in the task's thread
	// (threads.Service.StartBuilder); nil on a daemon that runs no sessions.
	build func(taskq.Builder) (string, error)
}

// TellWorker sends a worker's Claude text over the bus, then counts the
// worker as prompted from now: a turn a bus message starts fires no
// user_prompt_submit hook, and the runner must not read its last Stop as
// "finished without reporting". msgID is the message's fixed id ("" for
// none): said again under the same id, the worker hears it once, and still
// counts as prompted.
func (h taskHost) TellWorker(paneID, msgID, text string) error {
	if h.peers == nil {
		return errors.New("this daemon runs no peers bus")
	}
	if err := h.peers.NotifyPaneAs(paneID, msgID, text); err != nil {
		return err
	}
	h.mgr.NoteTaskPrompt(paneID)
	return nil
}

func (h taskHost) TaskWorkspace(repo, windowID string) (string, error) {
	return h.mgr.TaskWorkspace(repo, windowID)
}
func (h taskHost) StartBuilder(b taskq.Builder) (string, error) {
	if h.build == nil {
		return "", errors.New(noBuilders)
	}
	return h.build(b)
}

// noBuilders is why a daemon without the session runners builds no task: a
// builder is a Claude session in a systemd service of its own.
const noBuilders = "it runs no Claude sessions (they need systemd)"

func (h taskHost) ClearTaskFolder(dir string) error { return h.mgr.ClearTaskFolder(dir) }

// TasksChanged tells the lenses, and lets what follows a task follow it
// (settle: a merged task's to-do item turns done).
func (h taskHost) TasksChanged() {
	h.mgr.TasksChanged()
	if h.settle != nil {
		h.settle()
	}
}

func (h taskHost) TaskHostnames(repo, windowID string) ([]model.Hostname, error) {
	return h.mgr.TaskHostnames(repo, windowID)
}
func (h taskHost) SetTaskRoutes(routes map[string]int) { h.mgr.SetTaskRoutes(routes) }
func (h taskHost) DevDomain() string                   { return h.mgr.DevDomain() }
func (h taskHost) TaskPane(paneID string) (taskq.PaneState, bool) {
	if paneID == "" {
		return taskq.PaneState{}, false
	}
	st, ok := h.mgr.TaskPane(paneID)
	sig := st.Signal
	return taskq.PaneState{Attention: sig.Attention, Reason: sig.Reason, At: sig.At, AtShell: st.AtShell, Why: st.Why}, ok
}

// StartTaskRunner starts the task queue's runner: up to slots tasks at once
// in a window with no number of its own (PUT /v1/windows/{id}/slots sets
// one), each built in a worktree under root, checked every `every`. It shares the
// routes' lock, so a person's edit never lands in the middle of a pass.
// slots 0 runs nothing: filed tasks just wait. Call it after EnablePeers:
// the runner takes the bus here, once, and without it no worker hears that
// its merge failed. And after SetThreads: a part's builder is a session in
// the task's thread, and a daemon with no threads service starts none
// (waiting tasks say so).
//
// A built feature's merge runs as a job under merges (taskq.Jobs; nil with
// jobsOff saying why this daemon runs none, and built features then wait
// and say so). The jobs' ends are acted on by TaskJobHandlers, which need
// the runner made here, ticking or not.
func (s *Server) StartTaskRunner(ctx context.Context, every time.Duration, slots int, root string, merges taskq.Jobs, jobsOff string) {
	if s.tasks == nil {
		return
	}
	host := taskHost{mgr: s.mgr, peers: s.peersSvc, settle: s.settleTodos}
	buildersOff := noBuilders
	if s.threads != nil {
		host.build, buildersOff = s.threads.StartBuilder, ""
	}
	r := &taskq.Runner{
		Store: s.tasks, Host: host, Git: taskq.ExecGit{}, MergeGit: taskq.ExecGit{}, BuildersOff: buildersOff,
		Jobs: merges, JobsOff: jobsOff, CheckLine: 20 * time.Minute, Lock: &s.tasksMu,
		Slots: slots, WindowSlots: s.mgr.WindowSlots, TotalSlots: s.mgr.TaskSlotsTotal, WindowPriorities: s.mgr.WindowPriorities,
		Root: root, Now: time.Now,
	}
	s.taskSettler = r
	if slots <= 0 {
		return
	}
	s.taskRunner = r
	r.Start(ctx, every)
}

// TaskJobHandlers is the daemon's side of each kind of job the task queue
// starts, by kind, for the job manager; none without a queue. Call it
// after StartTaskRunner.
func (s *Server) TaskJobHandlers() map[string]jobs.Handler {
	if s.taskSettler == nil {
		return nil
	}
	return map[string]jobs.Handler{
		taskq.MergeKind: s.taskSettler.MergeHandler(),
		taskq.TidyKind:  s.taskSettler.TidyHandler(),
	}
}
