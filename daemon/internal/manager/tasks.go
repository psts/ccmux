package manager

import (
	"fmt"
	"sync"

	"ccmux.dev/ccmuxd/internal/model"
)

// TasksChanged tells every lens the task queue changed, so a page showing it
// reads the list again. The queue itself lives in the store (internal/taskq).
func (m *Manager) TasksChanged() { m.events.publish(Event{Kind: "tasks-changed"}) }

// taskRunner is the createdBy stamp on a task worker's pane.
const taskRunner = "task-runner"

// TaskWorkspace finds the session a task in repo runs in: a repo session
// (not an agent's) on this daemon, a live one first. A closed one is
// reopened, the way a teammate start reopens it, so a task filed in the
// evening still runs after its session was closed for the night.
func (m *Manager) TaskWorkspace(repo string) (string, error) {
	var closed string
	for _, ws := range m.List() {
		if ws.RepoPath != repo || ws.Agent != "" {
			continue
		}
		if m.Controller(ws.ID) != nil {
			return ws.ID, nil
		}
		if closed == "" {
			closed = ws.ID
		}
	}
	if closed == "" {
		return "", fmt.Errorf("no ccmux session for %s on this daemon", repo)
	}
	if _, err := m.ReviveWorkspace(closed); err != nil {
		return "", fmt.Errorf("reopening the session for %s: %w", repo, err)
	}
	return closed, nil
}

// StartTaskPane adds a pane in cwd (the task's worktree) and types cmd into
// it once. Ephemeral, like a teammate's: a revive brings it back as a plain
// shell instead of starting the task a second time.
func (m *Manager) StartTaskPane(wsID, cwd, cmd string) (string, error) {
	p, err := m.spawnPane(wsID, cwd, "", cmd, taskRunner, "claude", true, "")
	if err != nil {
		return "", err
	}
	return p.ID, nil
}

// ClearTaskFolder closes every task worker's pane started in dir, before a
// new worker starts there: one Claude per worktree, whatever a past run
// left behind (a failed or cancelled run's pane, or one whose start was never
// recorded). A person's own panes in that folder are left alone.
func (m *Manager) ClearTaskFolder(dir string) error {
	type target struct{ ws, pane string }
	var found []target
	m.mu.RLock()
	for _, e := range m.byID {
		for _, p := range e.ws.Panes {
			if p.CreatedBy == taskRunner && p.CWD == dir {
				found = append(found, target{e.ws.ID, p.ID})
			}
		}
	}
	m.mu.RUnlock()
	for _, f := range found {
		if err := m.KillPane(f.ws, f.pane); err != nil {
			return fmt.Errorf("closing the last run's pane: %w", err)
		}
	}
	return nil
}

// TaskSignal is a worker's last hook signal, kept apart from the pane's
// attention: a lens looking at the session retires that attention to idle
// (attention_seen.go), and the runner must still see what the worker said.
type TaskSignal struct {
	Attention model.Attention
	Reason    model.AttentionReason
	At        int64 // unix ms
}

type taskSignals struct {
	mu   sync.Mutex
	last map[string]TaskSignal
}

// note keeps the signal when the pane is a task worker's.
func (s *taskSignals) note(p model.Pane, att model.Attention, reason model.AttentionReason) {
	if p.CreatedBy != taskRunner {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = map[string]TaskSignal{}
	}
	s.last[p.ID] = TaskSignal{Attention: att, Reason: reason, At: nowMillis()}
}

func (s *taskSignals) get(paneID string) TaskSignal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last[paneID]
}

// TaskPaneStatus is what the task runner reads off a worker's pane.
type TaskPaneStatus struct {
	Signal TaskSignal
	// AtShell is true once the pane's foreground is a shell again: Claude
	// exited, or never started.
	AtShell bool
}

// TaskPane reports a worker pane's state. False when no LIVE session holds
// it: a closed session keeps its panes on record, but its Claude is gone.
func (m *Manager) TaskPane(paneID string) (TaskPaneStatus, bool) {
	m.mu.RLock()
	e, p := m.findPaneLocked(paneID)
	live := p != nil && e.ctrl != nil
	atShell := live && atBareShell(p)
	m.mu.RUnlock()
	if !live {
		return TaskPaneStatus{}, false
	}
	return TaskPaneStatus{Signal: m.taskSigs.get(paneID), AtShell: atShell}, true
}

// WindowPeers lists the workspaces sharing a ccmux window with paneID's
// session (that session too): the repos a task worker may pull into its
// feature. A session in no window has only itself.
func (m *Manager) WindowPeers(paneID string) []*model.Workspace {
	st := m.windowSnapshot()
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, _ := m.findPaneLocked(paneID)
	if e == nil {
		return nil
	}
	group := st.resolve(e.ws.ID, e.ws.Group)
	var out []*model.Workspace
	for _, o := range m.byID {
		same := o.ws.ID == e.ws.ID || (group != "" && st.resolve(o.ws.ID, o.ws.Group) == group)
		if same {
			out = append(out, o.ws)
		}
	}
	return out
}
