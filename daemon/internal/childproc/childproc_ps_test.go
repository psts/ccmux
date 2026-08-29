package childproc

import "testing"

// TestParsePS_RealMacOSShape is the test the macOS path did not have. The
// census shipped with `ps -eo "ppid=,stat="`, which BSD ps parses as ONE
// keyword headed ",stat=" — one column, every line skipped, a permanent
// Known:true/Defunct:0 all-clear on the platform the Mac lens runs on. Nothing
// caught it because GNU ps accepts the comma form and the darwin file could
// not be unit-tested at all.
func TestParsePS_RealMacOSShape(t *testing.T) {
	// Two columns, leading-space aligned, as `ps -A -o ppid= -o stat=` emits.
	const out = `    1 Ss
  501 S
  501 Z
  501 Z+
  501 S+
    1 Ss
  999 Z
`
	got := parsePS([]byte(out), 501)
	if got.Live != 2 || got.Defunct != 2 {
		t.Errorf("parsePS = %+v, want Live 2 Defunct 2 (children of 501 only)", got)
	}
	if !got.Known {
		t.Error("parsePS must report Known on output it could read")
	}

	// A different parent picks up only its own.
	if got := parsePS([]byte(out), 999); got.Live != 0 || got.Defunct != 1 {
		t.Errorf("parsePS(pid=999) = %+v, want Live 0 Defunct 1", got)
	}
}

// TestParsePS_SingleColumnIsNotAClean Bill pins the failure mode directly: if
// the ps invocation ever regresses to one column, the parser must not report a
// confident zero. It cannot tell "no children" from "unusable output" on its
// own — which is exactly why the invocation is commented so heavily — so this
// test documents the boundary rather than asserting a fix.
func TestParsePS_SingleColumnIsNotACleanBill(t *testing.T) {
	got := parsePS([]byte("    1\n  501\n  501\n"), 501)
	if got.Live != 0 || got.Defunct != 0 {
		t.Fatalf("parsePS on one-column output = %+v, want zeroes", got)
	}
	t.Log("one-column output yields zeroes and Known:true — the reason the ps " +
		"flags in childproc_other.go must stay one -o per keyword")
}

func TestParsePS_IgnoresGarbageLines(t *testing.T) {
	const out = "\n   \nnotanumber Ss\n  501 Z\nheader junk here\n"
	if got := parsePS([]byte(out), 501); got.Live != 0 || got.Defunct != 1 {
		t.Errorf("parsePS = %+v, want Live 0 Defunct 1", got)
	}
}
