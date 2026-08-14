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

// The fail-open branch, asserted directly. Being unable to read the parent must
// never be reported as "no flag": that would make the shim run keepRegistered
// instead of runPushLoop and refuse every push, silencing a session that was
// working — the exact harm this design is built around avoiding.
//
// The earlier version of this test never reached the branch. It asserted
// !(known && enabled) against the real `go test` parent, which a plain
// known=true, enabled=false satisfies, so flipping the fail-open `return true`
// to false left it green.
func TestUnreadableParentFailsOpen(t *testing.T) {
	t.Setenv("CCMUX_PEERS_CHANNEL", "")
	defer withParentArgv(nil, false)()

	enabled, known := channelsEnabled()
	if known {
		t.Fatal("an unreadable parent produced a verdict")
	}
	if enabled {
		t.Fatal("an unreadable parent reported channels as enabled")
	}
	if !resolveChannelMode() {
		t.Fatal("an unreadable parent must keep push, not silence the session")
	}
}

// A real nonexistent pid must also read as unreadable, not as an empty argv.
func TestReadingAMissingPidIsUnreadable(t *testing.T) {
	if _, known := readProcessArgv(-1); known {
		t.Fatal("reading a nonexistent pid reported success")
	}
}

// A parent we cannot recognise is not evidence about our session. The repo runs
// pane startup through `sh -c`, so a wrapper between claude and this shim is a
// realistic shape, and concluding "no flag" from someone else's argv would
// silence a healthy session.
func TestUnrecognisedParentIsNotAVerdict(t *testing.T) {
	t.Setenv("CCMUX_PEERS_CHANNEL", "")

	for _, argv := range [][]string{
		{"sh", "-c", "exec ccmux-peers"},
		{"env", "FOO=1", "ccmux-peers"},
		{"tmux", "server"},
	} {
		restore := withParentArgv(argv, true)
		if _, known := channelsEnabled(); known {
			t.Errorf("parent %q was treated as a verdict about our channels", argv)
		}
		if !resolveChannelMode() {
			t.Errorf("parent %q silenced the session", argv)
		}
		restore()
	}
}

// And a parent we DO recognise is judged, including the node-hosted spelling
// where claude is the script rather than the executable.
func TestRecognisedParentsAreJudged(t *testing.T) {
	t.Setenv("CCMUX_PEERS_CHANNEL", "")

	for _, tc := range []struct {
		argv []string
		want bool
	}{
		{[]string{"/usr/local/bin/claude", "--channels", "server:claude-peers"}, true},
		{[]string{"/usr/local/bin/claude", "--resume", "abc"}, false},
		{[]string{"node", "/opt/claude/cli.js", "--channels", "server:claude-peers"}, true},
		{[]string{"node", "/opt/claude/cli.js", "--resume", "abc"}, false},
	} {
		restore := withParentArgv(tc.argv, true)
		if _, known := channelsEnabled(); !known {
			t.Errorf("parent %q was not recognised as a claude session", tc.argv)
		}
		if got := resolveChannelMode(); got != tc.want {
			t.Errorf("resolveChannelMode() for %q = %v, want %v", tc.argv, got, tc.want)
		}
		restore()
	}
}

// withParentArgv installs a fake parent reader and returns its undo.
func withParentArgv(argv []string, ok bool) func() {
	previous := parentArgvReader
	parentArgvReader = func() ([]string, bool) { return argv, ok }
	return func() { parentArgvReader = previous }
}

// The env var stays the escape hatch for a launcher this detection does not
// understand, in both directions.
func TestEnvOverridesDetection(t *testing.T) {
	defer withParentArgv([]string{"claude", "--channels", "server:claude-peers"}, true)()
	t.Setenv("CCMUX_PEERS_CHANNEL", "0")
	if resolveChannelMode() {
		t.Error("CCMUX_PEERS_CHANNEL=0 did not disable push")
	}

	defer withParentArgv([]string{"claude"}, true)()
	t.Setenv("CCMUX_PEERS_CHANNEL", "1")
	if !resolveChannelMode() {
		t.Error("CCMUX_PEERS_CHANNEL=1 did not force push on")
	}
}

// The two verdicts the flag actually drives.
func TestResolveChannelModeFollowsTheParent(t *testing.T) {
	t.Setenv("CCMUX_PEERS_CHANNEL", "")

	restore := withParentArgv([]string{"claude", "--dangerously-load-development-channels", "server:claude-peers"}, true)
	if !resolveChannelMode() {
		t.Error("a flagged session was reported as poll-only")
	}
	restore()

	defer withParentArgv([]string{"claude", "--resume", "abc", "--permission-mode", "auto"}, true)()
	if resolveChannelMode() {
		t.Error("a session with no channel flag still claimed push")
	}
}

// The shim as actually constructed must take its channel mode from detection.
// Testing resolveChannelMode alone leaves the wiring free to drift back to
// trusting an env var, which is the assumption this whole change removes.
func TestNewAppTakesItsChannelModeFromDetection(t *testing.T) {
	t.Setenv("CCMUX_PEERS_CHANNEL", "")

	restore := withParentArgv([]string{"claude", "--resume", "abc"}, true)
	if newApp().channelMode {
		t.Error("a session with no channel flag was built claiming push")
	}
	restore()

	defer withParentArgv([]string{"claude", "--dangerously-load-development-channels", "server:claude-peers"}, true)()
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
