package manager

import (
	"log"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/session"
)

// A flash lives until it gets attention SOMEWHERE. The lenses used to decide
// that each for themselves — the web page cleared a row when you opened it
// there, the Mac ran a timer — so looking at a workspace on one screen left it
// blinking on every other. The daemon is the one place every lens reports to,
// so "seen" is decided here and fanned out as a plain attention change to idle,
// which every shipped lens already reads as "nothing to show".

// seenClears says which attention values a look at the workspace retires.
// running and idle are ambient, not claims on the human.
func seenClears(att model.Attention) bool {
	return att == model.AttentionDone || att == model.AttentionNeedsInput
}

// retireIfWatched is the "already watching" half of the rule: a pane that
// finishes while a lens is looking at its workspace stops flashing at once,
// everywhere. It runs AFTER the hook's own value went out, on purpose: the
// notifier and the alert flag read that first frame, so who gets a push (the
// driver's phone, a present Mac) keeps today's rules. Folding the rewrite into
// the stored value silenced every push whenever anyone at all was looking.
// Nil Watched (tests, or a manager with no api) means nobody is watching.
func (m *Manager) retireIfWatched(wsID string, att model.Attention) {
	if seenClears(att) && m.Watched != nil && m.Watched(wsID) {
		m.MarkSeen(wsID)
	}
}

// MarkSeen retires every flash in a workspace because a lens reported it is
// looking at one of its panes. The agent lifecycle's busy/idle record is left
// alone on purpose: it read the real hook (claudeBusy) when it arrived, and a
// human glancing at the pane is not the agent starting work.
func (m *Manager) MarkSeen(wsID string) {
	m.mu.Lock()
	e := m.byID[wsID]
	if e == nil {
		m.mu.Unlock()
		return
	}
	var cleared []model.Pane
	for _, p := range e.ws.Panes {
		if !seenClears(p.Attention) {
			continue
		}
		p.Attention = model.AttentionIdle
		cleared = append(cleared, *p)
	}
	ctrl := e.ctrl
	m.mu.Unlock()
	for i := range cleared {
		p := &cleared[i]
		if ctrl != nil {
			ctrl.Broadcast(session.Event{Kind: "attention", PaneID: p.ID, Attention: p.Attention})
		}
		m.events.publish(Event{Kind: "attention", WorkspaceID: wsID, PaneID: p.ID, Attention: p.Attention})
		if err := m.store.SavePane(p); err != nil {
			log.Printf("pane %s: seen not persisted (the flash returns after a restart): %v", p.ID, err)
		}
	}
}
