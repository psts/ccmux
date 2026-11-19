// Role agents as sessions, as the manager sees them. Since task #177 a role
// agent on the claude harness is a runner session (store/agentsessions.go):
// its home is still its workspace (the one whose Agent names its base), but
// no tmux session is behind it, and its pane is a runner pane that carries
// the agent's stamp. Everything keyed on "an agent's pane" (the lifecycle
// loop, the cap, the lenses' instance list, the schedule tool) finds it by
// that stamp, as it found the tmux pane.
//
// An agent on another harness, and a claude agent that is still running in
// the tmux pane it was started in before this, keep the pane paths
// (agents.go): nothing here touches a running pane.
package manager

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

// CreateAgentHome records the workspace that is an agent instance's home
// when the agent runs as a session: the instance folder dir, the base's
// name, a place in shared window group, and no tmux session. It is closed
// from its first moment, which is all "closed" means for it: there is no
// terminal to open.
func (m *Manager) CreateAgentHome(name, createdBy, group, agentName, dir string) (*model.Workspace, error) {
	wsID := uuid.NewString()
	ws := &model.Workspace{
		ID: wsID, Name: name, RepoPath: dir, CreatedBy: createdBy, CreatedAt: nowMillis(),
		TmuxSession: model.SessionName(model.Slug(dir), wsID), Status: model.StatusCold,
		Group: group, Agent: agentName,
	}
	if err := m.store.SaveWorkspace(ws); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.byID[ws.ID] = &entry{ws: ws}
	m.mu.Unlock()
	m.events.publish(Event{Kind: "workspace-added", WorkspaceID: ws.ID})
	return ws, nil
}

