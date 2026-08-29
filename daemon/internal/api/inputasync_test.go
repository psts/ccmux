package api

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"
)

// TestApplyInput_DoesNotBlockItsGoroutine pins the head-of-line fix at the layer
// the bug lived on. applyInput runs on the attach read goroutine, which also
// dispatches resize, repaint and focus and owns the websocket read deadline, so
// a synchronous send froze every other pane on that lens for the whole paste —
// about 3s for 1 MB, measured against tmux 3.4.
//
// The queue lives two layers down (internal/tmux/sender.go) and is tested
// there. What is only testable HERE is that this caller still uses the
// non-blocking entry point: swap SendInputAsync back to SendInput and every
// other test in the tree stays green.
func TestApplyInput_DoesNotBlockItsGoroutine(t *testing.T) {
	mgr, base := floodStack(t, "ccmux-applyinput-itest")
	ws := createWS(t, base)
	ctrl := mgr.Controller(ws.ID)
	if ctrl == nil {
		t.Fatal("no controller for a freshly created workspace")
	}
	s := NewServer(mgr)

	// Big enough that a synchronous send is unmistakably slower than the
	// threshold: ~256 kB is roughly 0.75s of round trips at the current chunk
	// size, three times the budget below.
	big := bytes.Repeat([]byte("x"), 256*1024)
	msg := wsMsg{
		T:    "input",
		Pane: ws.Panes[0].ID,
		Data: base64.StdEncoding.EncodeToString(big),
	}

	start := time.Now()
	s.applyInput(ctrl, msg, ws.ID, "conn-under-test", newNoticeQueue())
	took := time.Since(start)

	// Loose on purpose. The claim is "returns without waiting for tmux", not a
	// latency budget that would flake on a busy host.
	if took > 250*time.Millisecond {
		t.Errorf("applyInput blocked for %v on a %d-byte paste — the attach read "+
			"goroutine is stalled again, and with it resize, repaint, focus and "+
			"the read deadline", took, len(big))
	}
}
