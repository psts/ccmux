package api

import (
	"bytes"
	"testing"
)

// A snapshot frame must describe the buffer the pane is on in BOTH directions.
// The leave is the one a lens cannot get any other way once it missed the
// program's own: a lagged subscriber drops the output it rode on, and a lens
// that reconnected never received it.
func TestSnapshotFrame_DescribesBothDirections(t *testing.T) {
	rows := []byte("row")
	if got := snapshotFrame(rows, true); !bytes.Equal(got, []byte(enterAlternateScreen+snapshotReset+"row")) {
		t.Fatalf("alt frame = %q", got)
	}
	if got := snapshotFrame(rows, false); !bytes.Equal(got, []byte(leaveAlternateScreen+snapshotReset+"row")) {
		t.Fatalf("main frame = %q", got)
	}
}
