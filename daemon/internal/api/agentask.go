package api

import (
	"context"
	"log"
	"net/http"
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
// nobody could answer. Double-gated like every mutating bus route: loopback
// AND the pane's own bearer token (CCMUX_PANE_TOKEN in the pane's
// environment), so no other local process can relay text as this worker.
func (s *Server) agentAsk(w http.ResponseWriter, r *http.Request) {
	if !requireLoopback(w, r) || !s.peersEnabled(w) {
		return
	}
	paneID := r.PathValue("id")
	if bearerToken(r) == "" {
		writeError(w, http.StatusUnauthorized, "pane token required (CCMUX_PANE_TOKEN in the pane's environment)")
		return
	}
	if !s.peersSvc.AuthorizePane(paneID, bearerToken(r)) {
		writeError(w, http.StatusUnauthorized, "invalid pane token: the daemon's secret changed since this pane started, restart the agent")
		return
	}
	req, ok := decodeAsk(w, r)
	if !ok {
		return
	}
	worker := s.peersSvc.PeerForPane(paneID)
	if worker == "" {
		writeError(w, http.StatusNotFound, "no peer is registered on this pane")
		return
	}
	var n int
	var err error
	if req.Kind == peers.AskPermission {
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

// askReq is the card the sidecar posts: kind and id always, the tool
// fields for a permission, text for a question.
type askReq struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	Tool        string `json:"tool"`
	Description string `json:"description"`
	Preview     string `json:"preview"`
	Text        string `json:"text"`
}

// decodeAsk reads the card and checks it is one the bus can match; ok=false
// means the response was already written.
func decodeAsk(w http.ResponseWriter, r *http.Request) (askReq, bool) {
	var req askReq
	if !decodeJSON(w, r, &req) {
		return req, false
	}
	if !peers.ValidAskID(req.ID) || (req.Kind != peers.AskPermission && req.Kind != peers.AskQuestion) {
		writeError(w, http.StatusBadRequest, `kind must be "permission" or "question", id five lowercase letters without l`)
		return req, false
	}
	return req, true
}

// replyToAgentPane is the bus's ReplyToPane hook: a delegator's verdict or
// answer for a card in a pane whose agent takes it through its chat server.
// A pane without one (a Claude TUI, whose shim resolves its own dialog) is
// ErrNoPanePush. Both paths list the pending cards first: a card the chat
// answered already, or that died with a restarted sidecar, is gone from
// the list, and the reply is dropped with one log line. First answer wins.
func (s *Server) replyToAgentPane(paneID string, reply peers.PaneReply) error {
	p := s.mgr.PaneByID(paneID)
	if p == nil {
		return peers.ErrNoPanePush
	}
	port := agent.ChatPort(p.StartupCommand)
	if port == 0 || s.mgr.PaneAtShell(paneID) {
		return peers.ErrNoPanePush
	}
	oc := agent.OpencodeAt(port)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var answered bool
	var err error
	switch reply.Kind {
	case peers.AskPermission:
		answered, err = replyPermissionIfPending(ctx, oc, reply)
	default:
		answered, err = replyQuestionIfPending(ctx, oc, reply)
	}
	if err == nil && !answered {
		log.Printf("agents: bus reply %s to pane %s: card no longer pending (answered in the chat, or the agent restarted)", reply.RequestID, paneID)
	}
	return err
}

// replyPermissionIfPending answers a pending permission card: allow is
// "once" (never "always": a delegator approves this run, not a rule).
// answered is false when no card with that id is pending.
func replyPermissionIfPending(ctx context.Context, oc *agent.Opencode, reply peers.PaneReply) (answered bool, err error) {
	pending, err := oc.Permissions(ctx)
	if err != nil {
		return false, err
	}
	for _, p := range pending {
		if p.ID != reply.RequestID {
			continue
		}
		answer := "reject"
		if reply.Behavior == "allow" {
			answer = "once"
		}
		return true, oc.ReplyPermission(ctx, p.ID, answer)
	}
	return false, nil
}

// replyQuestionIfPending answers a pending question card with the one text
// the bus carries, under every question on the card (the delegator saw
// them all and answered in one message). answered is false when no card
// with that id is pending.
func replyQuestionIfPending(ctx context.Context, oc *agent.Opencode, reply peers.PaneReply) (answered bool, err error) {
	pending, err := oc.Questions(ctx)
	if err != nil {
		return false, err
	}
	for _, q := range pending {
		if q.ID != reply.RequestID {
			continue
		}
		answers := make([][]string, len(q.Questions))
		for i := range answers {
			answers[i] = []string{reply.Answer}
		}
		return true, oc.ReplyQuestion(ctx, q.ID, answers)
	}
	return false, nil
}
