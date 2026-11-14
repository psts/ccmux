package main

import (
	"os"
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/peers"
)

// daemonInfoAt points the pane-less discovery file at a fresh config dir and,
// when url is set, writes it there.
func daemonInfoAt(t *testing.T, url, token string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if url == "" {
		return
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg, "ccmuxd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := peers.WriteDaemonInfo(filepath.Join(dir, "peers.json"), url, token); err != nil {
		t.Fatal(err)
	}
}

// Where a session starts, and whom it will ask about the bus afterwards, comes
// from its environment: a hosted pane has its own daemon and token; a plain
// terminal has only the daemon-info file; an older daemon stamped the hub
// straight into the pane, which then asks nobody.
func TestStartTarget(t *testing.T) {
	const fileURL, fileTok = "http://127.0.0.1:7900", "shared-tok"
	for _, c := range []struct {
		name                         string
		paneID, daemonURL, paneToken string // the pane's environment
		legacy                       string // CCMUX_PEERS_URL, from an older daemon
		file                         bool   // is there a daemon-info file?
		wantURL, wantToken, wantPane string // where it starts, and as which pane
		wantLocalURL, wantLocalToken string // whom resolveBus asks ("" = nobody)
	}{
		{"a hosted pane starts on its own daemon and asks it about the bus",
			"pane-1", "http://d:7900", "pane-tok", "", true,
			"http://d:7900", "pane-tok", "pane-1", "http://d:7900", "pane-tok"},
		{"an older daemon stamped the hub: start there, ask nobody",
			"pane-1", "http://d:7900", "hub-tok", "https://hub", true,
			"https://hub", "hub-tok", "pane-1", "", ""},
		{"a plain terminal starts from the daemon-info file and asks too",
			"", "", "", "", true,
			fileURL, fileTok, "", fileURL, fileTok},
		{"a pane with no token is a plain terminal: its pane id is dropped",
			"pane-1", "http://d:7900", "", "", true,
			fileURL, fileTok, "", fileURL, fileTok},
		{"a plain terminal under an older daemon's stamp starts from the file and asks nobody",
			"", "", "", "https://hub", true,
			fileURL, fileTok, "", "", ""},
		{"no daemon anywhere: the default address, no token, nobody to ask",
			"", "", "", "", false,
			"http://127.0.0.1:7890", "", "", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CCMUX_DAEMON_URL", c.daemonURL)
			t.Setenv("CCMUX_PANE_TOKEN", c.paneToken)
			t.Setenv("CCMUX_PEERS_URL", c.legacy)
			if c.file {
				daemonInfoAt(t, fileURL, fileTok)
			} else {
				daemonInfoAt(t, "", "")
			}
			a := &app{paneID: c.paneID}
			url, token := a.startTarget()
			if url != c.wantURL || token != c.wantToken || a.paneID != c.wantPane {
				t.Errorf("starts at %q with token %q as pane %q; want %q, %q, %q", url, token, a.paneID, c.wantURL, c.wantToken, c.wantPane)
			}
			if a.localURL != c.wantLocalURL || a.localToken != c.wantLocalToken {
				t.Errorf("will ask %q with %q about the bus; want %q, %q", a.localURL, a.localToken, c.wantLocalURL, c.wantLocalToken)
			}
		})
	}
}
