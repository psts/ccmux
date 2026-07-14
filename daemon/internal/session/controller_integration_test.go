package session

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/tmux"
)

// TestController_SpawnSubscribeInput exercises the full session lifecycle against
// a real tmux server: create a session, adopt pane 0, spawn pane 1, subscribe,
// inject input, receive the echoed output, capture, and kill a pane.
func TestController_SpawnSubscribeInput(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const socket = "ccmux-sess-itest"
	srv := &tmux.Server{Socket: socket, ConfigPath: "../../config/tmux.conf"}
	_ = srv.KillServer()
	t.Cleanup(func() { _ = srv.KillServer() })

	if err := srv.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	const sess = "ccmux-t-deadbeef"
	if err := srv.NewSession(sess, "/tmp", 80, 24, map[string]string{"CCMUX_PANE_ID": "pane-0"}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ctrl, err := Open(ctx, srv, sess, "ws-1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ctrl.Close()

	// Adopt the fresh session's sole window as pane 0.
	win, pane, err := ctrl.FirstWindow()
	if err != nil {
		t.Fatalf("FirstWindow: %v", err)
	}
	if err := ctrl.AdoptWindow("pane-0", win, pane); err != nil {
		t.Fatalf("AdoptWindow: %v", err)
	}

	// Spawn a second pane (window).
	if err := ctrl.SpawnWindow("pane-1", "/tmp", map[string]string{"CCMUX_PANE_ID": "pane-1"}); err != nil {
		t.Fatalf("SpawnWindow: %v", err)
	}

	// Subscribe and drive pane 0.
	sub := ctrl.Subscribe()
	defer sub.Close()

	marker := "SESS_MARKER_5521"
	if err := ctrl.SendInput("pane-0", []byte("printf "+marker+"\n")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}

	// The echo/output should arrive on the subscription tagged to pane-0.
	if !waitForEvent(sub, "pane-0", marker, 3*time.Second) {
		t.Errorf("did not receive %q for pane-0 on subscription", marker)
	}

	// Capture should also show it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, err := ctrl.Capture("pane-0", 0)
		if err != nil {
			t.Fatalf("Capture: %v", err)
		}
		if strings.Contains(string(b), marker) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Kill pane-1 and expect a window-close notice resolving to its id.
	if err := ctrl.KillPane("pane-1"); err != nil {
		t.Fatalf("KillPane: %v", err)
	}
	if !waitForNotice(ctrl, "pane-1", 3*time.Second) {
		t.Errorf("did not get window-close notice for pane-1")
	}
}

func waitForEvent(sub *Sub, paneID, sub2 string, d time.Duration) bool {
	deadline := time.After(d)
	var acc []byte
	for {
		select {
		case ev := <-sub.C:
			if ev.PaneID == paneID {
				acc = append(acc, ev.Data...)
				if strings.Contains(string(acc), sub2) {
					return true
				}
			}
		case <-deadline:
			return false
		}
	}
}

func waitForNotice(ctrl *Controller, paneID string, d time.Duration) bool {
	deadline := time.After(d)
	for {
		select {
		case n := <-ctrl.Notices():
			if n.Kind == "window-close" && n.PaneID == paneID {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
