package api

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"
)

// A pane on its alternate screen (Claude Code in fullscreen, vim, htop) is
// mirrored by a lens only if the lens saw the switch. A snapshot taken while the
// pane is there must therefore carry the switch itself, and leaving must repaint
// every attached lens — without either, a lens seeded mid-session keeps the
// program's last screen under the shell prompt until someone presses Ctrl+L.
func TestAPI_AlternateScreenSnapshots(t *testing.T) {
	_, base := floodStack(t, "ccmux-altscreen-itest")

	ws := createWS(t, base)
	pane0 := ws.Panes[0].ID
	c := attachAndHello(t, base, ws.ID)
	defer c.Close()
	ch := frames(c)
	snapshotsUntilQuiet(ch, pane0, time.Second)
	// A second lens that does nothing itself: the shared-window case. It must
	// be repainted on leave exactly like the first — the fan-out is the point —
	// which is the opposite of the resize rule, where only the resizing lens
	// repaints.
	bystander := attachAndHello(t, base, ws.ID)
	defer bystander.Close()
	bch := frames(bystander)
	snapshotsUntilQuiet(bch, pane0, time.Second)

	typeLine(t, c, pane0, "printf '\\033[?1049h'")
	// Entering asks for no repaint: the program draws itself.
	if got := snapshotsUntilQuiet(ch, pane0, time.Second); got != 0 {
		t.Fatalf("entering the alternate screen gave %d snapshots, want 0", got)
	}

	// A repaint while the pane is there must switch the lens first, then reset.
	if err := c.WriteJSON(wsMsg{T: "repaint", Pane: pane0}); err != nil {
		t.Fatalf("repaint: %v", err)
	}
	b := nextSnapshot(t, ch, pane0)
	if !bytes.HasPrefix(b, []byte(enterAlternateScreen+snapshotReset)) {
		t.Fatalf("snapshot of a pane on its alternate screen does not start with enter+reset: %q", firstBytes(b))
	}

	// Leaving repaints on its own, once, and describes the main screen.
	typeLine(t, c, pane0, "printf '\\033[?1049l'")
	b = nextSnapshot(t, ch, pane0)
	if !bytes.HasPrefix(b, []byte(leaveAlternateScreen+snapshotReset)) {
		t.Fatalf("snapshot after leaving the alternate screen does not leave it: %q", firstBytes(b))
	}
	if got := snapshotsUntilQuiet(ch, pane0, time.Second); got != 0 {
		t.Fatalf("leaving the alternate screen gave %d extra snapshots, want exactly one", got+1)
	}
	b = nextSnapshot(t, bch, pane0)
	if !bytes.HasPrefix(b, []byte(leaveAlternateScreen+snapshotReset)) {
		t.Fatalf("bystander's snapshot after leaving does not leave the alternate screen: %q", firstBytes(b))
	}
	if got := snapshotsUntilQuiet(bch, pane0, time.Second); got != 0 {
		t.Fatalf("bystander got %d extra snapshots on leave, want exactly one", got+1)
	}
}

// typeLine sends one shell line to a pane over the attach socket.
func typeLine(t *testing.T, c interface{ WriteJSON(any) error }, pane, line string) {
	t.Helper()
	if err := c.WriteJSON(wsMsg{T: "input", Pane: pane,
		Data: base64.StdEncoding.EncodeToString([]byte(line + "\n"))}); err != nil {
		t.Fatalf("input %q: %v", line, err)
	}
}

// nextSnapshot returns the decoded bytes of the next snapshot frame for pane.
func nextSnapshot(t *testing.T, ch <-chan wsMsg, pane string) []byte {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				t.Fatal("connection closed before a snapshot arrived")
			}
			if m.T != "snapshot" || m.Pane != pane {
				continue
			}
			b, err := base64.StdEncoding.DecodeString(m.Data)
			if err != nil {
				t.Fatalf("decode snapshot: %v", err)
			}
			return b
		case <-deadline:
			t.Fatal("no snapshot frame arrived")
		}
	}
}