// TmuxAgentPane is the first step of moving an agent's home off tmux: the
// id of the tmux pane that is the agent's ("" when the home has none), so
// its session's pane can take it over and the agent keeps its peer on the
// bus, its mailbox and its llm route. Nothing is changed here.
//
// An agent still running in its pane is ErrAgentRunning: it moves at its
// next start, never under a turn. So is a home whose tmux session is alive
// while this daemon is not attached to it, where what runs cannot be told.
func (m *Manager) TmuxAgentPane(wsID string) (string, error) {
	if m.unattachedAgentSession(wsID) {
		return "", fmt.Errorf("%w: its tmux session is alive and this daemon is not attached to it yet, so it cannot tell that the agent is asleep", ErrAgentRunning)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	e := m.byID[wsID]
	if e == nil {
		return "", fmt.Errorf("%w %s", ErrUnknownWorkspace, wsID)
	}
	pane, up := m.tmuxAgentPaneLocked(e)
	if up {
		return "", ErrAgentRunning
	}
	return pane, nil
}

// RetireAgentTmux is the last step, once the agent's session exists: the
// home's tmux session is closed and its panes are removed, whatever is in
// them. The session is the agent from here on, and a pane left behind
// would still hold the old launch line for a revive to type. Asked first
// (TmuxAgentPane), so nothing that could be an agent's turn is cut; a home
// already off tmux is left as it is.
func (m *Manager) RetireAgentTmux(wsID string) error {
	m.mu.Lock()
	e := m.byID[wsID]
	if e == nil {
		m.mu.Unlock()
		return fmt.Errorf("%w %s", ErrUnknownWorkspace, wsID)
	}
	ctrl, tmuxSession, panes := e.ctrl, e.ws.TmuxSession, e.ws.Panes
	e.ctrl = nil // detach before killing, as archiveIf does
	e.ws.Status, e.ws.Panes = model.StatusCold, nil
	group := e.ws.Group
	m.mu.Unlock()
	if len(panes) == 0 && ctrl == nil {
		return nil // already off tmux
	}
	m.forgetAgentStarts(wsID, group, panes)
	if ctrl != nil {
		ctrl.Close()
	}
	// Killed even when this daemon held no connection to it: a session left
	// alive in tmux would be adopted again at the next pass (Reconcile), and
	// the old agent in it would answer to the same name on the bus.
	if err := m.server.KillSession(tmuxSession); err != nil && ctrl != nil {
		log.Printf("agent home %s: tmux session %s not closed: %v", wsID, tmuxSession, err)
	}
	m.forgetPanes(panes)
	_ = m.store.SetWorkspaceStatus(wsID, model.StatusCold)
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
	return nil
}

// unattachedAgentSession reports a home with a tmux agent pane whose tmux
// session is alive while this daemon holds no connection to it (an attach
// that failed, healed by the next Reconcile pass). What runs in the pane is
// not known then, and closing the session could cut a turn.
func (m *Manager) unattachedAgentSession(wsID string) bool {
	m.mu.RLock()
	e := m.byID[wsID]
	name := ""
	if e != nil && e.ctrl == nil {
		if pane, _ := m.tmuxAgentPaneLocked(e); pane != "" {
			name = e.ws.TmuxSession
		}
	}
	m.mu.RUnlock()
	return name != "" && m.server != nil && m.server.HasSession(name)
}

// tmuxAgentPaneLocked is the id of a home's tmux agent pane ("" when it has
// none), and whether an agent is running in one.
func (m *Manager) tmuxAgentPaneLocked(e *entry) (id string, up bool) {
	for _, p := range e.ws.Panes {
		if p.Agent == "" {
			continue
		}
		if m.tmuxAgentUpLocked(e, p) {
			return p.ID, true
		}
		if id == "" {
			id = p.ID
		}
	}
	return id, false
}

// tmuxAgentUpLocked reports an agent pane whose harness is running in tmux,
// or being started there this moment.
func (m *Manager) tmuxAgentUpLocked(e *entry, p *model.Pane) bool {
	if m.starting.inProgress(p.ID, time.Now()) {
		return true
	}
	return e.ctrl != nil && !atBareShell(p)
}

// TmuxAgentUp reports whether workspace wsID's agent is running in a tmux
// pane right now: the one case in which a claude agent is not moved onto a
// session, and every pane path still serves it.
func (m *Manager) TmuxAgentUp(wsID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e := m.byID[wsID]
	if e == nil {
		return false
	}
	_, up := m.tmuxAgentPaneLocked(e)
	return up
}

// AgentAsleep reports whether the agent in paneID is asleep: its session
// does not want to run, or its tmux pane is back at its shell. An unknown
// pane is asleep: nothing runs there.
func (m *Manager) AgentAsleep(paneID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, p := m.findPaneLocked(paneID)
	return p == nil || agentAsleep(p)
}

// RouteLLM pins a pane's LLM traffic to account ("" leaves it to the
// harness's own order), for an agent session's start.
func (m *Manager) RouteLLM(paneID, account string) error { return m.routeLLM(paneID, account) }

// forgetPanes drops what was kept for tmux panes that are gone, and their
// rows. Not the idle clock: a pane whose id went to the agent's session
// keeps it, and the clock of any other dies with the daemon.
func (m *Manager) forgetPanes(panes []*model.Pane) {
	for _, p := range panes {
		m.restart.forget(p.ID)
		m.wakeBackoff.forget(p.ID)
		if err := m.store.DeletePane(p.ID); err != nil {
			log.Printf("pane %s: its row was not removed: %v", p.ID, err)
		}
	}
}

// agentAsleep is the one answer to "is this agent's pane asleep": a session
// whose row does not want to run, or a tmux pane back at its shell.
func agentAsleep(p *model.Pane) bool {
	if p.Runtime == model.RuntimeRunner {
		return p.Status != model.StatusLive
	}
	return atBareShell(p)
}

// stampRunnerAgent makes a session's pane say which agent it is and which
// base version it started with. Any other session's pane says neither.
func stampRunnerAgent(p *model.Pane, sess store.RunnerSession) {
	if sess.Kind != store.SessionAgent {
		return
	}
	p.Agent, p.AgentVersion = sess.Name, sess.AgentVersion
}

// seedAgentClock gives a new agent session's pane its idle clock from the
// attention its row kept. A runner pane has no row in panes, where a tmux
// agent's clock is saved, so without this every daemon restart forgot it and
// counted a working agent as idle from that moment: one with a short idle
// cap was put to sleep under its turn.
func (m *Manager) seedAgentClock(p model.Pane) {
	if p.Agent == "" {
		return
	}
	if _, ok := m.activity.get(p.ID); ok {
		return
	}
	since := p.AttentionSince
	if since == 0 {
		since = nowMillis()
	}
	m.activity.seed(map[string]store.PaneActivity{p.ID: {State: string(restoredAgentState(p)), Since: since}}, time.Now())
}

// restoredAgentState reads a restored pane's attention as the lifecycle's
// state. Unlike agentStateFor, which reads a signal as it arrives, idle
// here is a pane that claims nothing (someone dealt with it, or it never
// ran): not working.
func restoredAgentState(p model.Pane) agentState {
	switch {
	case p.Attention == model.AttentionRunning:
		return stateBusy
	case p.Attention == model.AttentionNeedsInput && p.AttentionReason != model.ReasonFinished:
		return stateWaiting
	}
	return stateIdle
}

// runnerPaneID reports whether paneID is, or was, a session's pane.
func (m *Manager) runnerPaneID(paneID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.runnerIDs[paneID]
}

// ErrNoAgentSleep is a daemon with no way to put an agent's session to
// sleep (no session supervisor wired).
var ErrNoAgentSleep = errors.New("this daemon runs no session supervisor to put the agent to sleep")

// sleepRunnerAgent puts an agent's session to sleep through the api's one
// door (SleepAgentSession), which knows whether its base starts fresh.
func (m *Manager) sleepRunnerAgent(p model.Pane, wsID, why string) error {
	if m.SleepAgentSession == nil {
		return ErrNoAgentSleep
	}
	log.Printf("agent %s (session %s): %s, putting it to sleep", p.Agent, p.SessionID, why)
	if err := m.SleepAgentSession(p.SessionID); err != nil {
		log.Printf("agent %s (session %s): not put to sleep: %v", p.Agent, p.SessionID, err)
		return err
	}
	m.activity.forget(p.ID, false)
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
	return nil
}

// runnerAgentPanes is a copy of every agent session's pane, with its home.
func (m *Manager) runnerAgentPanesLocked() []agentTarget {
	var out []agentTarget
	for _, e := range m.byID {
		for _, p := range e.runners {
			if p.Agent != "" {
				out = append(out, agentTarget{e.ws.ID, *p})
			}
		}
	}
	return out
}

// agentTarget is one agent pane the lifecycle loop looks at.
type agentTarget struct {
	wsID string
	p    model.Pane
}
