package api

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// attachAndHello dials the attach WS for a workspace and consumes the opening
// hello, returning the live connection.
func attachAndHello(t *testing.T, base, wsID string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/v1/attach?workspace=" + wsID
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	if m := readMsg(t, conn); m.T != "hello" {
		t.Fatalf("first frame = %q, want hello", m.T)
	}
	return conn
}

// awaitMarker reads frames until a marker appears in some pane's output/snapshot,
// or fails after a deadline. Returns the accumulated text.
func awaitMarker(t *testing.T, conn *websocket.Conn, marker string) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var acc strings.Builder
	for i := 0; i < 500; i++ {
		var m wsMsg
		if err := conn.ReadJSON(&m); err != nil {
			t.Fatalf("read waiting for %q: %v", marker, err)
		}
		if m.T == "output" || m.T == "snapshot" {
			if b, err := base64.StdEncoding.DecodeString(m.Data); err == nil {
				acc.Write(b)
				if strings.Contains(acc.String(), marker) {
					return
				}
			}
		}
	}
	t.Fatalf("marker %q never appeared", marker)
}

// TestAPI_ReconnectReseed proves the Phase 8 reconnect contract: a lens whose WS
// drops re-attaches and reseeds cleanly. The fresh attach's snapshot must reflect
// the pre-drop screen state (the session persisted — no data loss), and live
// input/output must round-trip on the new connection (the re-subscribe is wired,
// not stale). The reseed is a single fresh capture per pane, so there is no
// duplicated stream replay.
func TestAPI_ReconnectReseed(t *testing.T) {
	_, base := floodStack(t, "ccmux-reconnect-itest")

	ws := createWS(t, base)
	pane0 := ws.Panes[0].ID

	// First attach: leave a durable marker on the screen.
	conn1 := attachAndHello(t, base, ws.ID)
	_ = conn1.WriteJSON(wsMsg{T: "input", Pane: pane0,
		Data: base64.StdEncoding.EncodeToString([]byte("echo BEFORE_DROP_MARKER\n"))})
	awaitMarker(t, conn1, "BEFORE_DROP_MARKER")

	// Abruptly drop the connection (TCP close → server readLoop errors → the
	// subscription and presence entry are torn down via defers).
	_ = conn1.Close()
	time.Sleep(150 * time.Millisecond)

	// Reconnect. A fresh attach yields exactly one hello, then one snapshot per
	// pane. The snapshot must still contain the marker: the tmux session outlived
	// the WS drop, so the reseed reflects the real current screen.
	conn2 := attachAndHello(t, base, ws.ID)
	defer conn2.Close()

	conn2.SetReadDeadline(time.Now().Add(5 * time.Second))
	sawSnapshot := false
	snapshotHasMarker := false
	extraHellos := 0
	for i := 0; i < 50 && !sawSnapshot; i++ {
		var m wsMsg
		if err := conn2.ReadJSON(&m); err != nil {
			break
		}
		switch m.T {
		case "hello":
			extraHellos++
		case "snapshot":
			if m.Pane == pane0 {
				sawSnapshot = true
				if b, err := base64.StdEncoding.DecodeString(m.Data); err == nil {
					snapshotHasMarker = strings.Contains(string(b), "BEFORE_DROP_MARKER")
				}
			}
		}
	}
	if extraHellos != 0 {
		t.Fatalf("reconnect produced %d extra hello frames (want a single hello, already consumed)", extraHellos)
	}
	if !sawSnapshot {
		t.Fatal("reconnect never delivered a snapshot for pane0")
	}
	if !snapshotHasMarker {
		t.Fatal("reconnect snapshot did not reflect pre-drop screen state (BEFORE_DROP_MARKER missing) — reseed lost data")
	}

	// Live streaming must work on the new connection: input round-trips to output.
	_ = conn2.WriteJSON(wsMsg{T: "input", Pane: pane0,
		Data: base64.StdEncoding.EncodeToString([]byte("echo AFTER_RECONNECT_MARKER\n"))})
	awaitMarker(t, conn2, "AFTER_RECONNECT_MARKER")
}
