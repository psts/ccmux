package api

import (
	"errors"
	"net/http"
)

// peersAgents: POST /v1/peers/agents {"peer_id"} → the base agents as seen
// from the caller's workspace (state absent/asleep/running), which is what a
// session needs to know that "x-poster" exists and that messaging it by name
// starts it here. Pane-less callers have no workspace and get an empty list.
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
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	wsID := s.peersSvc.WorkspaceOfPeer(req.PeerID)
	for _, d := range defs {
		inst := agentInstance{Name: d.Name, Icon: d.Icon, Description: d.Description, Version: d.Version, State: "absent"}
		if wsID != "" {
			inst = s.instanceOf(wsID, d)
		}
		out = append(out, inst)
	}
	writeJSON(w, http.StatusOK, out)
}

// startAgentForPeer is the bus's spawn_if_missing path for agents: add or
// wake the base in the caller's workspace with the birth prompt as first
// message. Mirrors startOrWake without an HTTP response.
func (s *Server) startAgentForPeer(wsID, name, prompt string) error {
	ws := s.mgr.Workspace(wsID)
	if ws == nil {
		return errors.New("unknown workspace")
	}
	if msg := s.agentCapMessage(); msg != "" {
		return errors.New(msg)
	}
	l, _, msg := s.resolveAgentLaunch(ws, name, prompt)
	if msg != "" {
		return errors.New(msg)
	}
	if p := s.mgr.AgentPane(wsID, name); p != nil {
		if !s.mgr.PaneAtShell(p.ID) {
			return nil // already running: the bus delivers to it directly
		}
		if err := s.mgr.StartAgentInPane(p.ID, l); err != nil {
			return err
		}
		s.pushPromptLater(l)
		return nil
	}
	if _, err := s.mgr.SpawnAgentPane(wsID, "claude-peers", l); err != nil {
		return err
	}
	s.pushPromptLater(l)
	return nil
}

// isAgent answers the bus's "is this name a base agent" question.
func (s *Server) isAgent(name string) bool {
	if s.agents == nil {
		return false
	}
	_, err := s.agents.Get(name)
	return err == nil
}
