package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"
)

// paneAgentReply: POST /v1/panes/{id}/agent/reply answers one card without
// the chat socket, for the attention board's tiles: the same body as the
// socket's answer frames, {t:"permission", id, reply}, {t:"question", id,
// answers} or {t:"question-reject", id}. An answer names its request, so a
// card answered elsewhere meanwhile fails at the agent's server (502)
// rather than landing on a different card. 409 when the agent is not
// running: an asleep agent has no cards to answer. 503 when it runs but
// its server does not answer: busy or stuck, and still holding the card.
func (s *Server) paneAgentReply(w http.ResponseWriter, r *http.Request) {
	p := s.agentPaneOr404(w, r)
	if p == nil {
		return
	}
	var f chatFrame
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		writeError(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	if !isCardAnswer(f.T) || f.ID == "" {
		writeError(w, http.StatusBadRequest, `want t "permission", "question" or "question-reject" with an id`)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	oc, err := s.runningAgent(ctx, p)
	if errors.Is(err, errAsleep) {
		writeError(w, http.StatusConflict, "the agent is not running, so there is nothing to answer")
		return
	}
	if err != nil {
		log.Printf("agent reply %s: its server did not answer: %v", p.ID, err)
		silentAgent(w, err)
		return
	}
	c := newChatConn(s, p, cancel)
	if err := c.liveAction(ctx, oc, "", f); err != nil {
		writeError(w, http.StatusBadGateway, "agent server: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func isCardAnswer(t string) bool {
	return t == "permission" || t == "question" || t == "question-reject"
}
