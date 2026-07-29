//go:build darwin

package main

import (
	"strings"
	"testing"
)

func TestRenderPlist(t *testing.T) {
	cfg := serviceConfig{
		BinPath:    "/Users/me/.local/bin/ccmuxd",
		Args:       []string{"-addr", "127.0.0.1:7900", "-tsnet", "-tsnet-hostname", "boxb"},
		WorkingDir: "/Users/me",
	}
	out := renderPlist(cfg, "/Users/me/Library/Logs/ccmuxd.log")

	for _, want := range []string{
		"<string>" + darwinLabel + "</string>",
		"<string>/Users/me/.local/bin/ccmuxd</string>",
		"<string>-tsnet-hostname</string>",
		"<string>boxb</string>",
		"<key>PATH</key>",
		darwinPATH,
		"/Users/me/Library/Logs/ccmuxd.log",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing %q\n%s", want, out)
		}
	}
	// TS_AUTHKEY must never appear: the join happens at install time, not in the unit.
	if strings.Contains(out, "TS_AUTHKEY") {
		t.Errorf("plist leaked TS_AUTHKEY:\n%s", out)
	}
}

func TestXMLEsc(t *testing.T) {
	if got := xmlEsc(`a&b<c>"d`); got != "a&amp;b&lt;c&gt;&quot;d" {
		t.Errorf("xmlEsc = %q", got)
	}
}
