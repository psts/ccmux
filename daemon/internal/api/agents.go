package api

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/peers"
)

// SetAgents wires the base-agent folder store. Without it the /v1/agents
// routes answer 503, which is how a lens learns the daemon predates agents.
func (s *Server) SetAgents(st *agent.Store, maxRunning int) {
	s.agents, s.agentsMax = st, maxRunning
	s.mgr.Agents = st
	s.mgr.WakeAgent = s.wakeAgent
	s.wirePeersAgents()
}

// wirePeersAgents gives the bus its agent hooks once BOTH the store and the
// peers service are present. Called from SetAgents and EnablePeers so main's
// wiring order does not matter (it bit once: agents were wired first and the
// hooks landed on a nil service, so contact fell through to the repo guess).
func (s *Server) wirePeersAgents() {
	if s.peersSvc == nil || s.agents == nil {
		return
	}
	s.peersSvc.IsAgent = s.isAgent
	s.peersSvc.StartAgent = s.startAgentForPeer
	s.peersSvc.PushToPane = func(paneID, text string) error {
		err := s.mgr.PushToAgentPane(paneID, text)
		if errors.Is(err, manager.ErrNotAgentPane) {
			return peers.ErrNoPanePush
		}
		return err
	}
}

// wakeAgent is the lifecycle loop's restart path for a keep-alive instance
// found asleep: resolve the launch from the CURRENT base (so a drifted
// instance comes back on the new version) and type it into its pane.
func (s *Server) wakeAgent(wsID, name string) error {
	ws := s.mgr.Workspace(wsID)
	if ws == nil {
		return errors.New("unknown workspace")
	}
	p := s.mgr.AgentPane(wsID, name)
	if p == nil {
		return errors.New("agent not added to this workspace")
	}
	if msg := s.agentCapMessage(); msg != "" {
		return errors.New(msg)
	}
	l, _, msg := s.resolveAgentLaunch(ws, name, "")
	if msg != "" {
		return errors.New(msg)
	}
	return s.mgr.StartAgentInPane(p.ID, l)
}

// listAgents: GET /v1/agents → every base definition, instructions included.
// An unreadable base is a 503 with its name, never a silently shorter list.
func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	if s.agents == nil {
		writeError(w, http.StatusServiceUnavailable, "agents are not available on this daemon")
		return
	}
	defs, err := s.agents.List()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": defs})
}

// putAgent: PUT /v1/agents/{name} creates or updates ONE base. Deliberately
// not a whole-list replace like the other settings objects: a base folder
// holds skills and knowledge a human wrote, so removing one must be the
// explicit DELETE, never the side effect of a list that omitted it.
func (s *Server) putAgent(w http.ResponseWriter, r *http.Request) {
	if s.agents == nil {
		writeError(w, http.StatusServiceUnavailable, "agents are not available on this daemon")
		return
	}
	name := r.PathValue("name")
	if !agent.ValidName(name) {
		writeError(w, http.StatusBadRequest, "agent name: use 3 to 24 chars of a-z, 0-9 and -")
		return
	}
	// Merge, not replace: the request carries the fields a lens edits, and
	// json.Unmarshal into the STORED definition leaves every other field as it
	// was — a hand-tuned permission or a pinned model survives a blur-save.
	d, err := s.agents.Get(name)
	if err != nil && !errors.Is(err, agent.ErrNotFound) {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil || json.Unmarshal(body, &d) != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	d.Name = name
	if msg := agent.Reject(d); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if msg := s.rejectAgentHarness(d.Harness); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	stored, err := s.agents.Save(d)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stored)
}

// rejectAgentHarness refuses a harness name the registry cannot resolve, so
// an agent never points at a harness that would 400 at start time. Empty
// means the default and is always fine.
func (s *Server) rejectAgentHarness(name string) string {
	if name == "" || s.mgr.Harnesses == nil {
		return ""
	}
	if _, err := s.mgr.Harnesses.Resolve(name); err != nil {
		return "agent harness: " + err.Error()
	}
	return ""
}

// deleteAgent: DELETE /v1/agents/{name} removes the base folder, knowledge
// and skills included. The lens confirms with the human before calling.
func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	if s.agents == nil {
		writeError(w, http.StatusServiceUnavailable, "agents are not available on this daemon")
		return
	}
	err := s.agents.Delete(r.PathValue("name"))
	switch {
	case errors.Is(err, agent.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such agent")
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		log.Printf("agent %q deleted", r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	}
}
