package manager

import (
	"errors"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

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
	ErrAgentRunning = errors.New("agent is already running in this window")
	ErrAgentCap     = errors.New("agent concurrency cap reached")
	// ErrNotAgent refuses an agent-only action (sleep) on an ordinary pane.
	ErrNotAgent = errors.New("not an agent pane")
)

// CreateAgentWorkspace opens the session that IS an agent instance in shared
// window group: its first pane runs the launch, its RepoPath is the instance
// folder (l.CWD), and the workspace carries the base name so a window lists
// it as that agent. The double-start guard is by window and base for the
// 5s start window (there is no pane to confirm against until tmux reports
// the harness), then by the new workspace like every other spawn.
func (m *Manager) CreateAgentWorkspace(name, createdBy, group string, l AgentLaunch) (*model.Workspace, error) {
	m.agentStartMu.Lock()
	defer m.agentStartMu.Unlock()
	now := time.Now()
	if m.starting.inProgress(spawnKey("window:"+group, l.Name), now) {
		return nil, ErrAgentRunning
	}
	if m.overAgentCap() {
		return nil, ErrAgentCap
	}
	wsID := uuid.NewString()
	pane0 := m.newPane(wsID, l.CWD, l.Persist, createdBy)
	pane0.Harness = l.Harness.Name
	pane0.Agent, pane0.AgentVersion = l.Name, l.Version
	if err := m.routeLLM(pane0.ID, l.RouteAccount); err != nil {
		return nil, err
	}
	sessionName := model.SessionName(model.Slug(l.CWD), wsID)
	ctrl, err := m.openSession(wsID, sessionName, l.CWD, pane0)
	if err != nil {
		return nil, err
	}
	m.deliverStartup(ctrl, pane0.ID, l.Deliver, l.Harness.Autoconfirm)
	ws := &model.Workspace{
		ID: wsID, Name: name, RepoPath: l.CWD, CreatedBy: createdBy,
		CreatedAt: nowMillis(), TmuxSession: sessionName, Status: model.StatusLive,
		Group: group, Agent: l.Name, Panes: []*model.Pane{pane0},
	}
	if err := m.registerWorkspace(ws, ctrl); err != nil {
		// The harness is already up; a session no lens lists and no cap
		// counts must not keep running.
		ctrl.Close()
		if kerr := m.server.KillSession(sessionName); kerr != nil {
			log.Printf("agent %s: tmux session %s left running after a failed register: %v", l.Name, sessionName, kerr)
		}
		return nil, err
	}
	m.starting.mark(spawnKey("window:"+group, l.Name), now)
	m.starting.mark(spawnKey(wsID, l.Name), now)
	return ws, nil
}

// ReviveAgentWorkspace brings an archived agent session back with the
// CURRENT launch, not the recipe it was archived with: the pane is stamped
// with the new base version and persisted line first, and the replay types
// the delivery form (launch plus first prompt) into it. Guarded like every
// other start: marks by pane and by session, and the cap.
func (m *Manager) ReviveAgentWorkspace(wsID string, l AgentLaunch) (*model.Pane, error) {
	m.agentStartMu.Lock()
	defer m.agentStartMu.Unlock()
	now := time.Now()
	p := m.AgentPane(wsID, l.Name)
	if p == nil {
		return nil, ErrNotAgent
	}
	if m.starting.inProgress(p.ID, now) || m.starting.inProgress(spawnKey(wsID, l.Name), now) {
		return nil, ErrAgentRunning
	}
	if m.overAgentCap() {
		return nil, ErrAgentCap
	}
	if err := m.routeLLM(p.ID, l.RouteAccount); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if _, live := m.findPaneLocked(p.ID); live != nil {
		live.StartupCommand, live.Harness = l.Persist, l.Harness.Name
		live.Agent, live.AgentVersion = l.Name, l.Version
	}
	m.mu.Unlock()
	if _, err := m.reviveWorkspace(wsID, map[string]string{p.ID: l.Deliver}); err != nil {
		return nil, err
	}
	m.starting.mark(p.ID, now)
	m.starting.mark(spawnKey(wsID, l.Name), now)
	return m.AgentPane(wsID, l.Name), nil
}

// forgetAgentStarts clears the start marks of a session being archived or
// deleted: a wake typed moments before is not in flight any more, and a
// revive or a fresh add right after must not read it as one. group is the
// session's window, whose own add mark goes too.
func (m *Manager) forgetAgentStarts(wsID, group string, panes []*model.Pane) {
	for _, p := range panes {
		m.starting.forget(p.ID)
		if p.Agent != "" {
			m.starting.forget(spawnKey(wsID, p.Agent))
			m.starting.forget(spawnKey("window:"+group, p.Agent))
		}
	}
}

// SleepAgent ends a running instance's harness session the way idle exit
// does (ctrl-d at its prompt): the pane drops to its shell with history
// intact, and the next message wakes it. Refuses a non-agent pane; an
// instance already asleep is left alone; a keystroke that did not go in is
// an error, never a silent 204.
func (m *Manager) SleepAgent(paneID string) error {
	m.mu.RLock()
	e, p := m.findPaneLocked(paneID)
	if p == nil || p.Agent == "" {
		m.mu.RUnlock()
		return ErrNotAgent
	}
	wsID, asleep := e.ws.ID, atBareShell(p)
	m.mu.RUnlock()
	if asleep {
		return nil
	}
	return m.exitAgent(paneID, wsID, "asked to sleep")
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

// confirm clears the marks for a pane once tmux reports a HARNESS in its
// foreground: from here on the ordinary liveness checks tell the truth. A
// shell report proves nothing — a fresh pane says "zsh" before the typed
// command execs — so it leaves the marks standing.
func (s *startMarks) confirm(wsID string, p *model.Pane) {
	if atBareShell(p) {
		return
	}
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
