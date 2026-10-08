package manager

import (
	"log"

	"ccmux.dev/ccmuxd/internal/model"
)

// A flash lives until it gets attention SOMEWHERE. The lenses used to decide
// that each for themselves — the web page cleared a row when you opened it
// there, the Mac ran a timer — so looking at a workspace on one screen left it
// blinking on every other. The daemon is the one place every lens reports to,
// so "seen" is decided here and fanned out as a plain attention change to idle,
// which every shipped lens already reads as "nothing to show".

// retireIfWatched is the "already watching" half of the rule: a pane that
// finishes while a lens is looking at its workspace stops flashing at once,
// everywhere. It runs AFTER the hook's own value went out, on purpose: the
// notifier and the alert flag read that first frame, so who gets a push (the
// driver's phone, a present Mac) keeps today's rules. Folding the rewrite into
// the stored value silenced every push whenever anyone at all was looking.
// Nil Watched (tests, or a manager with no api) means nobody is watching.
//
// The attention board never counts as watching: it reports a live screen with
// no pane named, so its tiles stay up until somebody deals with them.
func (m *Manager) retireIfWatched(wsID string, att model.Attention) {
	if att.Claims() && m.Watched != nil && m.Watched(wsID) {
		m.MarkSeen(wsID)
	}
}

// MarkSeen retires every flash in a workspace because a lens reported it is
// looking at one of its panes. The agent lifecycle's busy/idle record is left
// alone on purpose: it read the real hook (claudeBusy) when it arrived, and a
// human glancing at the pane is not the agent starting work.
func (m *Manager) MarkSeen(wsID string) {
	m.retire(wsID, "")
}

// MarkActed retires ONE pane's claim because a human dealt with it on the
// attention board: they clicked into its tile and moved on. Narrower than
// MarkSeen on purpose. The board shows every claiming pane at once, so a look
// there proves nothing, and two panes of one repo waiting side by side must
// not leave together because one of them was answered. An empty pane id, or
// one not in this workspace, retires nothing.
func (m *Manager) MarkActed(wsID, paneID string) {
	if paneID != "" {
		m.retire(wsID, paneID)
	}
}

// retire sets the claiming panes of a workspace to idle: every one of them, or
// only paneID when it is set. Broadcast and persisted like any other change.
func (m *Manager) retire(wsID, paneID string) {
	m.mu.Lock()
	e := m.byID[wsID]
	if e == nil {
		m.mu.Unlock()
		return
	}
	var cleared []model.Pane
	for _, p := range e.ws.Panes {
		if !p.Attention.Claims() || (paneID != "" && p.ID != paneID) {
			continue
		}
		setClaim(p, model.AttentionIdle, model.ReasonNone, 0)
		cleared = append(cleared, *p)
	}
	ctrl := e.ctrl
	m.mu.Unlock()
	for i := range cleared {
		p := &cleared[i]
		m.publishAttention(ctrl, wsID, *p)
		if err := m.store.SavePane(p); err != nil {
			log.Printf("pane %s: seen not persisted (the flash returns after a restart): %v", p.ID, err)
		}
	}
}
