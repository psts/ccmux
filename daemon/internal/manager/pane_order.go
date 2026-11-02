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
//
// Agent tabs always come first (agentsFirst): a lens attaching a session opens
// its first tab, and that should be the agent, not the terminal that happened
// to be opened before it. Every path that sets the order goes through it: a
// drag, a new tab, a terminal that starts an agent, and the load at startup.

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
	e.ws.Panes = agentsFirst(orderPanes(e.ws.Panes, ids))
	ordered := renumber(e.ws.Panes)
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

// agentsFirst puts the panes with a harness (claude, opencode, pi, codex, an
// agent) ahead of plain shells, each group keeping its order. Stable, so it is
// safe to apply to an order that already holds.
func agentsFirst(panes []*model.Pane) []*model.Pane {
	out := make([]*model.Pane, 0, len(panes))
	for _, p := range panes {
		if p.Harness != "" {
			out = append(out, p)
		}
	}
	for _, p := range panes {
		if p.Harness == "" {
			out = append(out, p)
		}
	}
	return out
}

// renumber makes each pane's Position its index and returns the ids in order,
// the shape UpdatePanePositions takes.
func renumber(panes []*model.Pane) []string {
	ids := make([]string, len(panes))
	for i, p := range panes {
		p.Position = i
		ids[i] = p.ID
	}
	return ids
}
