// Keeping the registry honest about tmux. Adoption ran exactly once, at
// startup, and a workspace that failed to attach — or that a spurious session
// exit cooled — stayed cold until the daemon restarted, with its controller
// nilled and no route back. Its tmux session and every Claude inside it went on
// running, unreachable and unshown: a workspace full of live work, dark.
//
// So attachment is retried on a timer instead of assumed, and anything the
// registry cannot explain is logged rather than silently dropped.
package manager

import (
	"log"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/session"
)

// reconcileInterval is how often the registry is re-checked against tmux. Well
// under the time it takes to notice a missing window by eye, and cheap: one
// list-sessions plus a map walk.
const reconcileInterval = 30 * time.Second

// StartReconciler runs Reconcile on a ticker for the lifetime of the manager's
// context.
func (m *Manager) StartReconciler() {
	go func() {
		t := time.NewTicker(reconcileInterval)
		defer t.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-t.C:
				m.Reconcile()
			}
		}
	}()
}

// Reconcile repairs drift between tmux and the registry: a workspace whose tmux
// session is alive but which we hold no control connection for is re-attached,
// and a managed session with no workspace record is reported. Exported so tests
// (and an operator) can force a pass.
func (m *Manager) Reconcile() {
	metas, err := m.server.ListManaged()
	if err != nil {
		log.Printf("reconcile: list managed sessions: %v", err)
		return
	}
	liveWS := make(map[string]bool, len(metas))
	for _, meta := range metas {
		liveWS[meta.WorkspaceID] = true
		if m.entry(meta.WorkspaceID) == nil {
			// A managed session the registry has never heard of. Not something to
			// clean up automatically — it may hold real work — but it must not
			// stay invisible.
			log.Printf("reconcile: orphan tmux session %q claims unknown workspace %s",
				meta.Name, meta.WorkspaceID)
		}
	}
	for _, wsID := range m.detachedIDs() {
		if liveWS[wsID] {
			m.reattach(wsID)
		}
	}
}

// detachedIDs lists workspaces we hold no control connection for.
func (m *Manager) detachedIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for id, e := range m.byID {
		if e.ctrl == nil {
			out = append(out, id)
		}
	}
	return out
}

// reattach reopens a control connection to a workspace whose tmux session is
// alive, bringing it back to live. Safe to call concurrently with anything that
// attaches: the controller is only installed if the slot is still empty.
func (m *Manager) reattach(wsID string) {
	m.mu.RLock()
	e := m.byID[wsID]
	var tmuxSession string
	if e != nil && e.ctrl == nil {
		tmuxSession = e.ws.TmuxSession
	}
	m.mu.RUnlock()
	if tmuxSession == "" {
		return
	}

	ctrl, err := session.Open(m.ctx, m.server, tmuxSession, wsID)
	if err != nil {
		log.Printf("reconcile: re-attach workspace %s (%s): %v", wsID, tmuxSession, err)
		return
	}

	m.mu.Lock()
	e = m.byID[wsID]
	if e == nil || e.ctrl != nil {
		m.mu.Unlock()
		_ = ctrl.Close() // someone got there first
		return
	}
	e.ctrl = ctrl
	e.ws.Status = model.StatusLive
	m.mu.Unlock()

	_ = m.store.SetWorkspaceStatus(wsID, model.StatusLive)
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
	go m.watch(wsID, ctrl)
	log.Printf("reconcile: re-attached workspace %s (%s)", wsID, tmuxSession)
}
