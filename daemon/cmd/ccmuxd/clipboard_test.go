package main

import (
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/config"
)

// TestRenderTmuxConf pins the substitution contract: the conf the tmux server
// actually loads must carry the helper path everywhere and no placeholder —
// a leftover __CCMUX_COPY__ would make every copy binding run a nonsense
// command, and the pipe's own error handling would hide it forever.
func TestRenderTmuxConf(t *testing.T) {
	got := renderTmuxConf(config.TmuxConf, "/run/ccmuxd/ccmux-copy")
	if strings.Contains(got, "__CCMUX_COPY__") {
		t.Error("placeholder survived substitution")
	}
	if n := strings.Count(got, "/run/ccmuxd/ccmux-copy '#{pane_id}'"); n < 13 {
		t.Errorf("helper wired into %d bindings, want all 13 (drag+keys+clicks across tables)", n)
	}
}

// TestClipboardScript pins the helper's load-bearing parts: token read from
// its 0600 file at request time (not baked into the world-readable-ish conf),
// stderr to the log (the one failure the daemon can't record is "the request
// never arrived"), and || true so a dead daemon never breaks copy-mode.
func TestClipboardScript(t *testing.T) {
	s := clipboardScript("/rt/clipboard-token", "http://127.0.0.1:7900", "/rt/clipboard.log")
	for _, want := range []string{
		`$(cat "/rt/clipboard-token"`,
		`"http://127.0.0.1:7900/v1/clipboard"`,
		`2>>"/rt/clipboard.log"`,
		"|| true",
		"X-Ccmux-Pane: $1",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q:\n%s", want, s)
		}
	}
}

// TestLoopbackURL pins the address→origin mapping the clipboard pipe and pane
// env both depend on; "" out of this function silently produces a curl to
// nowhere.
func TestLoopbackURL(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:7900": "http://127.0.0.1:7900",
		"0.0.0.0:7900":   "http://127.0.0.1:7900",
		":7900":          "http://127.0.0.1:7900",
		"[::]:7900":      "http://127.0.0.1:7900",
		"garbage":        "",
	}
	for in, want := range cases {
		if got := loopbackURL(in); got != want {
			t.Errorf("loopbackURL(%q) = %q, want %q", in, got, want)
		}
	}
}
