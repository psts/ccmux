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

// SetView upserts (or, with an empty window, deletes) one login's view row and
// broadcasts workspace-status so every lens re-groups its list.
func (m *Manager) SetView(login, wsID, window string) error {
	if m.store == nil {
		return errors.New("no store: views are not available")
	}
	if err := m.store.SetView(login, wsID, strings.TrimSpace(window)); err != nil {
		return err
	}
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
	return nil
}

// ImportView migrates a workspace's legacy persisted group into the owner's
// view row, once: the marker is what distinguishes "never migrated" from "the
// owner deliberately put it away", so a cleared arrangement stays cleared.
func (m *Manager) ImportView(owner, wsID, window string) error {
	if m.store == nil {
		return errors.New("no store: views are not available")
	}
	if err := m.store.MarkViewImported(wsID); err != nil {
		return err
	}
	return m.SetView(owner, wsID, window)
}

// ViewImports returns which workspaces already ran their legacy-group import.
func (m *Manager) ViewImports() map[string]bool {
	if m.store == nil {
		return map[string]bool{}
	}
	imported, err := m.store.ViewImports()
	if err != nil {
		log.Printf("views: reading import markers failed (%v); skipping legacy import this pass", err)
		return nil
	}
	return imported
}

// Views returns every view row, keyed workspace → login → window. Empty (never
// nil) without a store, so list stamping stays off the handlers' panic path.
func (m *Manager) Views() map[string]map[string]string {
	if m.store == nil {
		return map[string]map[string]string{}
	}
	views, err := m.store.AllViews()
	if err != nil {
		// Loud: an unreadable views table renders as "everything is Available /
		// nobody has windows", which looks like data loss, not a DB error.
		log.Printf("views: reading view rows failed (%v); serving as if none exist", err)
		return map[string]map[string]string{}
	}
	return views
}
