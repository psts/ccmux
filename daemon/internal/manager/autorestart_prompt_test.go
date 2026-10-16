package manager

import (
	"strings"
	"testing"
)

const rule = "\x1b[38;5;244m────────────────────────────────────────\x1b[39m"

// screen builds a capture the way CaptureScreen returns it: rows joined by
// CRLF, a cursor restore on the end.
func screen(rows ...string) []byte {
	return []byte(strings.Join(rows, "\r\n") + "\x1b[40;3H")
}

func TestPromptEmpty(t *testing.T) {
	footer := "  0% | Opus 5.5 | voc"
	cases := []struct {
		name string
		s    []byte
		want bool
	}{
		{"bare prompt", screen("hi", rule, "\x1b[39m❯ ", rule, footer), true},
		{"placeholder is dim, so empty",
			screen(rule, "\x1b[39m❯ \x1b[2mTry \"write a test for server.js\"\x1b[0m", rule, footer), true},
		{"typed text", screen(rule, "\x1b[38;5;246m❯\x1b[39m \x1b[38;5;231m/restart\x1b[39m \x1b[49m", rule, footer), false},
		{"plain typed text", screen(rule, "❯ fix the build", rule, footer), false},
		{"text after a dim run ends", screen(rule, "❯ \x1b[2mTry\x1b[22m x", rule, footer), false},
		{"multi-line draft: no rule right under the prompt", screen(rule, "❯ ", "second line", rule, footer), false},
		{"permission dialog, no prompt box", screen("Do you want to proceed?", "❯ 1. Yes", "  2. No", footer), false},
		{"empty screen", screen(""), false},
		{"history echo above an empty box uses the box",
			screen("❯ /clear", rule, "❯ ", rule, footer), true},
		{"history echo above a typed box uses the box",
			screen("❯ ", rule, "❯ draft", rule, footer), false},
	}
	for _, c := range cases {
		if got := promptEmpty(c.s); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
