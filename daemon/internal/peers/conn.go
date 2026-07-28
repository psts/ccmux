// WebSocket delivery: per-peer push connections (replay-then-stream with
// cumulative acks) and read-only group listeners for the viewer UIs.
package peers

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/model"
)

const (
	pingInterval  = 30 * time.Second
	readDeadline  = 90 * time.Second // missed pongs → dead socket, no zombies
	writeDeadline = 10 * time.Second
	outBuffer     = 256
)

// wireMessage is the push frame for kind=message events (and the listener copy
// of every event) — the exact shape today's clients decode.
type wireMessage struct {
	Type        string `json:"type"`
	Seq         int64  `json:"seq"`
	FromID      string `json:"from_id"`
	FromName    string `json:"from_name"`
	FromSummary string `json:"from_summary"`
	FromCWD     string `json:"from_cwd"`
	ToID        string `json:"to_id"`
	ToName      string `json:"to_name"`
	Text        string `json:"text"`
	SentAt      string `json:"sent_at"`
}

// wireVerdict is the push frame for permission verdicts: structured, so the
// thin client forwards it without parsing any text.
type wireVerdict struct {
	Type      string `json:"type"`
	Seq       int64  `json:"seq"`
	RequestID string `json:"request_id"`
	Behavior  string `json:"behavior"`
	FromID    string `json:"from_id"`
}

// ackFrame is the only client→server frame: a cumulative ack, sent after the
// MCP notification for that event was emitted.
type ackFrame struct {
	Type string `json:"type"`
	Seq  int64  `json:"seq"`
}

// WireFrame renders an event as its push-frame shape — the poll endpoint
// returns the same frames the WS pushes, so the thin client has one decoder.
func WireFrame(ev *model.PeerEvent) any { return peerFrame(ev) }

func peerFrame(ev *model.PeerEvent) any {
	if ev.Kind == model.PeerEventVerdict {
		return wireVerdict{Type: "permission_verdict", Seq: ev.Seq,
			RequestID: ev.RequestID, Behavior: ev.Behavior, FromID: ev.FromID}
	}
	return messageFrame(ev)
}

// messageFrame renders any event as a viewer-style message frame (verdicts
// show their raw "yes abcde" text, matching the old broker's listener stream).
func messageFrame(ev *model.PeerEvent) wireMessage {
	return wireMessage{
		Type: "message", Seq: ev.Seq,
		FromID: ev.FromID, FromName: ev.FromName,
		FromSummary: ev.FromSummary, FromCWD: ev.FromCWD,
		ToID: ev.ToID, ToName: ev.ToName,
		Text: ev.Text, SentAt: isoMillis(ev.SentAt),
	}
}

type peerConn struct {
	peerID string
	ws     *websocket.Conn
	out    chan *model.PeerEvent
	done   chan struct{}
	once   sync.Once
}

// close is called from the reader, the writer, AND deliverLocked's enqueue —
// sync.Once keeps the triple-shutdown race safe.
func (c *peerConn) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.ws.Close()
	})
}

// enqueue hands a live event to the writer; a full buffer means the client is
// stuck, so drop the socket — its cursor replay makes the reconnect lossless.
func (c *peerConn) enqueue(ev *model.PeerEvent) {
	select {
	case c.out <- ev:
	default:
		c.close()
	}
}

// AttachPeer takes ownership of an upgraded peer-mode WebSocket: replays
// events past the peer's cursor, then streams live ones, reading cumulative
// acks back. Blocks until the connection dies (callers run it per-request).
func (s *Service) AttachPeer(peerID string, ws *websocket.Conn) {
	c := &peerConn{peerID: peerID, ws: ws, out: make(chan *model.PeerEvent, outBuffer), done: make(chan struct{})}
	s.mu.Lock()
	if s.peers[peerID] == nil {
		s.mu.Unlock()
		_ = ws.Close()
		return
	}
	if old := s.conns[peerID]; old != nil {
		old.close()
	}
	s.conns[peerID] = c
	s.touchLocked(peerID) // a session attaching is a session that came back
	s.mu.Unlock()

	go s.readPeerAcks(c)
	s.writePeer(c)

	s.mu.Lock()
	if s.conns[peerID] == c {
		delete(s.conns, peerID)
	}
	s.mu.Unlock()
}

