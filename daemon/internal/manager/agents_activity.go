package manager

import (
	"log"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

// agentState is what an agent pane's harness is doing, as the lifecycle
// reads it. Three states, not two: an agent stopped on a human (a question
// card, a permission prompt) is neither working nor done. It must not take a
// pushed prompt, and it waits on its own clock (cardWaitMinutes) so a card
// can stay open longer than an idle session is allowed to live.
type agentState string

const (
	stateBusy    agentState = "busy"
	stateIdle    agentState = "idle"
	stateWaiting agentState = "waiting"
)

// agentStateFor reads a pane's attention change as the lifecycle's state.
// The hooks encode attention for the LENS, not for us, and the mapping is
// easy to invert: user_prompt_submit → "idle" (the flash clears because the
// human is there), which for an agent is the moment work STARTS. So:
//   - idle and running are busy (running is opencode's busy/replied);
//   - needs_input is waiting, unless its reason says the turn is over;
//   - done is idle.
//
// See hooks.outcome and api/agentsignal.go for the mappings this reads.
func agentStateFor(att model.Attention, reason model.AttentionReason) agentState {
	switch {
	case att == model.AttentionIdle, att == model.AttentionRunning:
		return stateBusy
	case att == model.AttentionNeedsInput && reason != model.ReasonFinished:
		return stateWaiting
	}
	return stateIdle
}

// agentActivity is the daemon's view of each agent pane's state and since
// when, the clock the idle exit counts from. It lands here from the
// harness's own signals (Claude's hooks, the opencode plugin) via
// applyAttention.
//
// save, when set, writes every change through to the registry under the
// same lock, so writes reach the store in the order they happened. Without
// it a restart forgot every clock, and the fallback read the pane's
// attention, which a human's look rewrites to "idle" — read as busy, so the
// agent never exited again.
type agentActivity struct {
	mu    sync.Mutex
	state map[string]activity
	save  func(paneID string, a activity)
}

type activity struct {
	state agentState
	since time.Time
	// trustUntil is set on a busy reading loaded from the registry, and only
	// there. The "idle" that would end it may have fired while the daemon was
	// down (the sidecar and the opencode plugin do not retry), so past this
	// time, with no fresh signal, it counts as idle. A fresh signal of any
	// state clears it.
	trustUntil time.Time
}

// restoredTrust is how long a busy reading from before a restart
// is believed without a fresh signal. Long enough that a turn running across
// a ship is not cut; short enough that a lost "idle" does not hold the agent
// awake, and its schedules waiting, for good.
const restoredTrust = time.Hour

// note records a state. The same state again keeps the original since: two
// readings of one idle stretch are one stretch.
func (a *agentActivity) note(paneID string, st agentState, now time.Time) activity {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state == nil {
		a.state = map[string]activity{}
	}
	if cur, ok := a.state[paneID]; ok && cur.state == st {
		// Same stretch, now confirmed by a live signal: a restored reading
		// stops expiring.
		cur.trustUntil = time.Time{}
		a.state[paneID] = cur
		return cur
	}
	act := activity{state: st, since: now}
	a.state[paneID] = act
	if a.save != nil {
		a.save(paneID, act)
	}
	return act
}

func (a *agentActivity) get(paneID string) (activity, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	act, ok := a.state[paneID]
	return act, ok
}

// forget drops a pane's record. persist says whether to clear the stored one
// too: a pane leaving the registry takes its row with it, so only a pane
// that stays (its session ended, it sits at a shell) needs the write.
func (a *agentActivity) forget(paneID string, persist bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.state[paneID]; !ok {
		return
	}
	delete(a.state, paneID)
	if persist && a.save != nil {
		a.save(paneID, activity{})
	}
}

// seed loads the stored readings at start. Idle and waiting are trusted as
// they are: their clocks are the point of saving them, and a waiting pane
// already ends on its own card wait (or never, under an opencode dialog,
// where turning it idle would send ctrl-d into the dialog). Busy is trusted
// for restoredTrust (see activity.trustUntil). Unknown states are skipped: a
// row from a future version is no reading at all, not a guess.
func (a *agentActivity) seed(stored map[string]store.PaneActivity, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state == nil {
		a.state = map[string]activity{}
	}
	for id, s := range stored {
		act := activity{state: agentState(s.State), since: time.UnixMilli(s.Since)}
		switch act.state {
		case stateBusy:
			act.trustUntil = now.Add(restoredTrust)
		case stateIdle, stateWaiting:
		default:
			continue
		}
		a.state[id] = act
	}
}

// current is get with a restored reading's trust applied: past it, the
// reading becomes idle from that moment, saved like any other change.
func (a *agentActivity) current(paneID string, now time.Time) (activity, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur, ok := a.state[paneID]
	if !ok || cur.trustUntil.IsZero() || now.Before(cur.trustUntil) {
		return cur, ok
	}
	act := activity{state: stateIdle, since: cur.trustUntil}
	a.state[paneID] = act
	if a.save != nil {
		a.save(paneID, act)
	}
	return act, true
}

// reset drops a pane's reading in memory and in the registry, whether or not
// one is held: a revive starts a new session, and a row a previous daemon
// left must not become its clock at the next start.
func (a *agentActivity) reset(paneID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.state, paneID)
	if a.save != nil {
		a.save(paneID, activity{})
	}
}

// persistActivity is agentActivity.save for a manager with a store.
func (m *Manager) persistActivity(paneID string, a activity) {
	var since int64
	if a.state != "" {
		since = a.since.UnixMilli()
	}
	if err := m.store.UpdatePaneActivity(paneID, string(a.state), since); err != nil {
		log.Printf("agent pane %s: activity %q not persisted (a restart forgets this idle clock): %v", paneID, a.state, err)
	}
}

// loadActivity seeds the clocks from the registry. A failure costs only the
// clocks: every agent pane starts with no reading, which the lifecycle
// already handles (readingFor).
func (m *Manager) loadActivity() {
	stored, err := m.store.PaneActivities()
	if err != nil {
		log.Printf("agent activity unreadable, idle clocks restart now: %v", err)
		return
	}
	m.activity.seed(stored, time.Now())
}

// AgentBusy says whether the harness in paneID cannot take a prompt now:
// working, or stopped on a human. No reading yet reads as idle: a pane
// nobody has typed into is not mid-task.
func (m *Manager) AgentBusy(paneID string) bool {
	act, ok := m.activity.current(paneID, time.Now())
	return ok && act.state != stateIdle
}
