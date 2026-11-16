package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
)

// busSessions is sessionsServer with a peers bus and one runner session
// "s1" (pane run1) in the repo's workspace.
func busSessions(t *testing.T) (*Server, *fakeRunners, *peers.Service) {
	t.Helper()
	repo := t.TempDir()
	s, f, st := sessionsServer(t, repo)
	svc := peers.NewService(st, s.mgr, []byte("test-secret-test-secret-test-sec"))
	svc.OpenCmd = ""
	svc.RunnerPane = func(paneID string) (peers.RunnerInfo, bool) {
		info, ok := s.mgr.RunnerPane(paneID)
		return peers.RunnerInfo{Name: info.Name, Repo: info.Repo, Asleep: info.Asleep}, ok
	}
	s.EnablePeers(svc)
	sess := store.RunnerSession{ID: "s1", Cwd: repo, Wanted: store.SessionRunning, PaneID: "run1", WorkspaceID: "ws-app"}
	if err := st.AddRunnerSession(sess); err != nil {
		t.Fatal(err)
	}
	s.mgr.SyncRunnerPane(sess)
	svc.EnsureRunnerPeer("run1", repo)
	return s, f, svc
}

func TestTheBusHandsARunnerItsMessagesThroughItsInbox(t *testing.T) {
	s, f, svc := busSessions(t)
	if svc.RunnerInbox == nil || svc.ReplyToPane == nil {
		t.Fatal("the bus is given the session inbox and the card reply whichever of the two was wired first")
	}
	if err := s.runnerInbox("run1", "prompt-uuid", "hello", "backend"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.sentAs, []string{"s1|prompt-uuid|backend|hello"}) {
		t.Fatalf("queued for the pane's session under the bus's id: %v", f.sentAs)
	}
	if err := s.runnerInbox("tmux1", "p", "hello", "backend"); !errors.Is(err, peers.ErrInboxClosed) {
		t.Fatalf("a pane that is no runner's has no inbox: %v", err)
	}
	s.mgr.SyncRunnerPane(store.RunnerSession{ID: "ended", Wanted: store.SessionRunning, PaneID: "run2", WorkspaceID: "ws-app"})
	if err := s.runnerInbox("run2", "p", "hello", "backend"); !errors.Is(err, peers.ErrInboxClosed) {
		t.Fatalf("an ended session reads nothing, and the bus is told so: %v", err)
	}
}

func TestADelegatorsReplyReachesARunnersCard(t *testing.T) {
	s, f, _ := busSessions(t)
	if err := s.replyToAgentPane("run1", peers.PaneReply{Kind: peers.AskPermission, RequestID: "abcde", Behavior: "allow"}); err != nil {
		t.Fatal(err)
	}
	if err := s.replyToAgentPane("run1", peers.PaneReply{Kind: peers.AskQuestion, RequestID: "gone1", Answer: "blue"}); err != nil {
		t.Fatalf("a card no longer pending is not an error: %v", err)
	}
	want := []runner.CardReply{{Kind: "permission", ID: "abcde", Allow: true}, {Kind: "question", ID: "gone1", Answer: "blue"}}
	if !slices.Equal(f.replies, want) {
		t.Fatalf("got %+v, want %+v", f.replies, want)
	}
	// A delegator who said no must never be heard as yes, and only the one
	// word allows: anything else the bus might carry denies.
	f.replies = nil
	for _, behavior := range []string{"deny", "", "ALLOW", "yes"} {
		if err := s.replyToAgentPane("run1", peers.PaneReply{Kind: peers.AskPermission, RequestID: "fghij", Behavior: behavior}); err != nil {
			t.Fatal(err)
		}
	}
	for i, r := range f.replies {
		if r.Allow || r.ID != "fghij" || r.Kind != "permission" {
			t.Errorf("reply %d allowed the tool, or lost its card: %+v", i, r)
		}
	}
	if len(f.replies) != 4 {
		t.Fatalf("each verdict reached the runner: %+v", f.replies)
	}
	if err := s.replyToAgentPane("tmux1", peers.PaneReply{Kind: peers.AskPermission, RequestID: "abcde"}); !errors.Is(err, peers.ErrNoPanePush) {
		t.Fatalf("a tmux Claude pane still answers its own dialog: %v", err)
	}
}

