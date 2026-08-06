package manager

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/session"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// TestManager_ClipboardScopedToOwningWorkspace pins the clipboard fan-out's
// privacy invariant with a real tmux: the copy reaches subscribers of the
// workspace OWNING the tmux pane (resolved from its "%N" id — the only id
// tmux itself can supply) and never any other workspace's subscribers. A
// scoping regression here leaks copied text — possibly credentials — across
// users' sessions.
func TestManager_ClipboardScopedToOwningWorkspace(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const socket = "ccmux-clip-itest"
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
	mgr := New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	wsA, err := mgr.CreateWorkspace("a", "/tmp", "/tmp", "", "tester", "")
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	wsB, err := mgr.CreateWorkspace("b", "/tmp", "/tmp", "", "tester", "")
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	subA := mgr.Controller(wsA.ID).Subscribe()
	defer subA.Close()
	subB := mgr.Controller(wsB.ID).Subscribe()
	defer subB.Close()

	// The tmux-side pane id of workspace A's pane 0 — what #{pane_id} would
	// hand the copy pipe.
	out, err := exec.Command("tmux", "-L", socket, "list-panes", "-t", wsA.TmuxSession, "-F", "#{pane_id}").Output()
	if err != nil {
		t.Fatalf("list-panes: %v", err)
	}
	tmuxPane := strings.TrimSpace(string(out))

	if err := mgr.BroadcastClipboard("%99999", []byte("x")); err == nil {
		t.Error("unknown tmux pane must error")
	}
	if err := mgr.BroadcastClipboard(tmuxPane, []byte("secret-copy")); err != nil {
		t.Fatalf("broadcast: %v", err)
	}

	// A must receive it, stamped with the ccmux pane id (not the tmux id).
	if ev, ok := awaitClipboard(subA, 3*time.Second); !ok {
		t.Fatal("workspace A's subscriber never received the clipboard event")
	} else {
		if string(ev.Data) != "secret-copy" {
			t.Errorf("data = %q", ev.Data)
		}
		if ev.PaneID != wsA.Panes[0].ID {
			t.Errorf("pane = %q, want ccmux id %q", ev.PaneID, wsA.Panes[0].ID)
		}
	}
	// B must NOT — drain briefly, ignoring unrelated events (shell output etc.).
	if _, leaked := awaitClipboard(subB, 500*time.Millisecond); leaked {
		t.Fatal("clipboard event leaked to a workspace that does not own the pane")
	}
}

// awaitClipboard drains a subscriber until a clipboard event arrives or the
// timeout passes, skipping unrelated events.
func awaitClipboard(sub *session.Sub, timeout time.Duration) (session.Event, bool) {
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-sub.C:
			if ev.Kind == "clipboard" {
				return ev, true
			}
		case <-deadline:
			return session.Event{}, false
		}
	}
}
