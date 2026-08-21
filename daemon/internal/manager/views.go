package manager

import (
	"errors"
	"log"
	"strings"
)

// Per-user views: which window each login keeps a workspace in. The rows live
// in this daemon's store, but the daemon that USES them is the one lenses talk
// to — the hub in a federation, a lone daemon otherwise — so a row's ws_id may
// belong to a remote host and is deliberately not checked against byID here.
// The API layer validates existence against the surface it serves (local
// manager, or the hub's aggregate). See docs/multitenant-plan.md.

// viewState is one coherent snapshot of everything the view resolver reads:
// the rows, the import markers (nil = unreadable, which BLOCKS imports), and
// the host owner. It exists because GroupForPane runs under the peers bus
// mutex, and the bus invariant is no I/O under that lock (hub/aggregator.go
// states the same rule) — so the store is read only when a write invalidated
// the cache, never per bus operation.
type viewState struct {
	views    map[string]map[string]string
	imported map[string]bool
	owner    string
}

// resolve applies the owner-view ladder against this snapshot: the owner's row
// wins; the legacy persisted group stands until the import ran; after import,
// a deleted row means put away ("" — the bus falls to its directory fallback).
func (st *viewState) resolve(owner, wsID, legacy string) string {
	if owner == "" {
		return legacy
	}
	if g, ok := st.views[wsID][owner]; ok {
		return g
	}
	if st.imported != nil && st.imported[wsID] {
		return ""
	}
	return legacy
}

// viewSnapshot returns the cached state, rebuilding it from the store only
// after a write invalidated it. A snapshot built from a failed read is served
// but NOT cached, so the next call retries instead of freezing the failure.
func (m *Manager) viewSnapshot() *viewState {
	m.viewsMu.Lock()
	defer m.viewsMu.Unlock()
	if m.viewCache != nil {
		return m.viewCache
	}
	st := &viewState{views: map[string]map[string]string{}, imported: map[string]bool{}}
	if m.store == nil {
		m.viewCache = st
		return st
	}
	st.owner = m.getSetting(settingOwner)
	views, verr := m.store.AllViews()
	if verr != nil {
		// Loud: an unreadable views table renders as "everything is Available /
		// nobody has windows", which looks like data loss, not a DB error.
		log.Printf("views: reading view rows failed (%v); serving as if none exist", verr)
		views = map[string]map[string]string{}
	}
	st.views = views
	imported, ierr := m.store.ViewImports()
	if ierr != nil {
		log.Printf("views: reading import markers failed (%v); skipping legacy import this pass", ierr)
		imported = nil
	}
	st.imported = imported
	if verr == nil && ierr == nil {
		m.viewCache = st
	}
	return st
}

// invalidateViews drops the cached snapshot; every view/owner write calls it.
func (m *Manager) invalidateViews() {
	m.viewsMu.Lock()
	m.viewCache = nil
	m.viewsMu.Unlock()
}

// SetView upserts (or, with an empty window, deletes) one login's view row and
// broadcasts workspace-status so every lens re-groups its list.
func (m *Manager) SetView(login, wsID, window string) error {
	if m.store == nil {
		return errors.New("no store: views are not available")
	}
	if err := m.store.SetView(login, wsID, strings.TrimSpace(window)); err != nil {
		return err
	}
	m.invalidateViews()
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
	return nil
}

// ImportView migrates a workspace's legacy persisted group into the owner's
// view row, once: the marker is what distinguishes "never migrated" from "the
// owner deliberately put it away", so a cleared arrangement stays cleared.
// The create path also seeds through here, so a workspace born with view rows
// can never be resurrected from its compat-persisted legacy column.
func (m *Manager) ImportView(owner, wsID, window string) error {
	if m.store == nil {
		return errors.New("no store: views are not available")
	}
	if err := m.store.MarkViewImported(wsID); err != nil {
		return err
	}
	return m.SetView(owner, wsID, window) // SetView invalidates the snapshot
}

// ViewImports returns which workspaces already ran their legacy-group import;
// nil means the markers are unreadable and imports must not run.
func (m *Manager) ViewImports() map[string]bool {
	return m.viewSnapshot().imported
}

// ViewResolver returns "which window does OWNER keep this workspace in",
// resolved against one coherent snapshot (see viewState.resolve for the
// ladder). Cached: safe to call under the peers bus mutex.
func (m *Manager) ViewResolver() func(owner, wsID, legacy string) string {
	return m.viewSnapshot().resolve
}

// Views returns every view row, keyed workspace → login → window, from the
// cached snapshot. Read-only for callers; empty (never nil) without a store.
func (m *Manager) Views() map[string]map[string]string {
	return m.viewSnapshot().views
}

// ViewsStrict is Views for callers that must not mistake an unreadable table
// for an empty one — the archive guard fails CLOSED on the error, because
// "no rows" is its permission to stop a session for everyone.
func (m *Manager) ViewsStrict() (map[string]map[string]string, error) {
	if m.store == nil {
		return map[string]map[string]string{}, nil
	}
	return m.store.AllViews()
}
