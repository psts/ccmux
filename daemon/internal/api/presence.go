package api

import (
	"log"
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
	// Present means "this screen can show a notification right now": awake and
	// unlocked. It is a property of the DEVICE, not of any workspace, which is
	// what makes it the right question to ask before alerting or pushing.
	//
	// Focused answers something narrower — which pane this lens is looking at —
	// and standing in for presence is what made notifications depend on which
	// workspace happened to be on screen. A Mac showing a local workspace, or a
	// hosted one on another Space, looked exactly like a Mac nobody was sitting at.
	Present bool `json:"present,omitempty"`
}

type client struct {
	info      ClientInfo
	login     string // canonical identity key (verified login/email, else self-declared name); push-suppression matches on this
	email     string // verified tailnet login (email) only; server-side only, never broadcast; for git attribution
	lastInput int64
	// presentReported separates "this lens told us whether it is at a screen"
	// from "this lens is too old to say". Without it, absent would read as false
	// and every older client would be treated as nobody-is-there: silent Macs,
	// and phones buzzing at the desk. See atAScreen.
	presentReported bool
}

// atAScreen reports whether this lens counts as a human able to see a
// notification. A lens that reports presence is taken at its word; one that never
// does falls back to the old proxy (a focused pane), so older clients behave
// exactly as they did before presence existed.
func (c *client) atAScreen() bool {
	if c.presentReported {
		return c.info.Present
	}
	return c.info.Focused != ""
}

// DriverIdentity is the human currently driving a workspace, for git attribution
// (the /v1/panes/{id}/driver endpoint). Unlike the broadcast ClientInfo it carries
// the email, so a commit hook can write a real Co-Authored-By trailer.
type DriverIdentity struct {
	User     string `json:"user"`
	Email    string `json:"email,omitempty"`
	Device   string `json:"device,omitempty"`
	Verified bool   `json:"verified"`
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

// Join registers a client and returns its connection id. login is the canonical
// identity key (what push subscriptions and suppression match on); email is the
// verified tailnet login (empty for self-declared identities), retained
// server-side for git attribution and never broadcast.
func (h *presenceHub) Join(wsID string, info ClientInfo, login, email string) string {
	h.mu.Lock()
	h.seq++
	id := strconv.Itoa(h.seq)
	info.ID = id
	wp := h.byWS[wsID]
	if wp == nil {
		wp = &wsPresence{clients: map[string]*client{}}
		h.byWS[wsID] = wp
	}
	wp.clients[id] = &client{info: info, login: login, email: email}
	snap := h.snapshotLocked(wsID)
	h.mu.Unlock()
	h.broadcast(wsID, snap)
	return id
}

// Driver returns the identity of the workspace's current driver (the most recent
// non-readonly typist), or ok=false when nobody is driving.
func (h *presenceHub) Driver(wsID string) (DriverIdentity, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	wp := h.byWS[wsID]
	if wp == nil || wp.driver == "" {
		return DriverIdentity{}, false
	}
	c := wp.clients[wp.driver]
	if c == nil {
		return DriverIdentity{}, false
	}
	return DriverIdentity{User: c.info.User, Email: c.email, Device: c.info.Device, Verified: c.info.Verified}, true
}

// ActiveOwners returns every identity login with at least one lens AT A SCREEN
// on ANY workspace. Two things read it, in opposite directions: the notifier
// suppresses phone pushes for these logins, and the firehose alerts them.
//
// The key is each client's canonical login, the same key a subscription is
// stored under (resolveIdentity produces both), never the collision-prone
// display name.
//
// "At a screen" used to mean "has a focused pane", which quietly made it a
// question about which workspace was on display rather than about the person.
// See client.atAScreen for what replaced it and why the old rule still applies
// to lenses that do not report presence.
func (h *presenceHub) ActiveOwners() map[string]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	owners := map[string]bool{}
	for _, wp := range h.byWS {
		for _, c := range wp.clients {
			if c.login == "" || !c.atAScreen() {
				continue
			}
			owners[c.login] = true
		}
	}
	return owners
}

// SetPresent records whether this lens's screen can currently show a
// notification. Broadcast like any other presence change so other lenses can
// render who is actually around.
func (h *presenceHub) SetPresent(wsID, connID string, present bool) {
	h.mu.Lock()
	wp := h.byWS[wsID]
	if wp == nil || wp.clients[connID] == nil {
		h.mu.Unlock()
		// Loud, because it should be impossible: this only runs for a connection
		// that just joined and is reading its own socket. Dropping it silently
		// leaves that lens on the pre-presence fallback, which is the behaviour
		// presence exists to replace — and nothing would say so.
		log.Printf("presence: presence report for an unknown lens (ws=%s conn=%s) — it will fall back to focus", wsID, connID)
		return
	}
	c := wp.clients[connID]
	unchanged := c.presentReported && c.info.Present == present
	c.info.Present, c.presentReported = present, true
	if unchanged {
		h.mu.Unlock()
		return
	}
	snap := h.snapshotLocked(wsID)
	h.mu.Unlock()
	h.broadcast(wsID, snap)
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
