package manager

import (
	"fmt"
	"log"

	"ccmux.dev/ccmuxd/internal/model"
)

// Tab order is shared, like everything else about a workspace: a lens drags a
// tab, the daemon renumbers, and every other lens redraws its strip from the
// served order. The Mac keeps its own split geometry in the layout blob, but
// within a leaf its tabs follow this order too, so the two lenses agree.

// ReorderPanes puts the listed panes first, in the given order, and keeps the
// rest behind them in their old order. Ids the workspace does not hold are
// ignored and a repeated id counts once, so a list from a lens that raced a
// spawn or a close still lands: nothing is dropped, nothing is invented.
// Returns the workspace as now served.
func (m *Manager) ReorderPanes(wsID string, ids []string) (*model.Workspace, error) {
	m.mu.Lock()
	e := m.byID[wsID]
	if e == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w %s", ErrUnknownWorkspace, wsID)
	}
	e.ws.Panes = orderPanes(e.ws.Panes, ids)
	ordered := make([]string, len(e.ws.Panes))
	for i, p := range e.ws.Panes {
		p.Position = i
		ordered[i] = p.ID
	}
	ws := e.ws
	m.mu.Unlock()
	if err := m.store.UpdatePanePositions(wsID, ordered); err != nil {
		log.Printf("workspace %s: pane order not persisted (lenses still follow it until restart): %v", wsID, err)
	}
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
	return ws, nil
}

// orderPanes is the pure half of ReorderPanes: listed first, rest after.
func orderPanes(panes []*model.Pane, ids []string) []*model.Pane {
	byID := make(map[string]*model.Pane, len(panes))
	for _, p := range panes {
		byID[p.ID] = p
	}
	out := make([]*model.Pane, 0, len(panes))
	placed := make(map[string]bool, len(panes))
	for _, id := range ids {
		if p := byID[id]; p != nil && !placed[id] {
			out = append(out, p)
			placed[id] = true
		}
	}
	for _, p := range panes {
		if !placed[p.ID] {
			out = append(out, p)
		}
	}
	return out
}

// nextPosition is where a new pane goes: after everything the workspace holds.
func nextPosition(panes []*model.Pane) int {
	next := 0
	for _, p := range panes {
		if p.Position >= next {
			next = p.Position + 1
		}
	}
	return next
}
