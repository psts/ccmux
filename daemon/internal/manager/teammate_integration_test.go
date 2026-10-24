package manager

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// A closed session reopened for a teammate comes back like Revive (each
// pane with its own start-up command) with the teammate's command typed
// into the pane Claude last ran in, not a second Claude pane; one that never
// had a Claude pane gets a new one; and one that someone opened after the
// bus found it closed still gets Claude, in a new pane, not nothing.
func TestStartClaudeIn(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	tsrv := &tmux.Server{Socket: "ccmux-teammate-itest", ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })
	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	seen := func(wsID, paneID, marker string) bool {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if ctrl := mgr.Controller(wsID); ctrl != nil {
				if b, _ := ctrl.Capture(paneID, 0); strings.Contains(string(b), marker) {
					return true
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		return false
	}

	ws, err := mgr.CreateWorkspace("admin", "/tmp", "/tmp", "printf DEV_7788", "tester", "")
	if err != nil {
		t.Fatal(err)
	}
	claude, err := mgr.SpawnPane(ws.ID, "/tmp", "printf OLD_CLAUDE_7788", "tester")
	if err != nil {
		t.Fatal(err)
	}
	claude.Harness = "claude"
	if _, err := mgr.ArchiveWorkspace(ws.ID); err != nil {
		t.Fatal(err)
	}
	if !waitStatus(mgr, ws.ID, model.StatusCold, 3*time.Second) {
		t.Fatal("not closed")
	}
	if err := mgr.StartClaudeIn(ws.ID, "printf NEW_CLAUDE_7788"); err != nil {
		t.Fatal(err)
	}
	if !seen(ws.ID, ws.Panes[0].ID, "DEV_7788") || !seen(ws.ID, claude.ID, "NEW_CLAUDE_7788") {
		t.Fatal("the dev pane gets its own command and the claude pane the teammate's")
	}
	if seen(ws.ID, claude.ID, "OLD_CLAUDE_7788") {
		t.Fatal("the claude pane must not also run its old command")
	}
	if n := len(mgr.Workspace(ws.ID).Panes); n != 2 {
		t.Fatalf("panes = %d, want no extra pane when an old claude pane exists", n)
	}

	plain, err := mgr.CreateWorkspace("hq", "/tmp", "/tmp", "printf HQ_7788", "tester", "")
	if err != nil {
		t.Fatal(err)
	}
	mgr.ArchiveWorkspace(plain.ID)
	waitStatus(mgr, plain.ID, model.StatusCold, 3*time.Second)
	if err := mgr.StartClaudeIn(plain.ID, "printf FRESH_CLAUDE_7788"); err != nil {
		t.Fatal(err)
	}
	panes := mgr.Workspace(plain.ID).Panes
	if len(panes) != 2 || !seen(plain.ID, panes[1].ID, "FRESH_CLAUDE_7788") || panes[1].CreatedBy != "claude-peers" {
		t.Fatalf("a session with no claude pane gets a new one: %d panes", len(panes))
	}

	// The bus found it closed; someone opens it before Claude starts.
	mgr.ArchiveWorkspace(ws.ID)
	waitStatus(mgr, ws.ID, model.StatusCold, 3*time.Second)
	if _, err := mgr.ReviveWorkspace(ws.ID); err != nil {
		t.Fatal(err)
	}
	before := len(mgr.Workspace(ws.ID).Panes)
	if err := mgr.StartClaudeIn(ws.ID, "printf RACED_CLAUDE_7788"); err != nil {
		t.Fatal(err)
	}
	after := mgr.Workspace(ws.ID).Panes
	if len(after) != before+1 || !seen(ws.ID, after[len(after)-1].ID, "RACED_CLAUDE_7788") {
		t.Fatalf("an already-open session must still get Claude, in a new pane: %d -> %d panes", before, len(after))
	}
}
