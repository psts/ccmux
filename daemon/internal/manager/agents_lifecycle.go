package manager

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/session"
)

// agentActivity is the daemon's view of whether an agent pane is busy, from
// the harness's own signals: opencode's ccmux plugin posts busy/idle to
// /v1/panes/{id}/agent-signal; a Claude Code pane's attention changes arrive
// through the hooks socket (ApplyAttention). Both land here with a time, so
// the lifecycle loop can ask "idle since when".
type agentActivity struct {
	mu    sync.Mutex
	state map[string]activity
}

type activity struct {
	busy  bool
	since time.Time
}

func (a *agentActivity) note(paneID string, busy bool, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state == nil {
		a.state = map[string]activity{}
	}
	if cur, ok := a.state[paneID]; ok && cur.busy == busy {
		return // same state: keep the original since
	}
	a.state[paneID] = activity{busy: busy, since: now}
}

func (a *agentActivity) get(paneID string) (activity, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	act, ok := a.state[paneID]
	return act, ok
}

func (a *agentActivity) forget(paneID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.state, paneID)
}

// NoteAgentSignal records a busy/idle signal for a pane (the plugin route).
// Unknown panes are refused so a stray POST cannot grow the map.
func (m *Manager) NoteAgentSignal(paneID string, busy bool) bool {
	m.mu.RLock()
	_, p := m.findPaneLocked(paneID)
	m.mu.RUnlock()
	if p == nil {
		return false
	}
	m.activity.note(paneID, busy, time.Now())
	return true
}

// agentAction is what one lifecycle tick decides for one agent pane.
type agentAction int

const (
	actNone agentAction = iota
	actExit             // idle past the cap with no open work: end the session
	actWake             // asleep and keep-alive (or drifted): start it again
)

// agentView is the slice of pane + policy the decision reads.
type agentView struct {
	atShell   bool
	idle      bool
	idleSince time.Time
	openTasks int
	keepAlive bool
	idleExit  time.Duration
	drift     bool
}

// decideAgent is the pure rule of the loop, tested on its own:
//   - at a shell: wake when keep-alive, otherwise leave it asleep;
//   - running and idle past idleExit with no open delegations: exit — a
//     keep-alive instance that drifted from its base exits too, so the wake
//     on the next tick picks the new base up;
//   - anything else: nothing.
func decideAgent(v agentView, now time.Time) agentAction {
	if v.atShell {
		if v.keepAlive {
			return actWake
		}
		return actNone
	}
	if !v.idle || v.openTasks > 0 || v.idleExit <= 0 {
		return actNone
	}
	if now.Sub(v.idleSince) < v.idleExit {
		return actNone
	}
	if v.keepAlive && !v.drift {
		return actNone // keep-alive agents stay up unless the base moved
	}
	return actExit
}

// StartAgentLifecycle runs the tick loop until ctx ends. Needs m.Agents (the
// base store) and, for wakes, m.WakeAgent (wired by the api layer, which
// owns launch resolution); m.OpenTasksForPane is optional and defaults to 0.
func (m *Manager) StartAgentLifecycle(ctx context.Context, every time.Duration) {
	if m.Agents == nil {
		return
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				m.agentTick(now)
			}
		}
	}()
}

// agentTick applies decideAgent to every agent pane in a live workspace.
func (m *Manager) agentTick(now time.Time) {
	type target struct {
		wsID string
		p    model.Pane
	}
	var targets []target
	m.mu.RLock()
	for _, e := range m.byID {
		if e.ctrl == nil {
			continue
		}
		for _, p := range e.ws.Panes {
			if p.Agent != "" {
				targets = append(targets, target{e.ws.ID, *p})
			}
		}
	}
	m.mu.RUnlock()
	for _, tg := range targets {
		m.applyAgentTick(tg.wsID, tg.p, now)
	}
}

func (m *Manager) applyAgentTick(wsID string, p model.Pane, now time.Time) {
	d, err := m.Agents.Get(p.Agent)
	if errors.Is(err, agent.ErrNotFound) {
		return // base deleted: the pane is just a pane now
	}
	if err != nil {
		log.Printf("agent %s pane %s: base unreadable, lifecycle skipped this tick: %v", p.Agent, p.ID, err)
		return
	}
	switch decideAgent(m.agentViewFor(p, d, now), now) {
	case actExit:
		m.exitAgent(p.ID, wsID)
	case actWake:
		m.wakeWithBackoff(wsID, p, now)
	}
}

