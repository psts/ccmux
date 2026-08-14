package main

import (
	"os"
	"os/exec"
	"testing"
)

// Claude Code drops channel notifications for a server the session did not load
// and, by its own documentation, returns no error for them. So a session with no
// channels looks exactly like a healthy one over the protocol, and the shim used
// to assume it had push and register poll_only=false regardless. That is how a
// teammate sat "online" through five undelivered replies.
func TestArgvLoadsChannel(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want bool
	}{
		// Captured from a live session on this machine.
		{"the command ccmux spawns", []string{
			"claude", "--dangerously-load-development-channels", "server:claude-peers"}, true},
		// Also captured live — the resume that started this whole investigation.
		{"a hand-resumed session", []string{
			"/home/sanlabs/.local/bin/claude", "--resume", "9f5caac9", "--permission-mode", "auto"}, false},

		{"stable flag", []string{"claude", "--channels", "server:claude-peers"}, true},
		{"equals form", []string{"claude", "--channels=server:claude-peers"}, true},
		{"bare name, no prefix", []string{"claude", "--channels", "claude-peers"}, true},
		{"in a comma list", []string{
			"claude", "--channels", "server:other,server:claude-peers"}, true},
		{"plugin form for us", []string{
			"claude", "--channels", "plugin:claude-peers@marketplace"}, true},

		// The flag is per-server: naming someone else says nothing about us.
		{"another server only", []string{"claude", "--channels", "server:webhook"}, false},
		{"another server in a list", []string{"claude", "--channels", "server:a,server:b"}, false},
		{"prefix of our name", []string{"claude", "--channels", "server:claude-peers-2"}, false},
		{"our name as a substring", []string{"claude", "--channels", "server:xclaude-peers"}, false},

		{"no flags at all", []string{"claude"}, false},
		{"flag with nothing after it", []string{"claude", "--channels"}, false},
		{"empty argv", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := argvLoadsChannel(tc.argv, serverChannelName); got != tc.want {
				t.Errorf("argvLoadsChannel(%q) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

// Being unable to read the parent must never be reported as "no flag": marking a
// healthy session poll-only tells every peer its replies are slow when they are
// not, which is worse than the optimism it replaces.
func TestUnreadableParentIsNotAVerdict(t *testing.T) {
	_, known := readProcessArgv(-1)
	if known {
		t.Fatal("reading a nonexistent pid reported success")
	}

	t.Setenv("CCMUX_PEERS_CHANNEL", "")
	restore := parentArgvOverride
	parentArgvOverride = nil
	defer func() { parentArgvOverride = restore }()

	// With no override, detection runs against this test binary's real parent
	// (`go test`), which carries no channel flag — so it must resolve to a
	// verdict either way without panicking, and the unknown path must default on.
	if enabled, known := channelsEnabled(); known && enabled {
		t.Fatal("the go test harness should not look like a channel-loading session")
	}
}

// The env var stays the escape hatch for a launcher this detection does not
// understand, in both directions.
func TestEnvOverridesDetection(t *testing.T) {
	restore := parentArgvOverride
	defer func() { parentArgvOverride = restore }()

	parentArgvOverride = []string{"claude", "--channels", "server:claude-peers"}
	t.Setenv("CCMUX_PEERS_CHANNEL", "0")
	if resolveChannelMode() {
		t.Error("CCMUX_PEERS_CHANNEL=0 did not disable push")
	}

	parentArgvOverride = []string{"claude"}
	t.Setenv("CCMUX_PEERS_CHANNEL", "1")
	if !resolveChannelMode() {
		t.Error("CCMUX_PEERS_CHANNEL=1 did not force push on")
	}
}

// The two verdicts the flag actually drives.
func TestResolveChannelModeFollowsTheParent(t *testing.T) {
	restore := parentArgvOverride
	defer func() { parentArgvOverride = restore }()
	t.Setenv("CCMUX_PEERS_CHANNEL", "")

	parentArgvOverride = []string{"claude", "--dangerously-load-development-channels", "server:claude-peers"}
	if !resolveChannelMode() {
		t.Error("a flagged session was reported as poll-only")
	}

	parentArgvOverride = []string{"claude", "--resume", "abc", "--permission-mode", "auto"}
	if resolveChannelMode() {
		t.Error("a session with no channel flag still claimed push")
	}
}

// The shim as actually constructed must take its channel mode from detection.
// Testing resolveChannelMode alone leaves the wiring free to drift back to
// trusting an env var, which is the assumption this whole change removes.
func TestNewAppTakesItsChannelModeFromDetection(t *testing.T) {
	restore := parentArgvOverride
	defer func() { parentArgvOverride = restore }()
	t.Setenv("CCMUX_PEERS_CHANNEL", "")

	parentArgvOverride = []string{"claude", "--resume", "abc"}
	if newApp().channelMode {
		t.Error("a session with no channel flag was built claiming push")
	}

	parentArgvOverride = []string{"claude", "--dangerously-load-development-channels", "server:claude-peers"}
	if !newApp().channelMode {
		t.Error("a flagged session was built as poll-only")
	}
}

// readProcessArgv has to work against a real process, not just the parser — this
// is the half that differs per platform and cannot be unit-tested by shape.
func TestReadProcessArgvReadsARealProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a helper process: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	argv, ok := readProcessArgv(cmd.Process.Pid)
	if !ok {
		t.Fatal("could not read a process this test just started")
	}
	if len(argv) < 2 || argv[1] != "30" {
		t.Fatalf("argv = %q, want the sleep command with its argument", argv)
	}
}

// And against our own parent, which is what production actually calls.
func TestReadProcessArgvReadsOurParent(t *testing.T) {
	if argv, ok := readProcessArgv(os.Getppid()); !ok || len(argv) == 0 {
		t.Fatalf("could not read our own parent's argv (ok=%v argv=%q)", ok, argv)
	}
}
