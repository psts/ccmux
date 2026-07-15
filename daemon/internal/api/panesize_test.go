package api

import (
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestAPI_PaneSizeBroadcast proves the sizing wire contract that drives the mobile
// "take over" affordance: a resize from one lens is broadcast as a pane-size event
// to every other attached lens, and a fresh attach's hello carries the current
// pane width. Without this a phone couldn't tell it was no longer the size driver.
func TestAPI_PaneSizeBroadcast(t *testing.T) {
	_, base := floodStack(t, "ccmux-panesize-itest")

	ws := createWS(t, base)
	pane0 := ws.Panes[0].ID

	// Lens A (the "phone") and lens B (the "desktop"), both attached.
	a := attachAndHello(t, base, ws.ID)
	defer a.Close()
	b := attachAndHello(t, base, ws.ID)
	defer b.Close()

	// Lens B drives a wide resize; lens A must receive a pane-size broadcast.
	_ = b.WriteJSON(wsMsg{T: "resize", Pane: pane0, Cols: 137, Rows: 42})

	a.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := false
	for i := 0; i < 200 && !got; i++ {
		var m wsMsg
		if err := a.ReadJSON(&m); err != nil {
			t.Fatalf("read: %v", err)
		}
		if m.T == "pane-size" && m.Pane == pane0 {
			if m.Cols != 137 || m.Rows != 42 {
				t.Fatalf("pane-size = %dx%d, want 137x42", m.Cols, m.Rows)
			}
			got = true
		}
	}
	if !got {
		t.Fatal("lens A never received the pane-size broadcast")
	}

	// A fresh attach's hello must report the current width (137), not the default.
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/v1/attach?workspace=" + ws.ID
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial c: %v", err)
	}
	defer c.Close()
	var hello wsMsg
	if err := c.ReadJSON(&hello); err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if hello.T != "hello" || len(hello.Panes) == 0 {
		t.Fatalf("unexpected hello: %+v", hello)
	}
	if hello.Panes[0].Cols != 137 {
		t.Fatalf("hello pane cols = %d, want 137", hello.Panes[0].Cols)
	}
}
