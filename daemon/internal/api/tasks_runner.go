package api

import (
	"context"
	"errors"
	"time"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// taskHost is the manager, and the peers bus, as the task runner sees them.
type taskHost struct {
	mgr   *manager.Manager
	peers *peers.Service // nil when this daemon runs no bus
}

// TellWorker sends a worker's Claude text over the bus, then counts the
// worker as prompted from now: a turn a bus message starts fires no
// user_prompt_submit hook, and the runner must not read its last Stop as
// "finished without reporting".
func (h taskHost) TellWorker(paneID, text string) error {
	if h.peers == nil {
		return errors.New("this daemon runs no peers bus")
	}
	if err := h.peers.NotifyPane(paneID, text); err != nil {
		return err
	}
	h.mgr.NoteTaskPrompt(paneID)
	return nil
}

func (h taskHost) TaskWorkspace(repo, windowID string) (string, error) {
	return h.mgr.TaskWorkspace(repo, windowID)
}
func (h taskHost) StartTaskPane(wsID, cwd, cmd string) (string, error) {
	return h.mgr.StartTaskPane(wsID, cwd, cmd)
}
func (h taskHost) ClearTaskFolder(dir string) error { return h.mgr.ClearTaskFolder(dir) }
func (h taskHost) TasksChanged()                    { h.mgr.TasksChanged() }
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
	return taskq.PaneState{Attention: sig.Attention, Reason: sig.Reason, At: sig.At, AtShell: st.AtShell}, ok
}

// StartTaskRunner starts the task queue's runner: up to slots tasks at once,
// each built in a worktree under root, checked every `every`. It shares the
// routes' lock, so a person's edit never lands in the middle of a pass.
// slots 0 (or no task store) runs nothing: filed tasks just wait. Call it
// after EnablePeers: the runner takes the bus here, once, and without it no
// worker hears that its merge failed.
func (s *Server) StartTaskRunner(ctx context.Context, every time.Duration, slots int, root string) {
	if s.tasks == nil || slots <= 0 {
		return
	}
	r := &taskq.Runner{
		Store: s.tasks, Host: taskHost{mgr: s.mgr, peers: s.peersSvc}, Git: taskq.ExecGit{}, MergeGit: taskq.ExecGit{},
		Checks: taskq.GateChecks{PerLine: 20 * time.Minute}, Lock: &s.tasksMu,
		Slots: slots, Root: root, Now: time.Now,
	}
	s.taskRunner = r
	r.Start(ctx, every)
}
