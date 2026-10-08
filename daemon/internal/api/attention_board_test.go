package api

import (
	"context"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// The firehose frame carries the claim (why, since when), which is what the
// attention board sorts by.
func TestFirehoseFrame_CarriesTheClaim(t *testing.T) {
	srv, _ := presenceServer(t)
	got := srv.firehoseFrame(manager.Event{Kind: "attention", WorkspaceID: "w", PaneID: "p",
		Attention: model.AttentionNeedsInput, Reason: model.ReasonQuestion, Since: 42}, firehoseReader{})
	if got.Reason != model.ReasonQuestion || got.Since != 42 || got.State != model.AttentionNeedsInput {
		t.Fatalf("frame %+v", got)
	}
}

// TestAPI_BoardActsOnOnePaneAndWatchesNothing: the attention board reports a
// live screen with no pane named. That must count as present (no phone buzz at
// the desk) without counting as watching (or every tile would retire the
// moment it arrived), and its "acted" frame retires the one pane it names,
// leaving the workspace's other claim standing.
func TestAPI_BoardActsOnOnePaneAndWatchesNothing(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const socket = "ccmux-board-itest"
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
	if err := mgr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	srv := NewServer(mgr)
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "", "tester", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pane0 := ws.Panes[0].ID
	second, err := mgr.SpawnPane(ws.ID, "/tmp", "", "tester")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	pane1 := second.ID

	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/v1/attach?workspace=" + ws.ID
	board, _, err := websocket.DefaultDialer.Dial(wsURL+"&user=a&device=mac", nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer board.Close()
	other, _, err := websocket.DefaultDialer.Dial(wsURL+"&user=b&device=web", nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer other.Close()
	for _, c := range []*websocket.Conn{board, other} {
		if m := readMsg(t, c); m.T != "hello" {
			t.Fatalf("first frame = %q, want hello", m.T)
		}
	}

	present := true
	if err := board.WriteJSON(wsMsg{T: "focus", Present: &present}); err != nil {
		t.Fatalf("focus: %v", err)
	}
	waitForOwner(t, srv, "a", true)
	if srv.presence.Watched(ws.ID) {
		t.Fatal("a board with no pane named counts as watching")
	}

	mgr.ApplyAttention(pane0, model.AttentionNeedsInput, model.ReasonPermission)
	mgr.ApplyAttention(pane1, model.AttentionDone, model.ReasonFinished)
	if got := paneAttention(mgr, ws.ID); got[pane0] != model.AttentionNeedsInput || got[pane1] != model.AttentionDone {
		t.Fatalf("claims retired with only the board open: %v", got)
	}

	if err := board.WriteJSON(wsMsg{T: "acted", Pane: pane0}); err != nil {
		t.Fatalf("acted: %v", err)
	}
	waitAttention(t, other, pane0, model.AttentionNeedsInput)
	waitAttention(t, other, pane0, model.AttentionIdle)
	if got := paneAttention(mgr, ws.ID)[pane1]; got != model.AttentionDone {
		t.Fatalf("the other pane = %q, want its claim left standing", got)
	}
}

func paneAttention(mgr *manager.Manager, wsID string) map[string]model.Attention {
	out := map[string]model.Attention{}
	for _, ws := range mgr.List() {
		if ws.ID != wsID {
			continue
		}
		for _, p := range ws.Panes {
			out[p.ID] = p.Attention
		}
	}
	return out
}
