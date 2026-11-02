package api

import (
	"bytes"
	"testing"

	"ccmux.dev/ccmuxd/internal/tmux"
)

// A snapshot frame must describe the buffer the pane is on in BOTH directions.
// The leave is the one a lens cannot get any other way once it missed the
// program's own: a lagged subscriber drops the output it rode on, and a lens
// that reconnected never received it.
func TestSnapshotFrame_DescribesBothDirections(t *testing.T) {
	rows := []byte("row")
	if got := snapshotFrame(rows, tmux.Screen{Alt: true}); !bytes.Equal(got, []byte(enterAlternateScreen+snapshotReset+"row")) {
		t.Fatalf("alt frame = %q", got)
	}
	if got := snapshotFrame(rows, tmux.Screen{}); !bytes.Equal(got, []byte(leaveAlternateScreen+snapshotReset+"row")) {
		t.Fatalf("main frame = %q", got)
	}
}

// A lens that attaches after the program asked for mouse reports must be told
// too: otherwise its emulator never reports the wheel, and a phone has nothing
// to scroll Claude Code with. Every mode goes off before the ones that are on,
// since 1000, 1002 and 1003 replace each other. Unknown modes say nothing.
func TestSnapshotFrame_RestoresMouseModes(t *testing.T) {
	rows := []byte("row")
	claude := tmux.Screen{Alt: true, Mouse: tmux.MouseModes{Known: true, Standard: true, SGR: true}}
	want := enterAlternateScreen + mouseOff + "\x1b[?1000h\x1b[?1006h" + snapshotReset + "row"
	if got := snapshotFrame(rows, claude); string(got) != want {
		t.Fatalf("claude frame = %q, want %q", got, want)
	}
	off := tmux.Screen{Mouse: tmux.MouseModes{Known: true}}
	if got := snapshotFrame(rows, off); string(got) != leaveAlternateScreen+mouseOff+snapshotReset+"row" {
		t.Fatalf("no-mouse frame = %q", got)
	}
	all := tmux.Screen{Mouse: tmux.MouseModes{Known: true, All: true, Button: true}}
	if got := snapshotFrame(rows, all); string(got) != leaveAlternateScreen+mouseOff+"\x1b[?1002h\x1b[?1003h"+snapshotReset+"row" {
		t.Fatalf("all-motion frame = %q", got)
	}
}
