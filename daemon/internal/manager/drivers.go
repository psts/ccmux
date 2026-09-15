package manager

import (
	"log"
	"sync"

	"ccmux.dev/ccmuxd/internal/model"
)

// Remembered drivers: who last typed in each workspace, kept after their lens
// is gone and across restarts. Presence records them on input; notification
// routing (api.alertAudience) reads them when no live driver is attached.
//
// Before this, the driver was only the LIVE client that typed last. Closing
// the laptop dropped that client, routing widened to everyone holding the
// workspace's window open, and a repo Dasha was working in buzzed Patric's
// phone because he had peeked at her window from the web lens days before.
type drivers struct {
	mu     sync.Mutex
	loaded bool
	byWS   map[string]model.WorkspaceDriver
}

// RecordDriver notes that login just typed in a workspace. An unidentified
// typist ("" or "anon") cannot be routed to, but they still displace whoever
// was remembered: "theirs until someone else types" must hold for a typist
// the daemon cannot name, or a shared machine could never take a repo back.
//
// The store is written only when the driver CHANGES — a keystroke per write
// would be a database hit per key — and after the lock is released, so other
// panes' input workers never queue behind a SQLite write. The write itself
// still runs on this pane's drain worker; it is rare only because it happens
// on a driver change, not per keystroke. At is therefore current in memory
// and, after a restart, the keystroke that STARTED the remembered driver's
// tenure.
func (m *Manager) RecordDriver(wsID, login string, at int64) {
	m.drivers.mu.Lock()
	m.loadDriversLocked()
	prev, had := m.drivers.byWS[wsID]
	usable := login != "" && login != "anon"
	if usable {
		m.drivers.byWS[wsID] = model.WorkspaceDriver{Login: login, At: at}
	} else {
		delete(m.drivers.byWS, wsID)
	}
	m.drivers.mu.Unlock()
	if m.store == nil {
		return
	}
	// Two positive cases; everything else is a no-op. The quiet one is an
	// anon typist on a workspace nobody is remembered for, which would
	// otherwise be a DELETE per keystroke.
	var err error
	switch {
	case usable && (!had || prev.Login != login): // a new person took the repo
		err = m.store.SetWorkspaceDriver(wsID, model.WorkspaceDriver{Login: login, At: at})
	case !usable && had: // an unidentified typist displaced them
		err = m.store.DeleteWorkspaceDriver(wsID)
	}
	if err != nil {
		log.Printf("driver for workspace %s not persisted: %v (wrong again after a restart)", wsID, err)
	}
}

// ForgetDriver drops a removed workspace's driver, memory and store, so a
// year of deleted workspaces does not ride along in every hub poll.
func (m *Manager) ForgetDriver(wsID string) {
	m.drivers.mu.Lock()
	m.loadDriversLocked()
	delete(m.drivers.byWS, wsID)
	m.drivers.mu.Unlock()
	if m.store == nil {
		return
	}
	if err := m.store.DeleteWorkspaceDriver(wsID); err != nil {
		log.Printf("driver for removed workspace %s not deleted: %v", wsID, err)
	}
}

// LastDriver answers who last typed in a workspace, live or not.
func (m *Manager) LastDriver(wsID string) (model.WorkspaceDriver, bool) {
	m.drivers.mu.Lock()
	defer m.drivers.mu.Unlock()
	m.loadDriversLocked()
	d, ok := m.drivers.byWS[wsID]
	return d, ok
}

// LastDrivers returns a copy of every remembered driver, keyed by workspace.
func (m *Manager) LastDrivers() map[string]model.WorkspaceDriver {
	m.drivers.mu.Lock()
	defer m.drivers.mu.Unlock()
	m.loadDriversLocked()
	out := make(map[string]model.WorkspaceDriver, len(m.drivers.byWS))
	for k, v := range m.drivers.byWS {
		out[k] = v
	}
	return out
}

// loadDriversLocked seeds the cache from the store once. Lazy rather than in
// New because New does no store reads today (Start does them) and this keeps
// it that way; a read failure logs and starts empty (routing widens to window
// holders until someone types).
func (m *Manager) loadDriversLocked() {
	if m.drivers.loaded {
		return
	}
	m.drivers.loaded = true
	m.drivers.byWS = map[string]model.WorkspaceDriver{}
	if m.store == nil {
		return
	}
	saved, err := m.store.WorkspaceDrivers()
	if err != nil {
		log.Printf("remembered drivers unreadable: %v (routing widens until someone types)", err)
		return
	}
	m.drivers.byWS = saved
}
