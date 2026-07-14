package tmux

import (
	"bytes"
	"testing"
)

// The golden case is the exact byte sequence produced by the running tmux 3.6b
// server in the escaping spike: a probe emitting
//
//	S[0x01][0x5c][0x7f][0x1b][0xe2 0x94 0x82]E\n
//
// was rendered by %output as:
//
//	S[\001][\134][<0x7f raw>][\033][<0xe2 0x94 0x82 raw>]E\015\012
func TestUnescapeOutput_GoldenFromLiveServer(t *testing.T) {
	escaped := []byte("S[\\001][\\134][\x7f][\\033][\xe2\x94\x82]E\\015\\012")
	want := []byte("S[\x01][\x5c][\x7f][\x1b][\xe2\x94\x82]E\x0d\x0a")
	got := UnescapeOutput(escaped)
	if !bytes.Equal(got, want) {
		t.Fatalf("unescape mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestUnescapeOutput_PlainASCII(t *testing.T) {
	in := []byte("hello world 123")
	if got := UnescapeOutput(in); !bytes.Equal(got, in) {
		t.Fatalf("plain ASCII should pass through: got %q", got)
	}
}

func TestUnescapeOutput_TrailingLoneBackslash(t *testing.T) {
	// Degenerate input tmux won't actually produce; must not panic or eat bytes.
	in := []byte("ab\\")
	if got := UnescapeOutput(in); !bytes.Equal(got, in) {
		t.Fatalf("lone trailing backslash: got %q want %q", got, in)
	}
}

func TestUnescapeOutput_BackslashNotOctal(t *testing.T) {
	// `\` followed by non-octal digits stays literal.
	in := []byte("a\\9zb")
	if got := UnescapeOutput(in); !bytes.Equal(got, in) {
		t.Fatalf("non-octal escape: got %q want %q", got, in)
	}
}

func TestHexKeys(t *testing.T) {
	got := HexKeys([]byte{0x1b, '[', 'A'})
	want := []string{"1b", "5b", "41"}
	if len(got) != len(want) {
		t.Fatalf("len: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hex[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}
