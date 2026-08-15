package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/hub"
)

// hubEvents streams the merged firehose in hub mode: the hub's own attention plus
// every listing member host's, by holding a /v1/events client to each. Frames
// carry globally-unique workspace/pane ids, so no host tagging is needed — a lens
// resolves each to the host-stamped workspace it already knows from /v1/workspaces
// (which also seeds attention, so a host joining mid-stream needs no replay here).
func (s *Server) hubEvents(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	id, ch := s.mgr.SubscribeEvents()
	defer s.mgr.UnsubscribeEvents(id)

	// Who is reading this stream; the alert flag is stamped for them alone. The
	// join matters most HERE: in a federation the Mac app's firehose lands on
	// the hub, and this is the entry that keeps its person visible to
	// ActiveOwners when no workspace is attached (see joinFirehose).
	reader, connID := s.joinFirehose(r)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	up := &eventUpstreams{hub: s.hub, frames: make(chan []byte, 128), ka: s.ka, connected: map[string]bool{}}
	hello := append(currentAttention(s.mgr), up.dialAll(ctx)...)
	if err := conn.WriteJSON(firehoseMsg{T: "hello", Attention: hello}); err != nil {
		s.presence.Leave(firehosePresenceWS, connID) // the read goroutine below never starts
		return
	}

	// The read goroutine owns the presence removal, because it also delivers
	// presence frames — see events for why a handler-side defer was a race. It
	// cannot start before the hello here: drainReads arms the read deadline, and
	// dialAll can block past it on broken members while the client legitimately
	// sends nothing.
	go func() {
		defer s.presence.Leave(firehosePresenceWS, connID)
		drainReads(cancel, conn, s.ka, reader.login, s.presenceFrames(connID))
	}()
	go up.reconnectLoop(ctx, 10*time.Second)

	// The ping belongs with the read deadline drainReads just armed, and this is
	// the connection's only writer. Arming one without the other reaps every idle
	// lens on schedule — and since s.events routes here whenever a hub is
	// configured, this is THE firehose in a federation, not a side path.
	ping := time.NewTicker(s.ka.pingEvery())
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			if err := s.ka.writePing(conn); err != nil {
				pingFailed("hub firehose", reader.login, err)
				return
			}
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := conn.WriteJSON(s.firehoseFrame(ev, reader)); err != nil {
				return
			}
		case raw := <-up.frames:
			if err := conn.WriteMessage(websocket.TextMessage, s.restampAlert(raw, reader)); err != nil {
				return
			}
		}
	}
}

// restampAlert replaces a member's alert verdict with the hub's before the frame
// reaches a lens, leaving every other FRAME untouched (fields, see below).
//
// The member computed that flag against ITS OWN presence — the lenses attached
// to it — which is the wrong question. The lens reading this stream is attached
// HERE, and the hub is the only node that knows about all of them. A member with
// nobody looking at its own workspaces stamped alert=false on every pane it
// owned, which is why a Linux session's "needs input" reached the Mac as a
// silent sidebar flash.
//
// Unparseable or non-attention frames pass through byte-for-byte: this is a
// relay, and a frame it does not understand is not its to rewrite.
//
// An attention frame does NOT pass through byte-for-byte. It is decoded into
// firehoseMsg and re-encoded, so a field a newer member sends and this struct
// does not model is dropped here. The frame contract has to stay in lockstep
// across the fleet; hosts already run mixed versions (hub.Health carries a
// contract number), so a field added on one side needs this struct on the other.
func (s *Server) restampAlert(raw []byte, reader firehoseReader) []byte {
	var frame firehoseMsg
	if err := json.Unmarshal(raw, &frame); err != nil || frame.T != "attention" {
		return raw
	}
	frame.Alert = s.alertsFor(reader, frame.State)
	restamped, err := json.Marshal(frame)
	if err != nil {
		// The member's own verdict crosses instead — wrong, but a dropped frame
		// would be worse, and this cannot happen for a struct that just decoded.
		log.Printf("peers: could not re-stamp a relayed attention frame (%v) — forwarding the member's verdict", err)
		return raw
	}
	return restamped
}

// eventUpstreams holds one /v1/events client per listing remote host, forwarding
// their non-hello frames onto a single channel the lens's writer drains.
type eventUpstreams struct {
	hub    *hubMode
	frames chan []byte
	ka     keepalive

	mu        sync.Mutex
	connected map[string]bool
	// failing latches which hosts have already reported a dial failure, so the
	// 10s reconnect loop states the fault once and the recovery once instead of
	// every tick. Same shape as federatedFocus.failing in hubpush.go.
	failing map[string]bool
}

