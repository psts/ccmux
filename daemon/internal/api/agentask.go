package api

import (
	"context"
	"net/http"
	"regexp"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/peers"
)

// agentAsk: POST /v1/panes/{id}/agent-ask — a chat-served agent (the claude
// sidecar) raised a permission or question card in that pane, and asks the
// bus to relay it to whoever delegated to it, the way a Claude TUI's shim
// relays its own dialogs (/v1/peers/permission-request). Loopback only,
// like agent-signal: the sidecar runs on this host with the pane's
// environment. The card stays open in the chat; the first answer from
// either side wins (see replyToAgentPane).
//
//	{"kind":"permission","id":"abcde","tool":"Bash","description":"...","preview":"..."}
//	{"kind":"question","id":"abcde","text":"the card as the chat shows it"}
//
// The id is the five-letter shape the bus's reply matchers accept; the
// worker is the peer registered on the pane (the shim inside the SDK's
// Claude child), and a pane without one gets a 404 rather than a card that
// nobody could answer.
func (s *Server) agentAsk(w http.ResponseWriter, r *http.Request) {
	if !requireLoopback(w, r) {
		return
	}
	var req struct {
		Kind        string `json:"kind"`
		ID          string `json:"id"`
		Tool        string `json:"tool"`
		Description string `json:"description"`
		Preview     string `json:"preview"`
		Text        string `json:"text"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !askIDRe.MatchString(req.ID) || (req.Kind != "permission" && req.Kind != "question") {
		writeError(w, http.StatusBadRequest, `kind must be "permission" or "question", id five lowercase letters without l`)
		return
	}
	if !s.peersEnabled(w) {
		return
	}
	worker := s.peersSvc.PeerForPane(r.PathValue("id"))
	if worker == "" {
		writeError(w, http.StatusNotFound, "no peer is registered on this pane")
		return
	}
	var n int
	var err error
	if req.Kind == "permission" {
		n, err = s.peersSvc.PermissionRequest(worker, req.ID, req.Tool, req.Description, req.Preview)
	} else {
		n, err = s.peersSvc.QuestionRequest(worker, req.ID, req.Text)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "relayed_to": n})
}

// askIDRe is the id alphabet the bus's reply matchers accept (permReplyRe,
// answerReplyRe in peers): five lowercase letters, no l.
var askIDRe = regexp.MustCompile(`^[a-km-z]{5}$`)

// replyToAgentPane is the bus's ReplyToPane hook: a delegator's verdict or
// answer for a card in a pane whose agent takes it through its chat server.
// A pane without one (a Claude TUI, whose shim resolves its own dialog) is
// ErrNoPanePush. Both paths list the pending cards first: a card the chat
// answered already is gone from the list, and the reply is dropped
// without a word, first answer wins.
func (s *Server) replyToAgentPane(paneID string, reply peers.PaneReply) error {
	p := s.mgr.PaneByID(paneID)
	if p == nil || agent.ChatPort(p.StartupCommand) == 0 || s.mgr.PaneAtShell(paneID) {
		return peers.ErrNoPanePush
	}
	oc := agent.OpencodeAt(agent.ChatPort(p.StartupCommand))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if reply.Behavior != "" {
		return replyPermissionIfPending(ctx, oc, reply)
	}
	return replyQuestionIfPending(ctx, oc, reply)
}

// replyPermissionIfPending answers a pending permission card: allow is
// "once" (never "always": a delegator approves this run, not a rule).
func replyPermissionIfPending(ctx context.Context, oc *agent.Opencode, reply peers.PaneReply) error {
	pending, err := oc.Permissions(ctx)
	if err != nil {
		return err
	}
	for _, p := range pending {
		if p.ID != reply.RequestID {
			continue
		}
		answer := "reject"
		if reply.Behavior == "allow" {
			answer = "once"
		}
		return oc.ReplyPermission(ctx, p.ID, answer)
	}
	return nil
}

// replyQuestionIfPending answers a pending question card with the one text
// the bus carries, under every question on the card (the delegator saw
// them all and answered in one message).
func replyQuestionIfPending(ctx context.Context, oc *agent.Opencode, reply peers.PaneReply) error {
	pending, err := oc.Questions(ctx)
	if err != nil {
		return err
	}
	for _, q := range pending {
		if q.ID != reply.RequestID {
			continue
		}
		answers := make([][]string, len(q.Questions))
		for i := range answers {
			answers[i] = []string{reply.Answer}
		}
		return oc.ReplyQuestion(ctx, q.ID, answers)
	}
	return nil
}
