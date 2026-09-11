package manager

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/listeners"
	"ccmux.dev/ccmuxd/internal/model"
)

// Listener discovery: every tick the daemon reads which TCP ports the hosted
// panes' processes are listening on and files each under its workspace
// (Workspace.Listeners). Hostname routes resolve against that (see
// resolveRoutesLocked), so a dev server binds wherever its script, its config
// or the user says, and the name follows.

// StartListenerScan runs the socket scan on an interval. A host without a
// proc tree logs once and stops: hostnames then route to their configured
// port blindly, which is what they did before discovery existed.
func (m *Manager) StartListenerScan(interval time.Duration) {
	go func() {
		s := listeners.New()
		found, err := s.Scan()
		if err != nil {
			log.Printf("listener discovery off: %v", err)
			return
		}
		m.applyListeners(found)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-t.C:
				found, err := s.Scan()
				if err != nil {
					log.Printf("listener scan: %v", err)
					continue
				}
				m.applyListeners(found)
			}
		}
	}()
}

// applyListeners files a scan's rows under their workspaces, re-resolves
// every route, and broadcasts each workspace whose listeners OR routes
// changed — a HeldBy flip comes from another workspace's scan, so the diff
// is on the routes, not just the workspace's own rows. Rows whose pane the
// daemon does not know (closed since the process started, a foreign
// daemon's pane) are dropped.
func (m *Manager) applyListeners(found []listeners.Listener) {
	byWS := map[string][]model.Listener{}
	m.mu.Lock()
	for _, l := range found {
		e, _ := m.findPaneLocked(l.PaneID)
		if e == nil {
			continue
		}
		byWS[e.ws.ID] = append(byWS[e.ws.ID], model.Listener{Port: l.Port, Process: l.Process, PaneID: l.PaneID})
	}
	before := map[string]string{}
	for id, e := range m.byID {
		before[id] = routeSignature(e.ws)
		next := byWS[id]
		sort.Slice(next, func(i, j int) bool { return next[i].Port < next[j].Port })
		e.ws.Listeners = next
	}
	m.resolveRoutesLocked()
	changed := []string{}
	for id, e := range m.byID {
		if routeSignature(e.ws) != before[id] {
			changed = append(changed, id)
		}
	}
	m.mu.Unlock()
	for _, id := range changed {
		m.events.publish(Event{Kind: "workspace-status", WorkspaceID: id})
	}
	if len(changed) > 0 {
		m.notifyDevhost()
	}
}

// routeSignature is a change-detection key over what applyListeners can
// alter: the listener set and every row's resolved route.
func routeSignature(ws *model.Workspace) string {
	var b strings.Builder
	for _, l := range ws.Listeners {
		fmt.Fprintf(&b, "%d:%s:%s;", l.Port, l.Process, l.PaneID)
	}
	for _, h := range ws.Hostnames {
		fmt.Fprintf(&b, "|%s:%d:%s", h.Name, h.LivePort, h.HeldBy)
	}
	return b.String()
}

// portOwner is the workspace whose pane holds a port: keyed by id (two
// workspaces on the same repo usually share a name), named for display.
type portOwner struct{ id, name string }

// Listeners returns a copy of what the workspace's panes hold right now
// (nil for an unknown workspace). The slice is rewritten by the scan loop
// under m.mu; hand callers a copy, never the live header.
func (m *Manager) Listeners(wsID string) []model.Listener {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e := m.byID[wsID]
	if e == nil {
		return nil
	}
	return append([]model.Listener(nil), e.ws.Listeners...)
}

// resolveRoutesLocked stamps LivePort/HeldBy on every hostname from the
// current listener sets. Called with m.mu held, at the two places the
// inputs change: a hostname save and a listener scan.
func (m *Manager) resolveRoutesLocked() {
	owner := map[int]portOwner{}
	for _, e := range m.byID {
		for _, l := range e.ws.Listeners {
			owner[l.Port] = portOwner{e.ws.ID, e.ws.Name}
		}
	}
	for _, e := range m.byID {
		resolveRoutes(e.ws, owner)
	}
}

// bumpWindow bounds how far a server may have auto-bumped from its
// configured port: Next retries up to 10 ports up, vite one at a time.
// Anything further is a different server, not a moved one.
const bumpWindow = 10

// resolveRoutes decides where each of ws's names routes, given who holds
// which port across the daemon:
//
//   - A pane of ws holds the row's port → routes there (LivePort 0).
//   - Otherwise, if exactly one row is unmatched and ws holds exactly one
//     port no row names, AND that port sits just above the configured one
//     (within bumpWindow), the two pair up (LivePort = that port): the
//     server auto-bumped ("5173 in use, using 5174"). The window is what
//     keeps a Docker-published mapping (54321, root-owned, invisible to the
//     scan) from being handed the workspace's unrelated vite server.
//   - Otherwise the row routes to its port as configured — unless another
//     workspace's pane holds that port, in which case HeldBy names it and
//     the row routes nowhere (RoutePort 0): the same repo open twice with
//     one pinned port must not serve the other worktree's app under this
//     name.
func resolveRoutes(ws *model.Workspace, owner map[int]portOwner) {
	held := listenerPorts(ws)
	named := map[int]bool{}
	unmatched := []int{}
	for i := range ws.Hostnames {
		h := &ws.Hostnames[i]
		h.LivePort, h.HeldBy = 0, ""
		named[h.Port] = true
		if held[h.Port] {
			continue
		}
		unmatched = append(unmatched, i)
		if by := owner[h.Port]; by.id != "" && by.id != ws.ID {
			h.HeldBy = by.name
		}
	}
	spare := unnamedPorts(ws, named)
	if len(unmatched) != 1 || len(spare) != 1 {
		return
	}
	h := &ws.Hostnames[unmatched[0]]
	if spare[0] > h.Port && spare[0] <= h.Port+bumpWindow {
		h.LivePort, h.HeldBy = spare[0], ""
	}
}

// listenerPorts is the set of ports ws's panes hold.
func listenerPorts(ws *model.Workspace) map[int]bool {
	held := map[int]bool{}
	for _, l := range ws.Listeners {
		held[l.Port] = true
	}
	return held
}

// unnamedPorts is every held port no hostname row names.
func unnamedPorts(ws *model.Workspace, named map[int]bool) []int {
	spare := []int{}
	for _, l := range ws.Listeners {
		if !named[l.Port] {
			spare = append(spare, l.Port)
		}
	}
	return spare
}