// writePeer replays past the cursor, then streams the live channel, deduping
// by seq (an event appended during the replay query shows up in both).
func (s *Service) writePeer(c *peerConn) {
	defer c.close()
	lastSent, err := s.st.PeerCursor(c.peerID)
	if err != nil {
		return
	}
	replay, err := s.st.PeerEventsAfter(c.peerID, lastSent)
	if err != nil {
		return
	}
	for _, ev := range replay {
		if !c.writeFrame(peerFrame(ev)) {
			return
		}
		lastSent = ev.Seq
	}
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-c.done:
			return
		case ev := <-c.out:
			if ev.Seq <= lastSent {
				continue
			}
			if !c.writeFrame(peerFrame(ev)) {
				return
			}
			lastSent = ev.Seq
		case <-ping.C:
			if c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeDeadline)) != nil {
				return
			}
		}
	}
}

func (c *peerConn) writeFrame(frame any) bool {
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeDeadline))
	return c.ws.WriteJSON(frame) == nil
}

// readPeerAcks consumes cumulative acks and keeps the read deadline fed by
// pongs; any read error tears the connection down.
func (s *Service) readPeerAcks(c *peerConn) {
	defer c.close()
	_ = c.ws.SetReadDeadline(time.Now().Add(readDeadline))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(readDeadline))
	})
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		_ = c.ws.SetReadDeadline(time.Now().Add(readDeadline))
		s.touch(c.peerID) // any frame is proof the session is still there
		var ack ackFrame
		if json.Unmarshal(data, &ack) == nil && ack.Type == "ack" && ack.Seq > 0 {
			_ = s.st.AdvancePeerCursor(c.peerID, ack.Seq)
		}
	}
}

// --- group listeners (read-only viewer stream) ---

type listenConn struct {
	group string
	ws    *websocket.Conn
	out   chan []byte
	done  chan struct{}
	once  sync.Once
}

func (l *listenConn) close() {
	l.once.Do(func() {
		close(l.done)
		_ = l.ws.Close()
	})
}

// AttachListener streams every delivered event in a group to a read-only
// viewer socket. Blocks until the connection dies.
func (s *Service) AttachListener(group string, ws *websocket.Conn) {
	l := &listenConn{group: group, ws: ws, out: make(chan []byte, outBuffer), done: make(chan struct{})}
	s.mu.Lock()
	s.listeners[l] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.listeners, l)
		s.mu.Unlock()
	}()

	go func() { // discard anything the viewer sends; notice the close
		defer l.close()
		_ = ws.SetReadDeadline(time.Now().Add(readDeadline))
		ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(readDeadline)) })
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
			_ = ws.SetReadDeadline(time.Now().Add(readDeadline))
		}
	}()

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	defer l.close()
	for {
		select {
		case <-l.done:
			return
		case payload := <-l.out:
			_ = ws.SetWriteDeadline(time.Now().Add(writeDeadline))
			if ws.WriteMessage(websocket.TextMessage, payload) != nil {
				return
			}
		case <-ping.C:
			if ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeDeadline)) != nil {
				return
			}
		}
	}
}

func (s *Service) fanToListenersLocked(ev *model.PeerEvent) {
	if len(s.listeners) == 0 {
		return
	}
	payload, err := json.Marshal(messageFrame(ev))
	if err != nil {
		return
	}
	for l := range s.listeners {
		if !sameGroup(l.group, ev.Group) {
			continue
		}
		select {
		case l.out <- payload:
		default:
			l.close()
		}
	}
}
