package manager

import (
	"fmt"
	"log"
	"sync"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
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
// Saved in the pane's row, so a daemon restart does not forget it.
type TaskSignal = store.TaskSignal

type taskSignals struct {
	mu   sync.Mutex
	last map[string]TaskSignal
	save func(paneID string, sig TaskSignal) // nil in tests without a store
}

// note keeps the signal when the pane is a task worker's, and saves it.
// Under the lock, so two signals in a row are saved in the order they came.
func (s *taskSignals) note(p model.Pane, att model.Attention, reason model.AttentionReason) {
	if p.CreatedBy != taskRunner {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = map[string]TaskSignal{}
	}
	sig := TaskSignal{Attention: att, Reason: reason, At: nowMillis()}
	s.last[p.ID] = sig
	if s.save != nil {
		s.save(p.ID, sig)
	}
}

// seed loads the saved signals at start, keeping any already noted.
func (s *taskSignals) seed(stored map[string]TaskSignal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = map[string]TaskSignal{}
	}
	for id, sig := range stored {
		if _, ok := s.last[id]; !ok {
			s.last[id] = sig
		}
	}
}

// persistTaskSignal is taskSignals.save for a manager with a store.
func (m *Manager) persistTaskSignal(paneID string, sig TaskSignal) {
	if err := m.store.UpdateTaskSignal(paneID, sig); err != nil {
		log.Printf("task pane %s: signal %q not persisted (a restart forgets it): %v", paneID, sig.Attention, err)
	}
}

// loadTaskSignals seeds the workers' signals from the registry. A failure
// costs only those: every worker then starts with no signal, as before.
func (m *Manager) loadTaskSignals() {
	stored, err := m.store.TaskSignals()
	if err != nil {
		log.Printf("task signals unreadable, workers start with none: %v", err)
		return
	}
	m.taskSigs.seed(stored)
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
