package manager

import (
	"errors"

	"ccmux.dev/ccmuxd/internal/model"
)

// Scratchpad returns a workspace's shared scratchpad (empty when unwritten).
// The caller has already checked the workspace exists: on a hub it may run
// on another host, so the local registry is not the one to ask.
func (m *Manager) Scratchpad(wsID string) (*model.Scratchpad, error) {
	if m.store == nil {
		return nil, errors.New("no store: scratchpads are not available")
	}
	return m.store.Scratchpad(wsID)
}

// SaveScratchpad replaces a workspace's scratchpad and tells every lens, so
// a page showing it can pick up the new text.
func (m *Manager) SaveScratchpad(wsID, content, by string) (*model.Scratchpad, error) {
	if m.store == nil {
		return nil, errors.New("no store: scratchpads are not available")
	}
	pad := &model.Scratchpad{Content: content, UpdatedAt: nowMillis(), UpdatedBy: by}
	if err := m.store.SaveScratchpad(wsID, pad); err != nil {
		return nil, err
	}
	m.events.publish(Event{Kind: "workspace-scratchpad", WorkspaceID: wsID})
	return pad, nil
}
