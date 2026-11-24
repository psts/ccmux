package api

import (
	"context"
	"log"
	"net/http"
	"slices"

	"ccmux.dev/ccmuxd/internal/store"
)

// efforts are the effort levels a session may be set to (Claude Code's
// /effort); "" goes back to the model's own. claudeserve/sessionself.mjs
// EFFORTS is the runner's list of the same.
var efforts = []string{"", "low", "medium", "high", "xhigh", "max"}

// setRunnerEffort: POST /v1/sessions/{id}/effort with {effort}. The row
// keeps it for every later start (--effort), and a running session's
// runner takes it at once. One that does not (a runner from before the
// route) gets it at its next start, and the answer says which.
func (s *Server) setRunnerEffort(w http.ResponseWriter, r *http.Request) {
	if !s.runnersReady(w) {
		return
	}
	var in struct{ Effort string }
	if !decodeJSON(w, r, &in) {
		return
	}
	if !slices.Contains(efforts, in.Effort) {
		writeError(w, http.StatusBadRequest, "not an effort to pick: "+in.Effort)
		return
	}
	id := r.PathValue("id")
	sess, state := s.sessionState(id)
	if sess != nil && sess.Kind == store.SessionAgent {
		writeError(w, http.StatusConflict, "an agent runs as its base says: change it in the agent editor")
		return
	}
	if err := s.runners.SetEffort(id, in.Effort); err != nil {
		writeRunnerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"effort": in.Effort, "applies": s.effortNow(r.Context(), id, state, in.Effort), "restart": "/v1/sessions/" + id + "/restart",
	})
}

// effortNow asks a running session's runner to take the effort at once,
// and says when it applies.
func (s *Server) effortNow(ctx context.Context, id, state, effort string) string {
	if state != store.SessionRunning || s.doors.Dial == nil {
		return "when it next starts"
	}
	ctx, cancel := context.WithTimeout(ctx, runnerActionWithin)
	defer cancel()
	if err := s.doors.Dial(id).SetEffort(ctx, effort); err != nil {
		log.Printf("sessions: effort on %s: the runner did not take it, it applies on the next restart: %v", id, err)
		return "on the next restart"
	}
	return "now"
}
