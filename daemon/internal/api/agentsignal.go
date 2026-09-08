package api

import (
	"net/http"

	"ccmux.dev/ccmuxd/internal/model"
)

// agentSignal: POST /v1/panes/{id}/agent-signal {"state": ...} — the opencode
// ccmux plugin's report of what the agent in that pane is doing. Loopback
// only, like the llm mount: the plugin runs on this host with the pane's
// own environment. Unknown pane → 404, so a stray request records nothing.
//
// The state feeds two things, the same two a Claude pane's hooks feed: the
// lifecycle's busy/idle clock, and the pane's attention (the tab badge, the
// sidebar flash, the push to a phone). Without the attention half an
// opencode agent waiting on a permission card was silent everywhere but
// its own chat view.
//
//	busy         a turn started              → running, busy
//	idle         the turn ended              → done, idle
//	needs-input  a permission or question    → needs_input (pushes), busy
//	replied      that dialog was answered    → running, busy
//
// needs-input counts as busy on purpose: an agent waiting for a human is
// mid-task, and reading it as idle would let the lifecycle's idle cap send
// ctrl-d under the dialog (which opencode ignores, leaving a half-dead
// pane). Claude panes read the same state as idle (claudeBusy); that
// asymmetry is known and this side is the safer one.
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
	att, busy, ok := agentSignalOutcome(req.State)
	if !ok {
		writeError(w, http.StatusBadRequest, `state must be one of "busy", "idle", "needs-input", "replied"`)
		return
	}
	// Attention first: ApplyAttention notes the Claude reading of busy for
	// agent panes, and the signal's own reading must land last.
	paneID := r.PathValue("id")
	s.mgr.ApplyAttention(paneID, att)
	if !s.mgr.NoteAgentSignal(paneID, busy) {
		writeError(w, http.StatusNotFound, "unknown pane")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// agentSignalOutcome is the table above; ok=false for an unknown state.
func agentSignalOutcome(state string) (model.Attention, bool, bool) {
	switch state {
	case "busy":
		return model.AttentionRunning, true, true
	case "idle":
		return model.AttentionDone, false, true
	case "needs-input":
		return model.AttentionNeedsInput, true, true
	case "replied":
		return model.AttentionRunning, true, true
	}
	return "", false, false
}
