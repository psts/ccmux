package main

import (
	"os"
	"path/filepath"
	"strings"
)

// serverChannelName is how this shim is named to Claude Code's channel flags —
// the value after "server:" in `--dangerously-load-development-channels
// server:claude-peers`. It matches the MCP server name in .mcp.json.
const serverChannelName = "claude-peers"

// channelsEnabled reports whether pushes from this shim will actually reach the
// session, and whether that answer is worth acting on.
//
// This has to be settled out of band. Claude Code drops channel notifications
// for a server the session did not load, and — by its own documentation —
// "returns no error to your server", so a deaf session is indistinguishable from
// a healthy one over the protocol. The one place the truth exists is the flags
// the session was started with, which is the parent process's command line.
//
// Getting this wrong in the pessimistic direction is the greater harm, and not
// only cosmetically: a false verdict makes the shim run keepRegistered instead
// of runPushLoop and refuse every push, so it does not just mislabel a healthy
// session, it silences one. `known` is therefore false unless we both read an
// argv AND recognise it as a Claude Code session — an unreadable parent, a
// platform without a reader, and a wrapper process (`sh -c`, `env`, a
// relauncher) all land there and keep the caller's optimism.
func channelsEnabled() (enabled, known bool) {
	argv, ok := parentCommandLine()
	if !ok || len(argv) == 0 {
		return false, false
	}
	if !looksLikeClaude(argv) {
		// Reading someone else's argv tells us nothing about our session's flags.
		// Logged with what we saw, so a wrong verdict is one line to diagnose
		// rather than a mystery about why push went quiet.
		logf("parent %q does not look like a claude session — not concluding anything about channels", argv[0])
		return false, false
	}
	return argvLoadsChannel(argv, serverChannelName), true
}

// looksLikeClaude reports whether an argv plausibly belongs to the Claude Code
// session that started this shim, rather than to something between us and it.
//
// Deliberately generous: the cost of failing to recognise claude is one session
// that keeps the old optimistic default, while the cost of mistaking a wrapper
// for claude is a session silenced on the strength of somebody else's flags.
func looksLikeClaude(argv []string) bool {
	if strings.Contains(filepath.Base(argv[0]), "claude") {
		return true
	}
	// A node-hosted CLI: the interpreter is argv[0] and claude's entry point is
	// the script it was handed.
	for _, arg := range argv[1:] {
		if strings.Contains(arg, "claude") {
			return true
		}
	}
	return false
}

// argvLoadsChannel reports whether a Claude Code command line loads `server` as
// a channel.
//
// The flag is per-server, not a global switch: `--channels server:a` says
// nothing about server b. Both the stable `--channels` and the development flag
// are accepted, in either `--flag value` or `--flag=value` form, and a value may
// carry a comma-separated list. A bare name (no "server:" prefix) counts too,
// since the flag also takes `plugin:` forms and the prefix is not what
// identifies us.
func argvLoadsChannel(argv []string, server string) bool {
	for i, arg := range argv {
		name, value, hasValue := strings.Cut(arg, "=")
		if !isChannelFlag(name) {
			continue
		}
		if !hasValue {
			if i+1 >= len(argv) {
				continue // trailing flag with nothing after it
			}
			value = argv[i+1]
		}
		if valueNames(value, server) {
			return true
		}
	}
	return false
}

func isChannelFlag(arg string) bool {
	return arg == "--channels" || arg == "--dangerously-load-development-channels"
}

// valueNames reports whether a channel flag's value selects `server`, allowing
// the "server:"/"plugin:" prefixes and comma-separated lists.
func valueNames(value, server string) bool {
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if _, after, found := strings.Cut(entry, ":"); found {
			entry = after
		}
		// A plugin entry is "name@marketplace"; compare on the name.
		if before, _, found := strings.Cut(entry, "@"); found {
			entry = before
		}
		if entry == server {
			return true
		}
	}
	return false
}

// newApp builds the shim with its channel mode already decided. Separate from
// main so a test can assert that the decision is DETECTED rather than assumed —
// the detection is worthless if the wiring quietly goes back to trusting an env
// var, and nothing else would notice.
func newApp() *app {
	return &app{mcp: newMCPServer(), channelMode: resolveChannelMode()}
}

// resolveChannelMode decides whether this session will be pushed to.
//
// The env var wins in both directions — it is the escape hatch for a launcher
// this detection does not understand — and otherwise the parent's argv decides.
// An unreadable parent keeps the old optimistic default: claiming push we might
// not have is the lesser error, because the alternative tells every peer that a
// perfectly healthy session is slow.
//
// A session that IS deaf now says so at registration (poll_only), which is what
// turns "peer never replied" from a mystery into a line in `list_peers`.
func resolveChannelMode() bool {
	switch os.Getenv("CCMUX_PEERS_CHANNEL") {
	case "0":
		logf("channel push disabled by CCMUX_PEERS_CHANNEL=0")
		return false
	case "1":
		logf("channel push forced on by CCMUX_PEERS_CHANNEL=1")
		return true
	}
	enabled, known := channelsEnabled()
	switch {
	case !known:
		logf("could not read the session's flags (parent pid %d) — assuming channel push works. "+
			"Set CCMUX_PEERS_CHANNEL=0 if this session in fact has no channels.", os.Getppid())
		return true
	case !enabled:
		logf("this session was NOT started with --dangerously-load-development-channels server:%s, "+
			"so pushed messages would be dropped silently. Registering poll-only: peers will be "+
			"told delivery waits for check_messages. Restart with the flag for live push.",
			serverChannelName)
		return false
	}
	return true
}

// parentArgvReader lets the tests drive parentCommandLine without a real process
// tree. A function rather than a slice so a test can express "unreadable" — the
// fail-open branch is the load-bearing safety property here, and a slice-shaped
// seam could not reach it, which is how it went untested the first time.
var parentArgvReader func() ([]string, bool)

func parentCommandLine() ([]string, bool) {
	if parentArgvReader != nil {
		return parentArgvReader()
	}
	return readProcessArgv(os.Getppid())
}
