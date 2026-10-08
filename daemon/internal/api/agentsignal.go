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
//	idle         the turn ended              → done (finished), idle
//	permission   it wants to run something   → needs_input (permission, pushes), busy
//	question     it asked the human          → needs_input (question, pushes), busy
//	needs-input  either, unsaid (an agent    → needs_input, no reason, busy
//	             started before the two above existed keeps sending this)
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
	out, ok := agentSignals[req.State]
	if !ok {
		writeError(w, http.StatusBadRequest, `state must be one of "busy", "idle", "permission", "question", "needs-input", "replied"`)
		return
	}
	// One manager call, so the busy clock gets a single write: the old two
	// steps noted the Claude reading of needs-input (idle) before the
	// plugin's busy landed, and a tick between them could end the pane.
	if !s.mgr.ApplyAgentSignal(r.PathValue("id"), out.att, out.reason, out.busy) {
		writeError(w, http.StatusNotFound, "unknown pane")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// agentSignalOutcome is one row of the table above.
type agentSignalOutcome struct {
	att    model.Attention
	reason model.AttentionReason
	busy   bool
}

// agentSignals is the table above; a state it does not list is refused.
var agentSignals = map[string]agentSignalOutcome{
	"busy":        {model.AttentionRunning, model.ReasonNone, true},
	"replied":     {model.AttentionRunning, model.ReasonNone, true},
	"idle":        {model.AttentionDone, model.ReasonFinished, false},
	"permission":  {model.AttentionNeedsInput, model.ReasonPermission, true},
	"question":    {model.AttentionNeedsInput, model.ReasonQuestion, true},
	"needs-input": {model.AttentionNeedsInput, model.ReasonNone, true},
}
