package manager

import (
	"testing"

	"ccmux.dev/ccmuxd/internal/shellint"
)

// TestManager_PaneEnv_InjectsHooksSocket verifies hosted panes are told where the
// daemon's hooks socket is (CCMUX_HOOKS_SOCK), so ccmux-notify.sh routes their
// hooks to the daemon rather than the native app's socket. When unset, the var is
// omitted so a lone app keeps owning the default path.
func TestManager_PaneEnv_InjectsHooksSocket(t *testing.T) {
	const paneID = "pane-env-test"
	t.Cleanup(func() { shellint.Cleanup(paneID) })

	// Not configured → omitted.
	m := &Manager{}
	if _, ok := m.paneEnv(nil, paneID)["CCMUX_HOOKS_SOCK"]; ok {
		t.Fatal("CCMUX_HOOKS_SOCK should be omitted when HooksSocket is empty")
	}

	// Configured → injected verbatim.
	m.HooksSocket = "/tmp/ccmuxd-hooks.sock"
	env := m.paneEnv(nil, paneID)
	if env["CCMUX_HOOKS_SOCK"] != "/tmp/ccmuxd-hooks.sock" {
		t.Fatalf("CCMUX_HOOKS_SOCK = %q, want /tmp/ccmuxd-hooks.sock", env["CCMUX_HOOKS_SOCK"])
	}
	// The pane id must still be present (the hook needs it to attribute the event).
	if env["CCMUX_PANE_ID"] != paneID {
		t.Fatalf("CCMUX_PANE_ID = %q, want %q", env["CCMUX_PANE_ID"], paneID)
	}
}

// TestManager_PaneEnv_AssumesFirstPartyWithTheProxy pins the pair that has to
// travel together. Claude Code turns off its model catalog and the 1M-context
// beta whenever ANTHROPIC_BASE_URL is not api.anthropic.com, so stamping our
// loopback proxy without the assume-first-party flag silently downgrades every
// hosted pane to a static model list and a 200k window.
//
// The flag is tied to the proxy, not stamped unconditionally: with no daemon
// URL there is no base URL either, the CLI is first-party on its own, and
// asserting it would be a claim we are not making.
func TestManager_PaneEnv_AssumesFirstPartyWithTheProxy(t *testing.T) {
	const paneID = "pane-firstparty-test"
	t.Cleanup(func() { shellint.Cleanup(paneID) })

	// No daemon URL → neither var.
	env := (&Manager{}).paneEnv(nil, paneID)
	for _, k := range []string{"ANTHROPIC_BASE_URL", "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s should be omitted when LocalURL is empty, got %q", k, env[k])
		}
	}

	// Proxy configured → base URL points at it AND the flag rides along.
	env = (&Manager{LocalURL: "http://127.0.0.1:7900"}).paneEnv(nil, paneID)
	if want := "http://127.0.0.1:7900/llm/pane/" + paneID; env["ANTHROPIC_BASE_URL"] != want {
		t.Fatalf("ANTHROPIC_BASE_URL = %q, want %q", env["ANTHROPIC_BASE_URL"], want)
	}
	if env["_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL"] != "1" {
		t.Errorf("_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL = %q, want \"1\"; without it the "+
			"proxy base URL costs the pane its model catalog and 1M context",
			env["_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL"])
	}
}
