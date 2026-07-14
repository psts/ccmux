package api

import (
	"sort"
	"strconv"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/session"
)

// ClientInfo describes one attached lens for presence display.
type ClientInfo struct {
	ID       string `json:"id"`
	User     string `json:"user"`
	Device   string `json:"device,omitempty"`
	Focused  string `json:"focused,omitempty"` // pane id
	ReadOnly bool   `json:"readonly"`
	Driving  bool   `json:"driving"`  // most recent typist (live pairing / handoff)
	Verified bool   `json:"verified"` // identity confirmed via Tailscale whois
}

type client struct {
	info      ClientInfo
	lastInput int64
}

type wsPresence struct {
	clients map[string]*client
	driver  string // connID of the current driver
}

// presenceHub tracks who is attached to each workspace and broadcasts changes to
// every attached lens through that workspace's controller fan-out (which enforces
// single-writer-per-connection). Identity is caller-supplied for now; Tailscale
// WhoIs replaces the User/Device fields in a later phase.
type presenceHub struct {
	mgr  *manager.Manager
	mu   sync.Mutex
	byWS map[string]*wsPresence
	seq  int
}

func newPresenceHub(mgr *manager.Manager) *presenceHub {
	return &presenceHub{mgr: mgr, byWS: map[string]*wsPresence{}}
}

// Join registers a client and returns its connection id.
func (h *presenceHub) Join(wsID string, info ClientInfo) string {
	h.mu.Lock()
	h.seq++
	id := strconv.Itoa(h.seq)
	info.ID = id
	wp := h.byWS[wsID]
	if wp == nil {
		wp = &wsPresence{clients: map[string]*client{}}
		h.byWS[wsID] = wp
	}
	wp.clients[id] = &client{info: info}
	snap := h.snapshotLocked(wsID)
	h.mu.Unlock()
	h.broadcast(wsID, snap)
	return id
}

// Leave removes a client.
func (h *presenceHub) Leave(wsID, connID string) {
	h.mu.Lock()
	wp := h.byWS[wsID]
	if wp == nil {
		h.mu.Unlock()
		return
	}
	delete(wp.clients, connID)
	if wp.driver == connID {
		wp.driver = ""
	}
	snap := h.snapshotLocked(wsID)
	h.mu.Unlock()
	h.broadcast(wsID, snap)
}

// Focus records which pane a client is looking at.
func (h *presenceHub) Focus(wsID, connID, pane string) {
	h.mu.Lock()
	wp := h.byWS[wsID]
	if wp == nil || wp.clients[connID] == nil {
		h.mu.Unlock()
		return
	}
	wp.clients[connID].info.Focused = pane
	snap := h.snapshotLocked(wsID)
	h.mu.Unlock()
	h.broadcast(wsID, snap)
}

// Input marks a client as the most recent typist; broadcasts only when the
// driver actually changes, so keystrokes don't spam presence updates.
func (h *presenceHub) Input(wsID, connID string) {
	h.mu.Lock()
	wp := h.byWS[wsID]
	if wp == nil || wp.clients[connID] == nil {
		h.mu.Unlock()
		return
	}
	wp.clients[connID].lastInput = time.Now().UnixMilli()
	changed := wp.driver != connID
	wp.driver = connID
	var snap []ClientInfo
	if changed {
		snap = h.snapshotLocked(wsID)
	}
	h.mu.Unlock()
	if changed {
		h.broadcast(wsID, snap)
	}
}

// snapshotLocked builds the presence list (caller holds the lock).
func (h *presenceHub) snapshotLocked(wsID string) []ClientInfo {
	wp := h.byWS[wsID]
	if wp == nil {
		return nil
	}
	out := make([]ClientInfo, 0, len(wp.clients))
	for id, c := range wp.clients {
		info := c.info
		info.Driving = id == wp.driver
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (h *presenceHub) broadcast(wsID string, snap []ClientInfo) {
	if ctrl := h.mgr.Controller(wsID); ctrl != nil {
		ctrl.Broadcast(session.Event{Kind: "presence", Payload: snap})
	}
}