// agentViewFor assembles what decideAgent reads for one pane: the pane's
// shell state, the base's policy, drift, the harness's busy/idle signal (or
// the attention fallback before the first signal) and open delegations.
func (m *Manager) agentViewFor(p model.Pane, d agent.Definition, now time.Time) agentView {
	v := agentView{
		atShell:   atBareShell(&p),
		keepAlive: d.KeepAlive,
		idleExit:  time.Duration(d.IdleExitMinutes) * time.Minute,
		drift:     agent.Drifted(p.AgentVersion, d.Version),
	}
	if act, ok := m.activity.get(p.ID); ok {
		v.idle, v.idleSince = !act.busy, act.since
	} else if p.Attention != "" && !claudeBusy(p.Attention) {
		// No harness signal yet (a Claude pane before its first hook, or a
		// plugin-less harness): fall back to the pane's attention, counting
		// from the moment we first saw it not working.
		m.activity.note(p.ID, false, now)
		v.idle, v.idleSince = true, now
	}
	if m.OpenTasksForPane != nil {
		v.openTasks = m.OpenTasksForPane(p.ID)
	}
	return v
}

// wakeWithBackoff retries a keep-alive wake with a doubling wait (one tick up
// to an hour) so a wake that keeps failing — cap reached, harness missing,
// launch line that exits at once — does not retype into the pane every 30
// seconds for the life of the daemon.
func (m *Manager) wakeWithBackoff(wsID string, p model.Pane, now time.Time) {
	if m.WakeAgent == nil || !m.wakeBackoff.due(p.ID, now) {
		return
	}
	err := m.WakeAgent(wsID, p.Agent)
	if err != nil && !errors.Is(err, ErrAgentRunning) {
		wait := m.wakeBackoff.failed(p.ID, now)
		log.Printf("agent %s: keep-alive wake failed (%v); next try in %s", p.Agent, err, wait)
		return
	}
	// Started, or someone else's start is in flight: either way not a failure.
	m.wakeBackoff.forget(p.ID)
}

// wakeBackoff is the per-pane retry schedule for keep-alive wakes.
type wakeBackoff struct {
	mu   sync.Mutex
	next map[string]wakeState
}

type wakeState struct {
	fails int
	at    time.Time
}

const (
	wakeRetryMin = 30 * time.Second
	wakeRetryMax = time.Hour
)

func (b *wakeBackoff) due(paneID string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.next[paneID]
	return !ok || !now.Before(st.at)
}

func (b *wakeBackoff) failed(paneID string, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.next == nil {
		b.next = map[string]wakeState{}
	}
	st := b.next[paneID]
	st.fails++
	wait := wakeRetryMin << uint(st.fails-1)
	if wait > wakeRetryMax || wait <= 0 {
		wait = wakeRetryMax
	}
	st.at = now.Add(wait)
	b.next[paneID] = st
	return wait
}

func (b *wakeBackoff) forget(paneID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.next, paneID)
}

// claudeBusy reads a Claude Code pane's attention as the lifecycle's
// busy/idle. The hooks encode attention for the LENS, not for us, and the
// mapping is easy to invert: user_prompt_submit → "idle" (the flash clears
// because the human is there), which for an agent is the moment work STARTS;
// stop → "done" and permission/ask → "needs_input" are the moments it stops.
// So "idle" is busy and everything else is not; see hooks.outcome for the
// mapping this reads.
func claudeBusy(att model.Attention) bool { return att == model.AttentionIdle }

// exitAgent ends the harness session with an end-of-input: Claude Code and
// opencode both quit on ctrl-d at their prompt, which is where an idle
// agent sits. The pane drops to its shell and stays (history intact).
func (m *Manager) exitAgent(paneID, wsID string) {
	m.mu.RLock()
	e, p := m.findPaneLocked(paneID)
	var ctrl *session.Controller
	if e != nil && p != nil {
		ctrl = e.ctrl
	}
	m.mu.RUnlock()
	if ctrl == nil {
		return // pane gone between the tick's snapshot and now
	}
	log.Printf("agent pane %s: idle past its cap, ending the session", paneID)
	if err := ctrl.SendInput(paneID, []byte{0x04}); err != nil {
		log.Printf("agent pane %s: exit keystroke failed: %v", paneID, err)
		return
	}
	m.activity.forget(paneID)
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
}
