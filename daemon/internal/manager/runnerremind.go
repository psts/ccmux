package manager

import (
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
)

// A finished turn alerts only on Claude Code's idle reminder (idle_prompt, 60s
// after the last Stop; see api.notifyState for why a bare done does not). In
// a runner session that reminder never comes: seen live on 2026-11-16 with
// Claude Code 2.1.325 under SDK 0.3.311, where SessionStart, UserPromptSubmit,
// Stop, PreToolUse, PostToolUse and SessionEnd fired and no Notification did
// in the 70s after a turn. So the daemon keeps the same clock for a runner
// pane itself: a minute after its runner says the turn ended, with nothing
// said since, the claim it still makes is re-sent as a reminder (Remind),
// which is what a push waits for. A pane somebody dealt with inside the
// minute claims nothing by then, and gets none.

// runnerRemindAfter is Claude Code's own wait before its idle reminder.
const runnerRemindAfter = 60 * time.Second

// reminders holds one pending reminder per runner pane.
type reminders struct {
	mu     sync.Mutex
	timers map[string]*time.Timer
	after  time.Duration // 0 = runnerRemindAfter; a test shortens it
}

// set replaces a pane's pending reminder: armed, fire runs after the wait
// unless set is called for the pane again first; not armed, none is pending.
func (r *reminders) set(paneID string, armed bool, fire func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t := r.timers[paneID]; t != nil {
		t.Stop()
		delete(r.timers, paneID)
	}
	if !armed {
		return
	}
	if r.timers == nil {
		r.timers = map[string]*time.Timer{}
	}
	after := r.after
	if after == 0 {
		after = runnerRemindAfter
	}
	r.timers[paneID] = time.AfterFunc(after, fire)
}

// remindRunnerLater starts, or calls off, a runner pane's reminder for the
// attention it was just given: only a finished turn waits for one.
func (m *Manager) remindRunnerLater(p model.Pane) {
	// Not for a role agent: its pane never sent that reminder when it was a
	// tmux pane, and an agent that finishes a run is not waiting on anyone.
	m.runnerReminders.set(p.ID, p.Attention == model.AttentionDone && p.Agent == "", func() { m.Remind(p.ID) })
}
