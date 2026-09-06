package manager

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/model"
)

// AgentLaunch is a resolved instance start: which base and version the pane
// becomes, the harness it runs, the two command forms (internal/agent.Launch)
// and the folder and llm account the pane gets.
type AgentLaunch struct {
	Name, Version    string
	Harness          harness.Harness
	Persist, Deliver string
	CWD              string
	RouteAccount     string
	// Prompt is the first message; Port the opencode server port it is pushed
	// through after the launch (0 for harnesses that take it positionally,
	// where Deliver already carries it).
	Prompt string
	Port   int
}

// ErrAgentRunning refuses a second instance of one base in one workspace;
// ErrAgentCap refuses a start that would exceed AgentsMax running agents.
// Both are checked here, under agentStartMu, so two concurrent starts (an
// HTTP click and a bus contact for the same name) cannot both pass.
var (
	ErrAgentRunning = errors.New("agent is already running in this workspace")
	ErrAgentCap     = errors.New("agent concurrency cap reached")
)

// SpawnAgentPane adds a new pane running an agent instance. The persisted
// startup command has no prompt (a revive must not replay a message); the
// typed one carries the first prompt when there is one.
func (m *Manager) SpawnAgentPane(wsID, createdBy string, l AgentLaunch) (*model.Pane, error) {
	m.agentStartMu.Lock()
	defer m.agentStartMu.Unlock()
	if p := m.AgentPane(wsID, l.Name); p != nil && !atBareShell(p) {
		return nil, ErrAgentRunning
	}
	if m.starting.inProgress(spawnKey(wsID, l.Name), time.Now()) {
		return nil, ErrAgentRunning
	}
	if m.overAgentCap() {
		return nil, ErrAgentCap
	}
	p, err := m.spawnPane(wsID, l.CWD, l.Persist, l.Deliver, createdBy, l.Harness.Name, l.Harness.Autoconfirm, l.RouteAccount)
	if err != nil {
		return nil, err
	}
	m.starting.mark(spawnKey(wsID, l.Name), time.Now())
	stamped := m.stampAgent(p.ID, l)
	if stamped == nil {
		return nil, fmt.Errorf("agent %s: pane %s vanished right after spawn", l.Name, p.ID)
	}
	return stamped, nil
}

// StartAgentInPane restarts an instance whose pane sits at a bare shell — the
// "asleep" agent a human or a peer just contacted. Refuses a busy pane with
// ErrPaneBusy like a harness start does, and the cap like SpawnAgentPane.
func (m *Manager) StartAgentInPane(paneID string, l AgentLaunch) error {
	m.agentStartMu.Lock()
	defer m.agentStartMu.Unlock()
	// A wake typed moments ago has not yet changed tmux's foreground command,
	// so "at a shell" would still read true: the marker is what says "in
	// progress" until tmux reports the harness (or the window lapses).
	wsID := m.WorkspaceForPane(paneID)
	if m.starting.inProgress(paneID, time.Now()) || m.starting.inProgress(spawnKey(wsID, l.Name), time.Now()) {
		return ErrAgentRunning
	}
	if m.overAgentCap() {
		return ErrAgentCap
	}
	if err := m.startInPane(paneID, l.Harness, l.Persist, l.Deliver, l.RouteAccount); err != nil {
		return err
	}
	m.starting.mark(paneID, time.Now())
	m.stampAgent(paneID, l)
	return nil
}

func (m *Manager) overAgentCap() bool {
	return m.AgentsMax > 0 && m.RunningAgents() >= m.AgentsMax
}

// spawnKey is the start mark for a fresh spawn: the pane does not exist yet,
// so the mark is by workspace and base. dropPane prunes it with the pane.
func spawnKey(wsID, name string) string { return wsID + "|" + name }

// startMarks remembers starts typed within the last startWindow, keyed by
// pane (wakes) or by workspace|name (spawns): the liveness every other guard
// reads comes from tmux's async foreground signal, which lags the keystroke
// by the harness's exec time — long enough for a second start to slip in.
type startMarks struct {
	mu sync.Mutex
	at map[string]time.Time
}

// startWindow only has to outlast tmux's foreground report for the typed
// launch (hundreds of ms); confirm clears the mark the moment that arrives,
// so the window is a safety net, not the normal path.
const startWindow = 5 * time.Second

func (s *startMarks) inProgress(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.at[key]
	return ok && now.Sub(t) < startWindow
}

func (s *startMarks) mark(key string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at == nil {
		s.at = map[string]time.Time{}
	}
	s.at[key] = now
}

// confirm clears the marks for a pane once tmux reported its foreground
// command: from here on the ordinary liveness checks tell the truth.
func (s *startMarks) confirm(wsID string, p *model.Pane) {
	s.forget(p.ID)
	if p.Agent != "" {
		s.forget(spawnKey(wsID, p.Agent))
	}
}

func (s *startMarks) forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.at, key)
}

// stampAgent records the base name and version on the pane after a start.
// The harness is already running by now, so a failed persist is logged and
// the live pane still carries the stamp: reporting "could not be started"
// for an agent that IS running would be the bigger lie.
func (m *Manager) stampAgent(paneID string, l AgentLaunch) *model.Pane {
	m.mu.Lock()
	e, p := m.findPaneLocked(paneID)
	if p == nil {
		m.mu.Unlock()
		return nil
	}
	p.Agent, p.AgentVersion = l.Name, l.Version
	saved := *p
	wsID := e.ws.ID
	m.mu.Unlock()
	if err := m.store.SavePane(&saved); err != nil {
		log.Printf("agent %s pane %s: stamp not persisted (%v); a daemon restart forgets it is an agent pane", l.Name, paneID, err)
	}
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
	return &saved
}

// AgentPane returns a copy of the pane that is workspace wsID's instance of
// agent name, or nil when the agent was never added there.
func (m *Manager) AgentPane(wsID, name string) *model.Pane {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e := m.byID[wsID]
	if e == nil {
		return nil
	}
	for _, p := range e.ws.Panes {
		if p.Agent == name {
			cp := *p
			return &cp
		}
	}
	return nil
}

// RunningAgents counts agent panes whose foreground is not a bare shell
// across live workspaces — the number the concurrency cap is checked
// against. An agent pane at a shell is asleep and costs nothing.
func (m *Manager) RunningAgents() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, e := range m.byID {
		if e.ctrl == nil {
			continue
		}
		for _, p := range e.ws.Panes {
			if p.Agent != "" && !atBareShell(p) {
				n++
			}
		}
	}
	return n
}
