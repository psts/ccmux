package api

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/session"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// TestAPI_DaemonRestartAdoption proves the pivot's headline durability guarantee
// under Phase 8: when the daemon restarts, live tmux sessions are re-adopted and
// a lens can re-attach to the surviving session with no loss. We simulate a
// restart by cancelling manager1's context (which kills its control-mode tmux
// clients but leaves the tmux SERVER and its sessions running), then building a
// fresh manager on the same tmux socket + registry DB and calling Start().
func TestAPI_DaemonRestartAdoption(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const socket = "ccmux-adopt-itest"
	tsrv := &tmux.Server{Socket: socket, ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })

	dbPath := t.TempDir() + "/reg.db"

	// --- daemon instance #1: create a workspace and leave a durable marker ---
	store1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store1: %v", err)
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	mgr1 := manager.New(ctx1, tsrv, store1)
	if err := mgr1.Start(); err != nil {
		t.Fatalf("mgr1 start: %v", err)
	}
	ws, err := mgr1.CreateWorkspace("t", "/tmp", "/tmp", "", "tester")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pane0 := ws.Panes[0].ID

	ctrl1 := mgr1.Controller(ws.ID)
	_ = ctrl1.SendInput(pane0, []byte("echo ADOPT_SURVIVE_MARKER\n"))
	if !waitCapture(t, ctrl1, pane0, "ADOPT_SURVIVE_MARKER", 5*time.Second) {
		t.Fatal("marker never rendered before restart")
	}

	// --- simulate the daemon process going away (tmux server survives) ---
	cancel1()          // kills the control-mode client processes bound to ctx1
	_ = store1.Close() // the process is gone; its DB handle closes
	time.Sleep(300 * time.Millisecond)
	if !tsrv.HasSession(ws.TmuxSession) {
		t.Fatal("tmux session did not survive the simulated restart")
	}

	// --- daemon instance #2: reopen the registry + adopt the live session ---
	store2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store2: %v", err)
	}
	defer store2.Close()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	mgr2 := manager.New(ctx2, tsrv, store2)
	if err := mgr2.Start(); err != nil {
		t.Fatalf("mgr2 start (adoption): %v", err)
	}

	adopted := mgr2.Workspace(ws.ID)
	if adopted == nil {
		t.Fatal("workspace not present after restart")
	}
	if adopted.Status != model.StatusLive {
		t.Fatalf("adopted workspace status = %s, want live (session was alive)", adopted.Status)
	}
	if len(adopted.Panes) != 1 || adopted.Panes[0].ID != pane0 {
		t.Fatalf("adopted panes = %+v, want the original pane %s intact", adopted.Panes, pane0)
	}
	if mgr2.Controller(ws.ID) == nil {
		t.Fatal("adopted workspace has no live controller (control connection not reopened)")
	}

	// --- a lens re-attaches to the adopted session and picks up where it left off ---
	hs := httptest.NewServer(NewServer(mgr2).Handler())
	defer hs.Close()

	conn := attachAndHello(t, hs.URL, ws.ID)
	defer conn.Close()

	// The reseed snapshot must reflect the surviving screen (marker still there).
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	sawMarker := false
	for i := 0; i < 50 && !sawMarker; i++ {
		var m wsMsg
		if err := conn.ReadJSON(&m); err != nil {
			break
		}
		if m.T == "snapshot" && m.Pane == pane0 {
			if b, err := base64.StdEncoding.DecodeString(m.Data); err == nil &&
				strings.Contains(string(b), "ADOPT_SURVIVE_MARKER") {
				sawMarker = true
			}
		}
	}
	if !sawMarker {
		t.Fatal("post-adoption attach snapshot lost the surviving screen content")
	}

	// And live I/O works through the re-adopted control connection.
	_ = conn.WriteJSON(wsMsg{T: "input", Pane: pane0,
		Data: base64.StdEncoding.EncodeToString([]byte("echo POST_ADOPT_MARKER\n"))})
	awaitMarker(t, conn, "POST_ADOPT_MARKER")
}

// waitCapture polls a pane's capture until it contains sub or the deadline passes.
func waitCapture(t *testing.T, ctrl *session.Controller, paneID, sub string, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if b, err := ctrl.Capture(paneID, 0); err == nil && strings.Contains(string(b), sub) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
