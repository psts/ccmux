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

// TodosChanged tells every lens a repo's to-do list changed (internal/todos),
// so a page showing the lists reads them again.
func (m *Manager) TodosChanged() { m.events.publish(Event{Kind: "todos-changed"}) }

// taskRunner is the createdBy stamp on a task worker's pane: the tmux pane
// of a worker started before task #176, and the pane of a builder session
// since (builders.go).
const taskRunner = "task-runner"

// TaskWorkspace finds the session a task in repo runs in: a repo session
// (not an agent's) on this daemon, one in the task's window before one in
// another (the repo may have moved since the task was filed), and a live one
// before a closed one. A closed one is reopened, the way a teammate start
// reopens it, so a task filed in the evening still runs after its session
// was closed for the night.
func (m *Manager) TaskWorkspace(repo, windowID string) (string, error) {
	best, err := m.taskSession(repo, windowID)
	if err != nil {
		return "", err
	}
	if m.Controller(best) != nil {
		return best, nil
	}
	if _, err := m.ReviveWorkspace(best); err != nil {
		return "", fmt.Errorf("reopening the session for %s: %w", repo, err)
	}
	return best, nil
}

// taskSession picks the session a task in repo runs in, live or closed.
func (m *Manager) taskSession(repo, windowID string) (string, error) {
	members, err := m.windowMembers()
	if err != nil {
		return "", fmt.Errorf("reading which window each session is in: %w", err)
	}
	best, bestRank := "", -1
	for _, ws := range m.List() {
		if ws.RepoPath != repo || ws.Agent != "" {
			continue
		}
		if rank := m.taskSessionRank(ws.ID, windowID, members); rank > bestRank {
			best, bestRank = ws.ID, rank
		}
	}
	if best == "" {
		return "", fmt.Errorf("no ccmux session for %s on this daemon", repo)
	}
	return best, nil
}

// taskSessionRank orders a task's candidate sessions: its own window counts
// more than being live.
func (m *Manager) taskSessionRank(wsID, windowID string, members map[string]string) int {
	rank := 0
	if windowID != "" && members[wsID] == windowID {
		rank += 2
	}
	if m.Controller(wsID) != nil {
		rank++
	}
	return rank
}

// WindowOfWorkspace is the id of the shared window holding wsID, "" for a
// session in no window. Read from the store, not the cached snapshot, which
// serves empty after a failed read: filing must never take that for "no
// window".
func (m *Manager) WindowOfWorkspace(wsID string) (string, error) {
	members, err := m.windowMembers()
	if err != nil {
		return "", err
	}
	return members[wsID], nil
}

func (m *Manager) windowMembers() (map[string]string, error) {
	if m.store == nil {
		return map[string]string{}, nil
	}
	return m.store.WindowMembers()
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

// ClearTaskFolder closes every task worker in dir, before a new worker
// starts there: one Claude per worktree, whatever a past run left behind (a
// failed or cancelled run's worker, or one whose start was never recorded).
// A worker is a builder session that works in dir (builders.go), or, from
// before task #176, a tmux pane the queue started there. A person's own
// panes in that folder are left alone.
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
	return m.endBuildersIn(dir)
}

// TaskSignal is a worker's last hook signal, kept apart from the pane's
// attention: a lens looking at the session retires that attention to idle
// (attention_seen.go), and the runner must still see what the worker said.
// Saved in the pane's row, so a daemon restart does not forget it.
type TaskSignal = store.TaskSignal

type taskSignals struct {
	mu   sync.Mutex
	last map[string]TaskSignal
	save func(p model.Pane, sig TaskSignal) // nil in tests without a store
}

// put changes a task worker's last signal: change is given the one kept
// and returns the one to keep, or false to keep it. A pane that is no task
// worker's keeps none. Under the lock, so two signals in a row are judged
// and saved in the order they came.
func (s *taskSignals) put(p model.Pane, change func(last TaskSignal) (TaskSignal, bool)) {
	if p.CreatedBy != taskRunner {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = map[string]TaskSignal{}
	}
	sig, ok := change(s.last[p.ID])
	if !ok {
		return
	}
	sig.At = nowMillis()
	s.last[p.ID] = sig
	if s.save != nil {
		s.save(p, sig)
	}
}

