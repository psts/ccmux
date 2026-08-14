package manager

import (
	"context"
	"fmt"
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

// The case every real workspace is in: more than one pane, at different sizes.
// Each ccmux pane is its own tmux window, and revive creates the session at pane
// 0's size and then spawns the rest into it — so this is also the test of whether
// a spawned window can hold a size of its own at all. If it cannot, per-pane
// restore is broken for every split workspace.
func TestManager_ReviveRestoresEachPanesOwnSize(t *testing.T) {
	mgr, tsrv := paneSizeStack(t, "ccmux-revivemulti-itest")

	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "", "tester", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pane0 := ws.Panes[0].ID
	pane1, err := mgr.SpawnPane(ws.ID, "/tmp", "", "tester")
	if err != nil {
		t.Fatalf("spawn second pane: %v", err)
	}

	// Deliberately different, and different from the default.
	if _, err := mgr.ResizePane(pane0, 131, 41); err != nil {
		t.Fatalf("resize pane0: %v", err)
	}
	if _, err := mgr.ResizePane(pane1.ID, 97, 29); err != nil {
		t.Fatalf("resize pane1: %v", err)
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

	want := map[string]paneDims{pane0: {131, 41}, pane1.ID: {97, 29}}
	for _, p := range revived.Panes {
		w := want[p.ID]
		if p.Cols != w.cols || p.Rows != w.rows {
			t.Errorf("pane %s records %dx%d, want %dx%d", p.ID, p.Cols, p.Rows, w.cols, w.rows)
		}
		// And tmux has to agree, per window — the record is worthless otherwise.
		if cols := tmuxWidths(t, tsrv, ws.TmuxSession)[p.ID]; cols != w.cols {
			t.Errorf("tmux window for pane %s is %d cols, want %d", p.ID, cols, w.cols)
		}
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

// A resize has to reach SQLite, not just memory. Revive reads the in-memory pane,
// so every other test here would still pass with the persist deleted — and the
// whole point is that the size survives a daemon restart.
func TestManager_ResizePanePersists(t *testing.T) {
	mgr, _ := paneSizeStack(t, "ccmux-resizepersist-itest")
	st := mgr.store

	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "", "tester", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pane0 := ws.Panes[0].ID

	if _, err := mgr.ResizePane(pane0, 131, 41); err != nil {
		t.Fatalf("resize: %v", err)
	}

	saved, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, w := range saved {
		if w.ID != ws.ID {
			continue
		}
		for _, p := range w.Panes {
			if p.ID != pane0 {
				continue
			}
			if p.Cols != 131 || p.Rows != 41 {
				t.Fatalf("registry has %dx%d, want 131x41", p.Cols, p.Rows)
			}
			return
		}
	}
	t.Fatalf("pane %s not found in the registry", pane0)
}

// A size tmux cannot honour must be refused at the door. It would otherwise be
// recorded in memory and written by the next UpdatePaneSize, and every revive
// after that would ask new-session for a width tmux silently clamps — the pane
// comes back at 10000 with nothing to say why.
func TestManager_ResizePaneRejectsOutOfRange(t *testing.T) {
	mgr, _ := paneSizeStack(t, "ccmux-resizebounds-itest")

	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "", "tester", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pane0 := ws.Panes[0].ID
	if _, err := mgr.ResizePane(pane0, 120, 35); err != nil {
		t.Fatalf("baseline resize: %v", err)
	}
	before := *mgr.Workspace(ws.ID).Panes[0]

	for _, tc := range []struct {
		name       string
		cols, rows int
	}{
		{"cols too large", maxCols + 1, 40},
		{"rows too large", 120, maxRows + 1},
		{"zero cols", 0, 40},
		{"negative rows", 120, -1},
	} {
		if _, err := mgr.ResizePane(pane0, tc.cols, tc.rows); err == nil {
			t.Fatalf("%s: %dx%d was accepted", tc.name, tc.cols, tc.rows)
		}
	}

	// And the pane still records exactly what it had before — a rejection that
	// left a wrong-but-in-range size (0, or one dimension applied) would be just
	// as broken as one that stored the out-of-range value.
	live := mgr.Workspace(ws.ID)
	if live.Panes[0].Cols != before.Cols || live.Panes[0].Rows != before.Rows {
		t.Fatalf("rejected resizes changed the pane to %dx%d, want %dx%d",
			live.Panes[0].Cols, live.Panes[0].Rows, before.Cols, before.Rows)
	}
}

// A resize that tmux never applied must not be recorded. If it were, the client's
// retry at those same dimensions would compute changed == false and then skip the
// persist, the broadcast and the repaint permanently.
func TestManager_FailedResizeLeavesTheSizeRetryable(t *testing.T) {
	mgr, tsrv := paneSizeStack(t, "ccmux-resizeretry-itest")

	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "", "tester", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pane0 := ws.Panes[0].ID

	// Kill the tmux session so the control connection's resize fails.
	if err := tsrv.KillSession(ws.TmuxSession); err != nil {
		t.Fatalf("kill-session: %v", err)
	}
	if _, err := mgr.ResizePane(pane0, 131, 41); err == nil {
		t.Fatal("resize against a dead session reported success")
	}

	if !waitStatus(mgr, ws.ID, model.StatusCold, 3*time.Second) {
		t.Fatalf("workspace did not go cold after session kill")
	}
	if _, err := mgr.ReviveWorkspace(ws.ID); err != nil {
		t.Fatalf("revive: %v", err)
	}

	// The same dimensions again: still a change, because the failed attempt was
	// never recorded.
	changed, err := mgr.ResizePane(pane0, 131, 41)
	if err != nil {
		t.Fatalf("resize after revive: %v", err)
	}
	if !changed {
		t.Fatal("retry at the failed size reported no change — the failure was recorded as fact")
	}
}

// failingSizeStore is the real registry with UpdatePaneSize switchable to failing,
// which is the one write whose failure has to stay recoverable.
type failingSizeStore struct {
	store.Store
	fail bool
}

func (f *failingSizeStore) UpdatePaneSize(paneID string, cols, rows int) error {
	if f.fail {
		return fmt.Errorf("simulated registry failure")
	}
	return f.Store.UpdatePaneSize(paneID, cols, rows)
}

// A persist that fails must stay retryable. The recorded size is what `changed` is
// computed from, so leaving the new size in memory after a failed write would make
// the client's retry at those same dimensions read false and skip the persist for
// good — a transient disk or lock error turned permanent.
func TestManager_FailedPersistIsRetriedOnTheNextResize(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	tsrv := &tmux.Server{Socket: "ccmux-persistretry-itest", ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })

	real, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = real.Close() })
	st := &failingSizeStore{Store: real}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr := New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "", "tester", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pane0 := ws.Panes[0].ID

	st.fail = true
	if _, err := mgr.ResizePane(pane0, 131, 41); err != nil {
		t.Fatalf("resize: %v", err) // tmux accepted it; only the registry write failed
	}

	// The same size again, with the registry healthy: it must still count as a
	// change, or nothing ever persists it.
	st.fail = false
	changed, err := mgr.ResizePane(pane0, 131, 41)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !changed {
		t.Fatal("the retry reported no change — a failed persist was recorded as done")
	}

	saved, err := real.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, w := range saved {
		for _, p := range w.Panes {
			if p.ID == pane0 {
				if p.Cols != 131 || p.Rows != 41 {
					t.Fatalf("registry has %dx%d after the retry, want 131x41", p.Cols, p.Rows)
				}
				return
			}
		}
	}
	t.Fatal("pane not found in the registry")
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

// tmuxWidths asks tmux for every window's width in a session, keyed by the ccmux
// pane id stamped on the window — the same tag the daemon's own discovery uses.
// Asserting against this rather than the registry is the point: it is the width
// the program in the pane actually got.
func tmuxWidths(t *testing.T, tsrv *tmux.Server, session string) map[string]int {
	t.Helper()
	out, err := exec.Command("tmux", "-L", tsrv.Socket, "list-windows", "-t", session,
		"-F", "#{@ccmux_pane_id}|#{window_width}").Output()
	if err != nil {
		t.Fatalf("list-windows: %v", err)
	}
	widths := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, w, ok := strings.Cut(line, "|")
		if !ok || id == "" {
			continue
		}
		cols, err := strconv.Atoi(strings.TrimSpace(w))
		if err != nil {
			t.Fatalf("parse window_width %q: %v", w, err)
		}
		widths[id] = cols
	}
	return widths
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
