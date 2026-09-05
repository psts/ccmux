package api

import (
	"net/http"
)

// agentSignal: POST /v1/panes/{id}/agent-signal {"state":"busy"|"idle"} —
// the opencode ccmux plugin's report of whether the agent in that pane is
// working. Loopback only, like the llm mount: the plugin runs on this host
// with the pane's own environment. Unknown pane → 404, so a stray request
// records nothing.
func (s *Server) agentSignal(w http.ResponseWriter, r *http.Request) {
	if !requireLoopback(w, r) {
		return
	}
	var req struct {
		State string `json:"state"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.State != "busy" && req.State != "idle" {
		writeError(w, http.StatusBadRequest, `state must be "busy" or "idle"`)
		return
	}
	if !s.mgr.NoteAgentSignal(r.PathValue("id"), req.State == "busy") {
		writeError(w, http.StatusNotFound, "unknown pane")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
