package api

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"ccmux.dev/ccmuxd/internal/agent"
)

// peersAgents: POST /v1/peers/agents {"peer_id"} → the base agents as seen
// from the caller's workspace (state absent/asleep/running), which is what a
// session needs to know that "x-poster" exists and that messaging it by name
// starts it here. Pane-less callers have no workspace, so every agent reads
// as absent.
func (s *Server) peersAgents(w http.ResponseWriter, r *http.Request) {
	if !s.peersEnabled(w) {
		return
	}
	var req peerIDReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.requirePeer(w, r, req.PeerID) {
		return
	}
	out := []agentInstance{}
	if s.agents == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	defs, err := s.agents.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// A pane-less caller has no workspace; instanceOf("") reads every base as absent.
	wsID := s.peersSvc.WorkspaceOfPeer(req.PeerID)
	for _, d := range defs {
		out = append(out, s.instanceOf(wsID, d))
	}
	writeJSON(w, http.StatusOK, out)
}

// startAgentForPeer is the bus's spawn_if_missing path for agents: the one
// start flow in the caller's workspace, with the birth prompt as first
// message. An instance already running is not an error: the bus delivers to
// it directly.
func (s *Server) startAgentForPeer(wsID, name, prompt string) error {
	_, status, msg := s.startAgent(wsID, name, prompt, "claude-peers")
	if msg == "" || status == http.StatusConflict && strings.Contains(msg, "is running") {
		return nil
	}
	return errors.New(msg)
}

// isAgent answers the bus's "is this name a base agent" question. A base
// that exists but cannot be read is logged and reported as not-an-agent,
// so the caller's error names the real cause instead of a repo guess.
func (s *Server) isAgent(name string) bool {
	if s.agents == nil {
		return false
	}
	_, err := s.agents.Get(name)
	if err != nil && !errors.Is(err, agent.ErrNotFound) {
		log.Printf("peers: agent %q unreadable, treated as not an agent: %v", name, err)
	}
	return err == nil
}
