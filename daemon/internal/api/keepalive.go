package api

import (
	"context"
	"errors"
	"log"
	"net"
	"time"

	"github.com/gorilla/websocket"
)

// Keepalive for the lens sockets (/v1/events and /v1/attach).
//
// Both are mostly idle by design: the firehose writes only when attention
// changes, and an attached pane can sit quiet for hours. That makes them blind
// to a connection that dies WITHOUT a close handshake — a sleeping laptop, a
// re-keyed tailnet path, a DERP relay swap. Nothing is written, so TCP never
// retransmits and never manufactures an error; nothing is read, so the reader
// blocks forever. Both ends go on believing the socket is fine.
//
// That is not hypothetical. It is why a Mac would stop flashing hosted attention
// after a sleep and never recover until the app was relaunched: the client's
// receive callback simply never fired again, and its reconnect only runs on a
// receive ERROR.
//
// The cure is the one the peers bus has always run (internal/peers/conn.go): the
// writer pings on a ticker, the reader holds a deadline that every pong re-arms.
//
// The two halves MUST stay together. A read deadline without a ping kills every
// idle client on schedule; a ping without a read deadline detects nothing. The
// web lens's attach socket has no reconnect at all (daemon/web/app.js), so the
// half-shipped version would silently kill browser terminals.
type keepalive struct {
	ping      time.Duration // how often the writer pings
	read      time.Duration // how long a reader waits before declaring the peer gone
	pingWrite time.Duration // deadline for the ping control frame itself
	hello     time.Duration // how long a DIALLED peer gets to send its opening frame
}

// defaultKeepalive is what every real server runs. It is a value on the Server
// rather than a package global so tests can compress it per-server: globals were
// raced by connection goroutines that outlive the test restoring them.
func defaultKeepalive() keepalive {
	return keepalive{
		ping: 30 * time.Second,
		// 3x the ping interval, so two pings may be lost before we give up.
		read: 90 * time.Second,
		// A handshake budget, not an idle one. The idle deadline is three lost
		// pings; a host that accepts a connection and then never says hello is
		// broken now, and dialAll waits on this inline for every member before a
		// lens gets its own hello.
		hello: 5 * time.Second,
		// Applies to the ping CONTROL frame only. Data writes stay deadline-free
		// on purpose: an attach reseed bursts a full screen capture for every
		// pane, and a slow link must not cost a lens its connection mid-reseed.
		pingWrite: 10 * time.Second,
	}
}

// The accessors below exist because the ZERO VALUE of this struct is reachable
// and, untreated, fatal: `time.NewTicker(0)` panics, and a zero read deadline
// kills a connection on its first read. `&Server{...}` literals are the dominant
// test constructor in this package, and none of them set `ka` — so the next test
// that dials /v1/events or /v1/attach would panic in a way that reads like a
// WebSocket bug rather than a missing field.
//
// Clamping to the defaults rather than validating loudly is the right call for a
// keepalive: the safe answer is known, and refusing to serve a connection over a
// missing tuning value would be worse than the problem.
func (k keepalive) pingEvery() time.Duration   { return orDuration(k.ping, 30*time.Second) }
func (k keepalive) readWithin() time.Duration  { return orDuration(k.read, 90*time.Second) }
func (k keepalive) helloWithin() time.Duration { return orDuration(k.hello, 5*time.Second) }
func (k keepalive) pingWriteWithin() time.Duration {
	return orDuration(k.pingWrite, 10*time.Second)
}

func orDuration(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

// writePing sends one ping, returning the error rather than a bare bool.
//
// The error is the point. "The client went away" and "this link stalled for ten
// seconds mid-reseed" both end a connection, and collapsing them into false left
// the daemon unable to answer the one question this whole keepalive exists to
// make answerable: did the lens leave, or did we drop it?
//
// WriteControl is the one write method gorilla permits from any goroutine ("The
// Close and WriteControl methods can be called concurrently with all other
// methods"). Every caller here is the connection's writer goroutine anyway, so
// the rule never has to be reasoned about again — but note that SetWriteDeadline
// is classed as a WRITE method, which is why the deadline travels as an argument
// rather than a separate call.
func (k keepalive) writePing(conn *websocket.Conn) error {
	return conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(k.pingWriteWithin()))
}

// pingFailed logs a ping failure worth a human's attention. The caller drops the
// connection either way; this only decides what is worth saying about it.
//
// A close already in flight, or an already-closed socket, is the ordinary way a
// lens goes away: logging those would bury the faults that matter under one line
// per departing client.
func pingFailed(what, who string, err error) {
	if errors.Is(err, websocket.ErrCloseSent) || errors.Is(err, net.ErrClosed) {
		return
	}
	log.Printf("%s: ping to %q failed (%v) — dropping the connection", what, who, err)
}

// armReads installs the read deadline and the pong handler that re-arms it.
//
// Must be called ON the goroutine that reads this connection. SetReadDeadline
// and SetPongHandler are read methods, and racing them against an in-flight
// ReadMessage is undefined behaviour.
func (k keepalive) armReads(conn *websocket.Conn) {
	k.touchReads(conn)
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(k.readWithin()))
	})
}

// touchReads pushes the deadline out after a successful read. Any frame is proof
// the peer is alive, so a chatty client never depends on pongs alone.
func (k keepalive) touchReads(conn *websocket.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(k.readWithin()))
}

// pingLoop pings until ctx ends or the connection breaks. It exists for the
// connections the daemon DIALS (the hub's upstreams to member hosts), where the
// reader is parked in ReadMessage and there is no write loop to fold a ticker
// into.
//
// Running as its own goroutine is safe only because WriteControl is the sole
// write here; any data write would need to move onto this goroutine or be
// serialised against it.
//
// The hub pings its upstreams rather than relying on the member to ping it. That
// is what makes this work against a member running an older build: gorilla
// answers a ping from inside whatever ReadMessage the peer is already blocked
// in, so no member-side change is required and the fleet needs no upgrade order.
func (k keepalive) pingLoop(ctx context.Context, conn *websocket.Conn, label string) {
	t := time.NewTicker(k.pingEvery())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := k.writePing(conn); err != nil {
				pingFailed("hub upstream", label, err)
				return
			}
		}
	}
}

// readEnded says why a read loop stopped, when that is worth saying.
//
// Arming a deadline changed what a bare `return` on a read error means. It used
// to mean one thing — the peer went away — and now it also means WE decided the
// peer was gone. Those are opposite diagnoses (their network vs our timer) and
// collapsing them into the same silent exit leaves the one question this
// keepalive exists to answer unanswerable from the logs.
//
// An ordinary close stays quiet: normal and going-away are how a lens is
// supposed to leave, and logging them would cost a line per departing client.
func readEnded(what, who string, err error, after time.Duration) {
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		log.Printf("%s: %q went silent for %v — reaped by the read deadline", what, who, after)
	case websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway):
	case errors.Is(err, net.ErrClosed):
	default:
		log.Printf("%s: read from %q failed (%v)", what, who, err)
	}
}
