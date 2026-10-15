package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/session"
)

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
	idle      bool // not working: idle, or waiting on a human
	waiting   bool // stopped on a human (a card, a permission prompt)
	idleSince time.Time
	openTasks int
	keepAlive bool
	idleExit  time.Duration
	cardWait  time.Duration // 0: a waiting agent follows idleExit
	// holdsDialog: the harness ignores the exit keystroke while a dialog is
	// open (opencode), so ending a waiting session would only leave a
	// half-dead pane. Claude closes the card and exits.
	holdsDialog bool
	drift       bool
}

// mayEnd says whether this session is a candidate for ending at all: not
// working, no open delegation, a cap configured, and not a dialog the
// harness would ignore ctrl-d under.
func (v agentView) mayEnd() bool {
	return v.idle && v.openTasks == 0 && v.idleExit > 0 && !(v.waiting && v.holdsDialog)
}

// limit is how long this stretch may last before the session ends. A card
// gets its own wait so it can stay open longer than an idle session lives;
// without one it follows the idle cap, which is how it always behaved.
func (v agentView) limit() time.Duration {
	if v.waiting && v.cardWait > 0 {
		return v.cardWait
	}
	return v.idleExit
}

// decideAgent is the pure rule of the loop, tested on its own:
//   - at a shell: wake when keep-alive, otherwise leave it asleep;
//   - running and idle past its limit (idleExit, or cardWait while it waits
//     on a human) with no open delegations: exit — a
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
	if !v.mayEnd() {
		return actNone
	}
	if now.Sub(v.idleSince) < v.limit() {
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
		_ = m.exitAgent(p.ID, wsID, "idle past its cap") // logged inside; the next tick retries
	case actWake:
		m.wakeWithBackoff(wsID, p, now)
	}
}

// agentViewFor assembles what decideAgent reads for one pane: the pane's
// shell state, the base's policy, drift, the harness's state and open
// delegations.
func (m *Manager) agentViewFor(p model.Pane, d agent.Definition, now time.Time) agentView {
	v := agentView{
		atShell:     atBareShell(&p),
		keepAlive:   d.KeepAlive,
		idleExit:    time.Duration(d.IdleExitMinutes) * time.Minute,
		cardWait:    time.Duration(d.CardWaitMinutes) * time.Minute,
		holdsDialog: paneHarness(p) != harness.Builtin,
		drift:       agent.Drifted(p.AgentVersion, d.Version),
	}
	if v.atShell {
		// The session is gone: its clock with it, so the next start counts
		// from its own first reading and not from the last session's.
		m.activity.forget(p.ID, true)
		return v
	}
	if act, ok := m.readingFor(p, now); ok {
		v.idle, v.waiting, v.idleSince = act.state != stateBusy, act.state == stateWaiting, act.since
	}
	if m.OpenTasksForPane != nil {
		v.openTasks = m.OpenTasksForPane(p.ID)
	}
	return v
}

// readingFor is the pane's state for the lifecycle, false when there is
// nothing to act on.
//
// With no reading, a harness that reports its own state (agent.HasChat:
// Claude through its hooks or sidecar, opencode through its plugin) counts
// as idle from the first tick that sees it. That is the safe guess for
// them: a working one reports within its turn, while a guess of busy, which
// reading the pane's attention gave after a restart, never expires. Any
// other harness (pi, codex, aider) never reports, so no reading is all it
// will ever have; reading that as idle would end it mid-task every cap, so
// it is never ended blindly.
func (m *Manager) readingFor(p model.Pane, now time.Time) (activity, bool) {
	if act, ok := m.activity.current(p.ID, now); ok {
		return act, true
	}
	if !agent.HasChat(paneHarness(p)) {
		return activity{}, false
	}
	return m.activity.note(p.ID, stateIdle, now), true
}

// paneHarness is the harness an agent pane runs, with "" (a row from before
// the harness column, when only Claude could run one) read as the builtin.
func paneHarness(p model.Pane) string {
	if p.Harness == "" {
		return harness.Builtin
	}
	return p.Harness
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

// ErrPaneGone says the pane (or its session) vanished before an action
// reached it — the tick's snapshot was stale.
var ErrPaneGone = errors.New("pane is gone")

// exitAgent ends the harness session with an end-of-input: Claude Code and
// opencode both quit on ctrl-d at their prompt, which is where an idle
// agent sits. The pane drops to its shell and stays (history intact). The
// error says whether the keystroke went in; the lifecycle loop only logs it,
// a human's sleep request answers with it.
func (m *Manager) exitAgent(paneID, wsID, why string) error {
	m.mu.RLock()
	e, p := m.findPaneLocked(paneID)
	var ctrl *session.Controller
	if e != nil && p != nil {
		ctrl = e.ctrl
	}
	m.mu.RUnlock()
	if ctrl == nil {
		return ErrPaneGone
	}
	log.Printf("agent pane %s: %s, ending the session", paneID, why)
	if err := ctrl.SendInput(paneID, []byte{0x04}); err != nil {
		log.Printf("agent pane %s: exit keystroke failed: %v", paneID, err)
		return fmt.Errorf("exit keystroke: %w", err)
	}
	m.activity.forget(paneID, true)
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
	return nil
}
