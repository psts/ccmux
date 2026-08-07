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

// TestPaneEnv_ClipShimDisplay pins both directions of the display claim. Set
// unconditionally, askpass and xdg-open take their GUI branch on a headless host
// and panes hang with no visible cause; never set, Claude Code never probes for
// a clipboard tool and the whole shim is inert.
func TestPaneEnv_ClipShimDisplay(t *testing.T) {
	const paneID = "pane-display-test"
	t.Cleanup(func() { shellint.Cleanup(paneID) })
	m := &Manager{}

	if _, ok := m.paneEnv(paneID)["DISPLAY"]; ok {
		t.Error("DISPLAY claimed with no shim installed")
	}
	m.ClipShimReady = true
	if got := m.paneEnv(paneID)["DISPLAY"]; got != ":0" {
		t.Errorf("DISPLAY = %q, want :0 once the shim is confirmed", got)
	}
}
