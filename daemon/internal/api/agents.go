package api

import (
	"errors"
	"log"
	"net/http"

	"ccmux.dev/ccmuxd/internal/agent"
)

// SetAgents wires the base-agent folder store. Without it the /v1/agents
// routes answer 503, which is how a lens learns the daemon predates agents.
func (s *Server) SetAgents(st *agent.Store, maxRunning int) { s.agents, s.agentsMax = st, maxRunning }

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
	var d agent.Definition
	if !decodeJSON(w, r, &d) {
		return
	}
	d.Name = r.PathValue("name")
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