// note keeps the signal when the pane is a task worker's, and saves it.
func (s *taskSignals) note(p model.Pane, att model.Attention, reason model.AttentionReason) {
	s.put(p, func(TaskSignal) (TaskSignal, bool) { return TaskSignal{Attention: att, Reason: reason}, true })
}

// heard keeps what a worker's Claude just said of itself. A tmux worker's
// hooks say it the way the queue's watchdog reads it; a builder session's
// runner says more, and some of it differently (builderSignal).
func (s *taskSignals) heard(p model.Pane, att model.Attention, reason model.AttentionReason) {
	if p.Runtime != model.RuntimeRunner {
		s.note(p, att, reason)
		return
	}
	s.put(p, func(last TaskSignal) (TaskSignal, bool) { return builderSignal(last, att, reason) })
}

// remind keeps Claude's idle reminder as a worker's last signal: the proof
// that its turn really ended, which a Stop alone is not. A worker that is
// asking something keeps that instead: the reminder only comes at the
// prompt, but if one ever arrived late it must not take a question off the
// list of what needs a person.
func (s *taskSignals) remind(p model.Pane) {
	s.put(p, func(last TaskSignal) (TaskSignal, bool) {
		if last.Attention == model.AttentionNeedsInput && last.Reason != model.ReasonFinished {
			return last, false
		}
		return TaskSignal{Attention: model.AttentionNeedsInput, Reason: model.ReasonFinished}, true
	})
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

// persistTaskSignal is taskSignals.save for a manager with a store: on the
// pane's row for a tmux worker, on the session's row for a builder, whose
// pane has no row.
func (m *Manager) persistTaskSignal(p model.Pane, sig TaskSignal) {
	var err error
	if p.Runtime == model.RuntimeRunner {
		err = m.store.UpdateRunnerTaskSignal(p.ID, sig)
	} else {
		err = m.store.UpdateTaskSignal(p.ID, sig)
	}
	if err != nil {
		log.Printf("task pane %s: signal %q not persisted (a restart forgets it): %v", p.ID, sig.Attention, err)
	}
}

// loadTaskSignals seeds the workers' signals from the registry, the tmux
// workers' and the builders'. A failure costs only those: every such
// worker then starts with no signal, as before.
func (m *Manager) loadTaskSignals() {
	stored, err := m.store.TaskSignals()
	if err != nil {
		log.Printf("task signals unreadable, workers start with none: %v", err)
	}
	m.taskSigs.seed(stored)
	builders, err := m.store.RunnerTaskSignals()
	if err != nil {
		log.Printf("builders' task signals unreadable, they start with none: %v", err)
	}
	m.taskSigs.seed(builders)
}

// NoteTaskPrompt records that a prompt went into a task worker's pane from
// outside its hooks (the runner's message after a failed merge), as a typed
// prompt would: the runner then counts the worker as busy from now, not as
// finished since its last Stop. A bus message starts a turn with no
// user_prompt_submit hook, so without this the old Stop would stand.
//
// For a builder session it is also how "a person typed" is known at all:
// its user_prompt_submit hook is not heard (ApplyAttention), so the route
// that queues a person's prompt for it says so here.
func (m *Manager) NoteTaskPrompt(paneID string) {
	m.mu.RLock()
	_, p := m.findPaneLocked(paneID)
	var saved model.Pane
	if p != nil {
		saved = *p
	}
	m.mu.RUnlock()
	if p != nil {
		m.taskSigs.note(saved, model.AttentionIdle, model.ReasonNone)
	}
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
	// exited, or never started. For a builder session, which has no shell,
	// it is true once the session is asleep (builderGone).
	AtShell bool
}

// TaskPane reports a worker pane's state. False when no LIVE session holds
// it: a closed tmux session keeps its panes on record, but its Claude is
// gone; a builder session's pane is there until the session has ended.
func (m *Manager) TaskPane(paneID string) (TaskPaneStatus, bool) {
	m.mu.RLock()
	e, p := m.findPaneLocked(paneID)
	live, atShell := false, false
	switch {
	case p == nil:
	case p.Runtime == model.RuntimeRunner:
		live, atShell = true, builderGone(p)
	default:
		live = e.ctrl != nil
		atShell = live && atBareShell(p)
	}
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
