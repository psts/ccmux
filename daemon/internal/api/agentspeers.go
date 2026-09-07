package api

import (
	"errors"
	"net/http"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/manager"
)

// peersAgents: POST /v1/peers/agents {"peer_id"} → the base agents as seen
// from the caller's window (state absent/asleep/running), which is what a
// session needs to know that "x-poster" exists and that messaging it by name
// starts it here. A caller outside a shared window (pane-less, or an
// ungrouped session) sees every agent as absent.
func (s *Server) peersAgents(w http.ResponseWriter, r *http.Request) {
	win, ok := s.peerWindow(w, r)
	if !ok {
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
	for _, d := range defs {
		out = append(out, s.instanceOf(win, d))
	}
	writeJSON(w, http.StatusOK, out)
}

// peersSessions: POST /v1/peers/sessions {"peer_id"} → {"sessions", "shared"}:
// the repo sessions of the caller's window and the window's shared folder
// ("" until an agent's first start there creates it). This is how an agent
// finds the project it was added to: its own folder holds only its memory,
// the code lives in these. A caller outside a shared window sees an empty
// list and no folder.
func (s *Server) peersSessions(w http.ResponseWriter, r *http.Request) {
	win, ok := s.peerWindow(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.windowSessions(win), "shared": s.sharedDirOf(win)})
}

// sharedDirOf is the window's shared folder when it exists, "" otherwise —
// and "" outside any window or without an agent store.
func (s *Server) sharedDirOf(win manager.WindowInfo) string {
	if s.agents == nil || win.ID == "" {
		return ""
	}
	return s.agents.ExistingSharedDir(win.ID, win.Name)
}

// peerWindow authenticates a bus caller and resolves its window. A group
// that is not a window (the directory fallback) resolves to an empty
// WindowInfo, so the caller sees no members. An unreadable window table is
// NOT that: it answers 503, never "nothing here". ok=false means the
// response was already written.
func (s *Server) peerWindow(w http.ResponseWriter, r *http.Request) (manager.WindowInfo, bool) {
	if !s.peersEnabled(w) {
		return manager.WindowInfo{}, false
	}
	var req peerIDReq
	if !decodeJSON(w, r, &req) {
		return manager.WindowInfo{}, false
	}
	if !s.requirePeer(w, r, req.PeerID) {
		return manager.WindowInfo{}, false
	}
	win, status, msg := s.windowByName(s.peersSvc.GroupOfPeer(req.PeerID))
	if msg != "" && status != http.StatusNotFound {
		writeError(w, status, msg)
		return manager.WindowInfo{}, false
	}
	return win, true
}

// startAgentForPeer is the bus's spawn_if_missing path for agents: the one
// add-or-wake flow in the caller's window (its bus group), with the birth
// prompt as first message. An instance already running is not an error: the
// bus delivers to it directly.
func (s *Server) startAgentForPeer(group, name, prompt string) error {
	win, status, msg := s.windowByName(group)
	if status == http.StatusNotFound {
		return errors.New("agent " + name + " joins a shared window, and " + group + " is not one — only a session inside a window can add it")
	}
	if msg != "" {
		return errors.New(msg)
	}
	return peerStartError(s.startWindowAgent(win, name, prompt, "claude-peers"))
}

// peerStartError is the bus's reading of a start: "already running" (a live
// pane, or a start in flight) is not an error — the bus delivers to it
// directly, and a message that arrived during the start reaches it once
// tmux confirms the harness; the cap and the rest are failures.
func peerStartError(out startOutcome) error {
	if out.msg == "" || errors.Is(out.err, manager.ErrAgentRunning) {
		return nil
	}
	return errors.New(out.msg)
}

// isAgent answers the bus's "is this name a base agent" question three
// ways: yes, no, or "it exists but cannot be read" — the last as an error the
// bus hands to the sender, so a corrupt agent.json is reported as that and
// not as a missing repo folder.
func (s *Server) isAgent(name string) (bool, error) {
	if s.agents == nil {
		return false, nil
	}
	_, err := s.agents.Get(name)
	if errors.Is(err, agent.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