// The sidecar's two calls, with a runner's pane id: its busy/idle/card
// signals become the pane's attention, and its card is relayed to whoever
// delegated to it.
func TestARunnersSignalsAndCardsGoWhereAPanesGo(t *testing.T) {
	s, _, svc := busSessions(t)
	_, events := s.mgr.SubscribeEvents()
	// The repo's tmux session, registered the way its shim does. The workspace
	// is in no window, so both fall back to the repo's parent folder as their
	// group: the runner's peer was given the repo as its git root for that.
	repo := s.mgr.Workspace("ws-app").RepoPath
	lead := svc.Register(peers.RegisterReq{PaneID: "tmux1", PID: 1, CWD: repo, GitRoot: repo})
	runnerID := svc.PeerForPane("run1")
	if runnerID == "" {
		t.Fatal("the runner's pane has its peer")
	}
	if resp := svc.Send(peers.SendReq{FromID: lead.PeerID, ToID: runnerID, Text: "check the build"}); !resp.OK {
		t.Fatalf("delegate: %+v", resp)
	}
	if w := callLocal(s, "/v1/panes/run1/agent-signal", `{"state":"permission"}`, ""); w.Code != http.StatusNoContent {
		t.Fatalf("signal: %d %s", w.Code, w.Body)
	}
	ev := <-events
	if ev.Kind != "attention" || ev.WorkspaceID != "ws-app" || ev.PaneID != "run1" || ev.Attention != model.AttentionNeedsInput || ev.Reason != model.ReasonPermission {
		t.Fatalf("the firehose carries it with its workspace: %+v", ev)
	}
	var hello *attnEntry
	for _, e := range currentAttention(s.mgr) {
		if e.Pane == "run1" {
			hello = &e
		}
	}
	if hello == nil || hello.Workspace != "ws-app" || hello.State != model.AttentionNeedsInput {
		t.Fatalf("and a lens connecting later is told too, though the repo's terminal session is closed: %+v", hello)
	}

	w := callLocal(s, "/v1/panes/run1/agent-ask", `{"kind":"permission","id":"abcde","tool":"Bash","description":"run it","preview":"make check"}`,
		peers.TokenForPane([]byte("test-secret-test-secret-test-sec"), "run1"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"relayed_to":1`) {
		t.Fatalf("the card reaches its delegator: %d %s", w.Code, w.Body)
	}
	evs, _ := svc.Poll(lead.PeerID)
	if len(evs) != 1 || !strings.Contains(evs[0].Text, "abcde") {
		t.Fatalf("the delegator's mailbox holds the relay: %+v", evs)
	}

	if w := callSessions(t, s, "POST", "/v1/sessions/s1/acted", ""); w.Code != http.StatusNoContent {
		t.Fatalf("acted: %d %s", w.Code, w.Body)
	}
	if p := s.mgr.PaneByID("run1"); p.Attention != model.AttentionIdle {
		t.Fatalf("dealing with it retires its claim: %q", p.Attention)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/nope/acted", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown session: %d", w.Code)
	}
}

// callLocal is a POST from this host, as the sidecar makes them, with the
// pane's token when one is given.
func callLocal(s *Server, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// A runner stopped mid-turn (put to sleep, restarted) says "stopped": its
// pane claims nothing, where "idle" would have flashed a finished turn.
func TestAStoppedRunnerClaimsNothing(t *testing.T) {
	s, _, _ := busSessions(t)
	state := func() (model.Attention, model.AttentionReason) {
		p := s.mgr.PaneByID("run1")
		return p.Attention, p.AttentionReason
	}
	for _, step := range []struct {
		signal string
		want   model.Attention
		reason model.AttentionReason
	}{
		{"busy", model.AttentionRunning, model.ReasonNone},
		{"permission", model.AttentionNeedsInput, model.ReasonPermission},
		{"stopped", model.AttentionIdle, model.ReasonNone}, // the card died with the runner
		{"busy", model.AttentionRunning, model.ReasonNone},
		{"idle", model.AttentionDone, model.ReasonFinished}, // a turn that did end still says so
	} {
		if w := callLocal(s, "/v1/panes/run1/agent-signal", `{"state":"`+step.signal+`"}`, ""); w.Code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", step.signal, w.Code, w.Body)
		}
		if att, reason := state(); att != step.want || reason != step.reason {
			t.Fatalf("after %s: %q/%q, want %q/%q", step.signal, att, reason, step.want, step.reason)
		}
	}
	if w := callLocal(s, "/v1/panes/run1/agent-signal", `{"state":"asleep"}`, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("a state the table does not list is still refused: %d", w.Code)
	}
}
