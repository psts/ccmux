package api

import (
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/peers"
)

// A task's FiledBy is its filer's workspace name and its WindowID the
// window's id; the bus knows neither. The host turns them into the repo's
// folder name in the window's group, which reaches the session there: here
// a workspace called "flood" in repo /tmp, in window "Chart Labs".
func TestTaskHost_TellFilerReachesTheReposSessionByItsFolderName(t *testing.T) {
	f := newWindowAgentFixture(t, "sleep 1;:")
	f.srv.EnablePeers(peers.NewService(f.st, f.srv.mgr, testSecret))
	peer, _ := registerOnPane(t, f, f.ws.Panes[0].ID, "/tmp")
	h := taskHost{mgr: f.srv.mgr, peers: f.srv.peersSvc}
	for i := 0; i < 2; i++ {
		if err := h.TellFiler(f.winID, "flood", "queue-drained-x-1-flood", "the queue is empty", false); err != nil {
			t.Fatal(err)
		}
	}
	evs, _ := f.srv.peersSvc.Poll(peer)
	if len(evs) != 1 || evs[0].FromName != "claude-peers" || !strings.Contains(evs[0].Text, "the queue is empty") {
		t.Fatalf("the repo's session heard %+v, want the one message from the bus", evs)
	}
	for i := 0; i < 2; i++ {
		if err := h.TellFiler(f.winID, "flood", "task-landed-1", "Task #1 merged", true); err != nil {
			t.Fatal(err)
		}
	}
	if pending, err := f.st.PendingNotices(peer); err != nil || len(pending) != 1 {
		t.Fatalf("quiet notices waiting = %v %v, want one", pending, err)
	}
	if err := h.TellFiler(f.winID, "nobody", "", "hi", false); err == nil {
		t.Fatal("a filer with no workspace in the window was reported told")
	}
	if err := h.TellFiler("no-such-window", "flood", "", "hi", false); err == nil {
		t.Fatal("a window with no name was reported told")
	}
	if err := (taskHost{mgr: f.srv.mgr}).TellFiler(f.winID, "flood", "", "hi", false); err == nil {
		t.Fatal("a daemon with no bus reported a filer told")
	}
}
