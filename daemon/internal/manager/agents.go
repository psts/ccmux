package manager

import (
	"fmt"

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

// SpawnAgentPane adds a new pane running an agent instance. The persisted
// startup command has no prompt (a revive must not replay a message); the
// typed one carries the first prompt when there is one.
func (m *Manager) SpawnAgentPane(wsID, createdBy string, l AgentLaunch) (*model.Pane, error) {
	p, err := m.spawnPane(wsID, l.CWD, l.Persist, l.Deliver, createdBy, l.Harness.Name, l.Harness.Autoconfirm, l.RouteAccount)
	if err != nil {
		return nil, err
	}
	return m.stampAgent(p.ID, l)
}

// StartAgentInPane restarts an instance whose pane sits at a bare shell — the
// "asleep" agent a human or a peer just contacted. Refuses a busy pane with
// ErrPaneBusy like a harness start does.
func (m *Manager) StartAgentInPane(paneID string, l AgentLaunch) error {
	if err := m.startInPane(paneID, l.Harness, l.Persist, l.Deliver, l.RouteAccount); err != nil {
		return err
	}
	_, err := m.stampAgent(paneID, l)
	return err
}

// stampAgent records the base name and version on the pane after a start.
func (m *Manager) stampAgent(paneID string, l AgentLaunch) (*model.Pane, error) {
	m.mu.Lock()
	e, p := m.findPaneLocked(paneID)
	if p == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("unknown pane %s", paneID)
	}
	p.Agent, p.AgentVersion = l.Name, l.Version
	saved := *p
	wsID := e.ws.ID
	m.mu.Unlock()
	if err := m.store.SavePane(&saved); err != nil {
		return nil, err
	}
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
	return &saved, nil
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
