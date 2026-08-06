//go:build linux

package main

import (
	"strings"
	"testing"
)

func TestRenderUnit(t *testing.T) {
	cfg := serviceConfig{
		BinPath:    "/home/me/.local/bin/ccmuxd",
		Args:       []string{"-addr", "127.0.0.1:7900", "-tsnet", "-tsnet-hostname", "boxb"},
		WorkingDir: "/home/me",
	}
	out := renderUnit(cfg)

	for _, want := range []string{
		"ExecStart=/home/me/.local/bin/ccmuxd -addr 127.0.0.1:7900 -tsnet -tsnet-hostname boxb",
		"Environment=PATH=" + linuxPATH,
		"WorkingDirectory=/home/me",
		"WantedBy=default.target",
		"Restart=on-failure",
		"KillMode=process",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("unit missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "TS_AUTHKEY") {
		t.Errorf("unit leaked TS_AUTHKEY:\n%s", out)
	}
}

func TestExecLine_QuotesSpaces(t *testing.T) {
	got := execLine("/bin/ccmuxd", []string{"-projects-root", "/srv/my projects"})
	want := `/bin/ccmuxd -projects-root "/srv/my projects"`
	if got != want {
		t.Errorf("execLine = %q, want %q", got, want)
	}
}