// noteDial logs a member dropping out of the merged firehose, and its return.
// Silence here meant a permanently unreachable host vanished from the stream with
// nothing anywhere naming it — the hub simply stopped relaying that machine's
// attention, and stopped pushing for it, for as long as the daemon ran.
func (u *eventUpstreams) noteDial(hostID string, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.failing == nil {
		u.failing = map[string]bool{}
	}
	switch {
	case err != nil && !u.failing[hostID]:
		u.failing[hostID] = true
		log.Printf("hub events: %s dropped out of the merged firehose (%v) — retrying every 10s", hostID, err)
	case err == nil && u.failing[hostID]:
		delete(u.failing, hostID)
		log.Printf("hub events: %s is back in the merged firehose", hostID)
	}
}

// dialAll connects to every listing remote host not already connected, returning
// their merged current-attention snapshots (from each hello). Reconnect calls
// discard the returned snapshot — the lens's periodic /v1/workspaces poll already
// carries per-pane attention, so only live deltas matter after the first hello.
func (u *eventUpstreams) dialAll(ctx context.Context) []attnEntry {
	var snap []attnEntry
	for _, h := range u.hub.reg.List() {
		if h.Self || !h.Lists() {
			continue
		}
		u.mu.Lock()
		already := u.connected[h.ID]
		u.mu.Unlock()
		if already {
			continue
		}
		conn, attn, err := dialHostEvents(ctx, u.hub.wsDial, h, u.ka)
		if err != nil {
			u.noteDial(h.ID, err)
			continue
		}
		u.noteDial(h.ID, nil)
		u.mu.Lock()
		u.connected[h.ID] = true
		u.mu.Unlock()
		snap = append(snap, attn...)
		go u.forward(ctx, h.ID, conn)
	}
	return snap
}

// forward pumps a host's non-hello frames onto u.frames until the connection
// drops, then releases the host so reconnectLoop can re-dial it.
//
// The keepalive here is load-bearing, not hygiene. Without a read deadline this
// loop parks in ReadMessage forever when a member dies without closing — a
// sleeping laptop, a dropped tailnet path. The deferred cleanup never runs, so
// `connected[hostID]` stays true, so dialAll's "already connected" check skips
// that host on every reconnect tick, forever. The hub silently stops relaying
// that host's attention AND stops pushing for it until ccmuxd restarts.
func (u *eventUpstreams) forward(ctx context.Context, hostID string, conn *websocket.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		conn.Close()
		u.mu.Lock()
		delete(u.connected, hostID)
		u.mu.Unlock()
	}()
	go u.ka.pingLoop(ctx, conn, hostID)
	u.ka.armReads(conn)
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		u.ka.touchReads(conn)
		if isHelloFrame(data) {
			continue // a reconnected host re-sends hello; the delta stream is what we forward
		}
		select {
		case u.frames <- data:
		case <-ctx.Done():
			return
		}
	}
}

// reconnectLoop re-dials hosts that joined or dropped mid-session.
func (u *eventUpstreams) reconnectLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			u.dialAll(ctx)
		}
	}
}

// dialHostEvents opens a host's /v1/events and consumes its opening hello,
// returning the connection and the host's current-attention snapshot.
func dialHostEvents(ctx context.Context, wsDial func(context.Context, string) (*websocket.Conn, error), h hub.Host, ka keepalive) (*websocket.Conn, []attnEntry, error) {
	conn, err := wsDial(ctx, "wss://"+h.Addr+"/v1/events")
	if err != nil {
		return nil, nil, err
	}
	// A host that accepts the connection and then never speaks would otherwise
	// park this dial forever. The HELLO budget, not the idle one: dialAll runs
	// this inline, in sequence, for every member, and a lens gets no hello of its
	// own until they all answer. Reusing the 90s idle deadline here would let two
	// broken members hold a sidebar empty for three minutes.
	_ = conn.SetReadDeadline(time.Now().Add(ka.helloWithin()))
	_, data, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	var msg firehoseMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		// A hello this build cannot read is a FAILED dial, not a healthy one.
		// Returning nil here let the caller clear the failure latch and announce
		// the host as recovered, while its attention snapshot silently became
		// empty — and mixed wire versions across the fleet are the stated
		// operating condition, so this is a live case, not a hypothetical.
		conn.Close()
		return nil, nil, fmt.Errorf("hello did not decode (wire contract skew?): %w", err)
	}
	return conn, msg.Attention, nil
}

// isHelloFrame reports whether a firehose frame is a hello (peeking just "t").
func isHelloFrame(data []byte) bool {
	var peek struct {
		T string `json:"t"`
	}
	return json.Unmarshal(data, &peek) == nil && peek.T == "hello"
}
