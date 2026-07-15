package api

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
)

// TestAPI_PaneSnapshot exercises GET /v1/panes/{id}/snapshot: an unknown pane is
// 404, and a live pane returns its current screen (base64 in "data") reflecting
// what was typed — the same seed an attach delivers, but without a WebSocket.
func TestAPI_PaneSnapshot(t *testing.T) {
	_, base := floodStack(t, "ccmux-snapshot-itest")

	ws := createWS(t, base)
	pane0 := ws.Panes[0].ID

	// Unknown pane → 404.
	if code := getStatus(t, base+"/v1/panes/nope/snapshot"); code != http.StatusNotFound {
		t.Fatalf("unknown pane snapshot = %d, want 404", code)
	}

	// Put a durable marker on the screen via an attach, then close it.
	conn := attachAndHello(t, base, ws.ID)
	_ = conn.WriteJSON(wsMsg{T: "input", Pane: pane0,
		Data: base64.StdEncoding.EncodeToString([]byte("echo SNAPSHOT_MARKER_31\n"))})
	awaitMarker(t, conn, "SNAPSHOT_MARKER_31")
	_ = conn.Close()

	// The REST snapshot must contain the marker.
	code, body := getJSON(t, base+"/v1/panes/"+pane0+"/snapshot")
	if code != http.StatusOK {
		t.Fatalf("snapshot status = %d, want 200", code)
	}
	if body["pane"] != pane0 {
		t.Fatalf("snapshot pane = %v, want %s", body["pane"], pane0)
	}
	dataStr, _ := body["data"].(string)
	raw, err := base64.StdEncoding.DecodeString(dataStr)
	if err != nil {
		t.Fatalf("snapshot data not base64: %v", err)
	}
	if !strings.Contains(string(raw), "SNAPSHOT_MARKER_31") {
		t.Fatalf("snapshot did not reflect the current screen (marker missing)")
	}
}
