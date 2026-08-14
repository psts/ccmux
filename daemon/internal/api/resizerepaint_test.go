package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// frames pumps a connection into a channel from its own goroutine. A read
// deadline is not usable here: a timed-out read puts a gorilla connection into a
// permanently failed state, so measuring "nothing more arrived" that way destroys
// the very connection under test. The goroutine reads until the socket closes and
// the test measures quiet on the channel instead.
func frames(conn *websocket.Conn) <-chan wsMsg {
	ch := make(chan wsMsg, 256)
	go func() {
		defer close(ch)
		for {
			var m wsMsg
			if err := conn.ReadJSON(&m); err != nil {
				return
			}
			ch <- m
		}
	}()
	return ch
}

// snapshotsUntilQuiet counts snapshot frames for pane until nothing arrives for
// `quiet`.
func snapshotsUntilQuiet(ch <-chan wsMsg, pane string, quiet time.Duration) int {
	snapshots := 0
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				return snapshots
			}
			if m.T == "snapshot" && m.Pane == pane {
				snapshots++
			}
		case <-time.After(quiet):
			return snapshots
		}
	}
}

// A resize that actually changes the pane reflows it. A program that repaints on
// winch sends its own deltas, but a plain shell sends nothing, so the lens would
// keep showing text wrapped at the old width. The daemon owes the resizing lens a
// fresh capture — and owes it to that lens ONLY, so a phone watching along is not
// re-seeded by someone else's window drag.
func TestAPI_ResizeRepaintsOnlyTheResizingLens(t *testing.T) {
	_, base := floodStack(t, "ccmux-resizerepaint-itest")

	ws := createWS(t, base)
	pane0 := ws.Panes[0].ID

	a := attachAndHello(t, base, ws.ID) // the bystander
	defer a.Close()
	b := attachAndHello(t, base, ws.ID) // the resizer
	defer b.Close()
	aCh, bCh := frames(a), frames(b)

	// Both were just seeded at attach; let that settle so it can't be mistaken
	// for the repaint under test.
	snapshotsUntilQuiet(aCh, pane0, time.Second)
	snapshotsUntilQuiet(bCh, pane0, time.Second)

	if err := b.WriteJSON(wsMsg{T: "resize", Pane: pane0, Cols: 131, Rows: 41}); err != nil {
		t.Fatalf("resize: %v", err)
	}

	// The settle window is 150ms; a second of quiet is well past it.
	if got := snapshotsUntilQuiet(bCh, pane0, time.Second); got != 1 {
		t.Fatalf("resizing lens got %d snapshots, want exactly 1", got)
	}
	if got := snapshotsUntilQuiet(aCh, pane0, time.Second); got != 0 {
		t.Fatalf("bystander got %d snapshots, want 0", got)
	}
}

// A resize to the size the pane already has changes nothing: tmux does not winch
// the program and nothing reflows, so a capture would be pure cost. This is the
// case that fires constantly — every lens re-asserts its size on focus and on
// reconnect, almost always to the size already in effect.
func TestAPI_UnchangedResizeSendsNoRepaint(t *testing.T) {
	_, base := floodStack(t, "ccmux-resizenoop-itest")

	ws := createWS(t, base)
	pane0 := ws.Panes[0].ID

	c := attachAndHello(t, base, ws.ID)
	defer c.Close()
	ch := frames(c)
	snapshotsUntilQuiet(ch, pane0, time.Second)

	if err := c.WriteJSON(wsMsg{T: "resize", Pane: pane0, Cols: 129, Rows: 39}); err != nil {
		t.Fatalf("first resize: %v", err)
	}
	if got := snapshotsUntilQuiet(ch, pane0, time.Second); got != 1 {
		t.Fatalf("first (changing) resize gave %d snapshots, want 1", got)
	}

	if err := c.WriteJSON(wsMsg{T: "resize", Pane: pane0, Cols: 129, Rows: 39}); err != nil {
		t.Fatalf("second resize: %v", err)
	}
	if got := snapshotsUntilQuiet(ch, pane0, time.Second); got != 0 {
		t.Fatalf("unchanged resize gave %d snapshots, want 0", got)
	}
}

// One attach socket carries every pane of a workspace, and a lens re-asserts the
// size of every visible pane at once — on window focus and on reconnect. All of
// them reflowed, so all of them owe a repaint; keeping only the newest request
// would leave the neighbours drawn at the old width.
func TestAPI_ResizeRepaintsEveryPaneInTheBurst(t *testing.T) {
	_, base := floodStack(t, "ccmux-resizeburst-itest")

	ws := createWS(t, base)
	pane0 := ws.Panes[0].ID
	pane1 := spawnPane(t, base, ws.ID)

	c := attachAndHello(t, base, ws.ID)
	defer c.Close()
	ch := frames(c)
	snapshotsUntilQuiet(ch, "", time.Second) // drain the attach seed for both panes

	// Back to back, inside one settle window — what one window event looks like.
	for _, p := range []string{pane0, pane1} {
		if err := c.WriteJSON(wsMsg{T: "resize", Pane: p, Cols: 127, Rows: 37}); err != nil {
			t.Fatalf("resize %s: %v", p, err)
		}
	}

	got := map[string]int{}
	deadline := time.After(3 * time.Second)
	for len(got) < 2 {
		select {
		case m, ok := <-ch:
			if !ok {
				t.Fatalf("connection closed with snapshots for %v", got)
			}
			if m.T == "snapshot" {
				got[m.Pane]++
			}
		case <-deadline:
			t.Fatalf("only panes %v were repainted, want both %s and %s", got, pane0, pane1)
		}
	}
	if got[pane0] != 1 || got[pane1] != 1 {
		t.Fatalf("repaint counts = %v, want exactly 1 each", got)
	}
}

// spawnPane adds a second pane to a workspace and returns its id.
func spawnPane(t *testing.T, base, wsID string) string {
	t.Helper()
	body := `{"cwd":"/tmp","startupCommand":"","createdBy":"tester"}`
	resp, err := http.Post(base+"/v1/workspaces/"+wsID+"/panes", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("spawn pane: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("spawn pane status = %d, want 201", resp.StatusCode)
	}
	var p struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode pane: %v", err)
	}
	if p.ID == "" {
		t.Fatal("spawned pane has no id")
	}
	return p.ID
}

// A read-only observer cannot resize, so it must not be able to make the daemon
// capture either — otherwise a viewer could drive capture-pane on someone else's
// pane at will.
func TestAPI_ReadOnlyLensGetsNoRepaint(t *testing.T) {
	_, base := floodStack(t, "ccmux-resizereadonly-itest")

	ws := createWS(t, base)
	pane0 := ws.Panes[0].ID

	c := attachAndHello(t, base, ws.ID+"&readonly=1")
	defer c.Close()
	ch := frames(c)
	snapshotsUntilQuiet(ch, pane0, time.Second)

	if err := c.WriteJSON(wsMsg{T: "resize", Pane: pane0, Cols: 133, Rows: 43}); err != nil {
		t.Fatalf("resize: %v", err)
	}
	if got := snapshotsUntilQuiet(ch, pane0, time.Second); got != 0 {
		t.Fatalf("read-only lens got %d snapshots, want 0", got)
	}
}
