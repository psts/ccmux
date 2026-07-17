package api

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/hooks"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// TestAPI_HostedHookRoutesToDaemonSocket proves the hooks-socket collision fix
// end-to-end with the REAL ccmux-notify.sh: a hosted pane's hook (which carries
// CCMUX_HOOKS_SOCK pointing at the daemon) reaches the DAEMON's socket — not the
// native app's /tmp/ccmux-hooks.sock — and drives the attention broadcast. The
// pre-fix script hardcoded /tmp/ccmux-hooks.sock and ignored CCMUX_HOOKS_SOCK, so
// with the daemon on a distinct socket this test would see no attention (and does
// — verified by reverting the script).
func TestAPI_HostedHookRoutesToDaemonSocket(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	for _, bin := range []string{"bash", "python3", "nc"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed (required by ccmux-notify.sh)", bin)
		}
	}
	script := "../../../hooks/ccmux-notify.sh"
	if _, err := os.Stat(script); err != nil {
		t.Skipf("notify script not found at %s", script)
	}

	const socket = "ccmux-hooksroute-itest"
	tsrv := &tmux.Server{Socket: socket, ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })

	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := manager.New(ctx, tsrv, st)

	// The daemon owns a DISTINCT hooks socket (mirrors the new default that is
	// separate from the app's /tmp/ccmux-hooks.sock). Injected into hosted panes.
	// A short /tmp path (macOS caps Unix socket paths at ~104 bytes; t.TempDir is
	// too long).
	daemonSock := "/tmp/ccmuxd-hooks-routetest.sock"
	_ = os.Remove(daemonSock)
	t.Cleanup(func() { _ = os.Remove(daemonSock) })
	mgr.HooksSocket = daemonSock
	if err := mgr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	hl, err := hooks.Listen(daemonSock, mgr)
	if err != nil {
		t.Fatalf("hooks listen: %v", err)
	}
	defer hl.Close()

	hs := httptest.NewServer(NewServer(mgr).Handler())
	defer hs.Close()

	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "", "tester", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pane0 := ws.Panes[0].ID

	conn := attachAndHello(t, hs.URL, ws.ID)
	defer conn.Close()

	// Run the REAL notify script exactly as a hosted Claude Code hook would: the
	// hosted pane's env carries CCMUX_HOOKS_SOCK (daemon path) + CCMUX_PANE_ID.
	cmd := exec.Command("bash", script, "permission-request")
	cmd.Env = append(os.Environ(),
		"CCMUX_HOOKS_SOCK="+daemonSock,
		"CCMUX_PANE_ID="+pane0,
	)
	cmd.Stdin = strings.NewReader(`{"cwd":"/tmp","session_id":"s1","notification_type":""}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("notify script failed: %v (%s)", err, out)
	}

	// The daemon must have received it and broadcast needs_input for pane0.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for i := 0; i < 200; i++ {
		var m wsMsg
		if err := conn.ReadJSON(&m); err != nil {
			t.Fatalf("read: %v", err)
		}
		if m.T == "attention" && m.Pane == pane0 {
			if string(m.State) != "needs_input" {
				t.Fatalf("attention state = %q, want needs_input", m.State)
			}
			return // success: the hosted hook reached the daemon via CCMUX_HOOKS_SOCK
		}
	}
	t.Fatal("daemon never received the hosted hook via its own socket (routing regressed)")
}
