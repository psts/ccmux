package api

import (
	"context"
	"encoding/json"
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

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	up := &eventUpstreams{hub: s.hub, frames: make(chan []byte, 128), connected: map[string]bool{}}
	hello := append(currentAttention(s.mgr), up.dialAll(ctx)...)
	if err := conn.WriteJSON(firehoseMsg{T: "hello", Attention: hello}); err != nil {
		return
	}

	go drainReads(cancel, conn)
	go up.reconnectLoop(ctx, 10*time.Second)

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := conn.WriteJSON(s.firehoseFrame(ev)); err != nil {
				return
			}
		case raw := <-up.frames:
			if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
				return
			}
		}
	}
}

// eventUpstreams holds one /v1/events client per listing remote host, forwarding
// their non-hello frames onto a single channel the lens's writer drains.
type eventUpstreams struct {
	hub    *hubMode
	frames chan []byte

	mu        sync.Mutex
	connected map[string]bool
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
		conn, attn, err := dialHostEvents(ctx, u.hub.wsDial, h)
		if err != nil {
			continue
		}
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
func (u *eventUpstreams) forward(ctx context.Context, hostID string, conn *websocket.Conn) {
	defer func() {
		conn.Close()
		u.mu.Lock()
		delete(u.connected, hostID)
		u.mu.Unlock()
	}()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
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
func dialHostEvents(ctx context.Context, wsDial func(context.Context, string) (*websocket.Conn, error), h hub.Host) (*websocket.Conn, []attnEntry, error) {
	conn, err := wsDial(ctx, "wss://"+h.Addr+"/v1/events")
	if err != nil {
		return nil, nil, err
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	var msg firehoseMsg
	_ = json.Unmarshal(data, &msg)
	return conn, msg.Attention, nil
}

// isHelloFrame reports whether a firehose frame is a hello (peeking just "t").
func isHelloFrame(data []byte) bool {
	var peek struct {
		T string `json:"t"`
	}
	return json.Unmarshal(data, &peek) == nil && peek.T == "hello"
}
