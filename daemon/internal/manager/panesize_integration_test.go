package manager

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// TestManager_ReviveRestoresPaneSize is the ccmuxd-upgrade case. Revive used to
// rebuild every pane at 80x24 and launch its startup command there, so the
// program drew its output wrapped at 80 into a pane a lens would immediately
// widen — text that looks like it came from a smaller screen, and stays that way
// until the user forces a redraw by hand. Revive must open the pane at the size
// it was last drawn at.
func TestManager_ReviveRestoresPaneSize(t *testing.T) {
	mgr, tsrv := paneSizeStack(t, "ccmux-revivesize-itest")

	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "", "tester", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pane0 := ws.Panes[0].ID

	if _, err := mgr.ResizePane(pane0, 131, 41); err != nil {
		t.Fatalf("resize: %v", err)
	}

	if err := tsrv.KillSession(ws.TmuxSession); err != nil {
		t.Fatalf("kill-session: %v", err)
	}
	if !waitStatus(mgr, ws.ID, model.StatusCold, 3*time.Second) {
		t.Fatalf("workspace did not go cold after session kill")
	}

	revived, err := mgr.ReviveWorkspace(ws.ID)
	if err != nil {
		t.Fatalf("revive: %v", err)
	}
	if revived.Panes[0].Cols != 131 || revived.Panes[0].Rows != 41 {
		t.Fatalf("revived pane records %dx%d, want 131x41",
			revived.Panes[0].Cols, revived.Panes[0].Rows)
	}
	// The record is only worth anything if tmux agrees.
	if cols := tmuxWindowCols(t, tsrv, ws.TmuxSession); cols != 131 {
		t.Fatalf("tmux window is %d cols after revive, want 131", cols)
	}
}

// A pane nobody ever sized still has to come back. 0 means "never sized", which
// must fall back to the default rather than asking tmux for a 0-column window.
func TestManager_ReviveUnsizedPaneUsesDefault(t *testing.T) {
	mgr, tsrv := paneSizeStack(t, "ccmux-revivedefault-itest")

	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "", "tester", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := tsrv.KillSession(ws.TmuxSession); err != nil {
		t.Fatalf("kill-session: %v", err)
	}
	if !waitStatus(mgr, ws.ID, model.StatusCold, 3*time.Second) {
		t.Fatalf("workspace did not go cold after session kill")
	}

	revived, err := mgr.ReviveWorkspace(ws.ID)
	if err != nil {
		t.Fatalf("revive: %v", err)
	}
	if revived.Panes[0].Cols != defaultCols || revived.Panes[0].Rows != defaultRows {
		t.Fatalf("revived unsized pane = %dx%d, want %dx%d",
			revived.Panes[0].Cols, revived.Panes[0].Rows, defaultCols, defaultRows)
	}
}

// ResizePane reports whether the pane actually changed. The attach loop uses that
// to decide whether a repaint is owed: an unchanged resize never winches the
// program, so nothing reflowed and there is nothing to redraw.
func TestManager_ResizePaneReportsChange(t *testing.T) {
	mgr, _ := paneSizeStack(t, "ccmux-resizechanged-itest")

	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "", "tester", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pane0 := ws.Panes[0].ID

	changed, err := mgr.ResizePane(pane0, 120, 35)
	if err != nil {
		t.Fatalf("first resize: %v", err)
	}
	if !changed {
		t.Fatal("first resize reported no change")
	}

	changed, err = mgr.ResizePane(pane0, 120, 35)
	if err != nil {
		t.Fatalf("repeat resize: %v", err)
	}
	if changed {
		t.Fatal("resize to the same size reported a change")
	}
}

func paneSizeStack(t *testing.T, socket string) (*Manager, *tmux.Server) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	tsrv := &tmux.Server{Socket: socket, ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })

	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr := New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	return mgr, tsrv
}

// tmuxWindowCols asks tmux itself how wide the session's first window is, so the
// assertion is about the terminal the program actually got, not our bookkeeping.
func tmuxWindowCols(t *testing.T, tsrv *tmux.Server, session string) int {
	t.Helper()
	out, err := exec.Command("tmux", "-L", tsrv.Socket, "display-message", "-p",
		"-t", session, "#{window_width}").Output()
	if err != nil {
		t.Fatalf("display-message: %v", err)
	}
	cols, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse window_width %q: %v", out, err)
	}
	return cols
}
