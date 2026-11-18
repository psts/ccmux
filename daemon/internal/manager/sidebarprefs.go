package manager

import (
	"errors"
	"fmt"

	"ccmux.dev/ccmuxd/internal/store"
)

// errNoStore is what each per-person sidebar call answers on a manager
// with no store, like the windows' own calls.
var errNoStore = errors.New("no store: windows are not available")

// SetSidebarCollapsed folds or unfolds one group (a window) on one
// person's thread-page sidebar. View only: no session is touched, unlike a
// window's open flag. ErrUnknownWindow when the window does not exist.
func (m *Manager) SetSidebarCollapsed(login, windowID string, collapsed bool) error {
	if m.store == nil {
		return errNoStore
	}
	ok, err := m.store.SetSidebarCollapsed(login, windowID, collapsed)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownWindow, windowID)
	}
	return nil
}

// SetSidebarOrder places one person's groups in the order given; the ones
// left out keep the default order after them, and unknown ids are skipped.
func (m *Manager) SetSidebarOrder(login string, windowIDs []string) error {
	if m.store == nil {
		return errNoStore
	}
	return m.store.SetSidebarOrder(login, windowIDs)
}

// SidebarPrefs returns one person's sidebar choices.
func (m *Manager) SidebarPrefs(login string) ([]store.SidebarPref, error) {
	if m.store == nil {
		return []store.SidebarPref{}, nil
	}
	return m.store.SidebarPrefs(login)
}
