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
	if _, ok := m.paneEnv(paneID)["CCMUX_HOOKS_SOCK"]; ok {
		t.Fatal("CCMUX_HOOKS_SOCK should be omitted when HooksSocket is empty")
	}

	// Configured → injected verbatim.
	m.HooksSocket = "/tmp/ccmuxd-hooks.sock"
	env := m.paneEnv(paneID)
	if env["CCMUX_HOOKS_SOCK"] != "/tmp/ccmuxd-hooks.sock" {
		t.Fatalf("CCMUX_HOOKS_SOCK = %q, want /tmp/ccmuxd-hooks.sock", env["CCMUX_HOOKS_SOCK"])
	}
	// The pane id must still be present (the hook needs it to attribute the event).
	if env["CCMUX_PANE_ID"] != paneID {
		t.Fatalf("CCMUX_PANE_ID = %q, want %q", env["CCMUX_PANE_ID"], paneID)
	}
}
