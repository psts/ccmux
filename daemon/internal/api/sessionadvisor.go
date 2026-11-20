package api

import (
	"log"
	"net/http"
	"slices"

	"ccmux.dev/ccmuxd/internal/store"
)

// advisors are the advisor models a session may be set to, by Claude
// Code's own aliases (an alias runs the newest of its family); "" goes
// back to what the person's settings choose. Whether one may advise the
// session's model is Claude Code's to say: a pairing it refuses runs
// with no advisor, and the page offers only the ones that rank at or
// above the model (threadadvisor.js).
var advisors = []string{"", "fable", "opus", "sonnet"}

// advisorTaken says whether advisor is one a session may start with, and
// answers 400 when it is not. A new session or thread may name one too.
func advisorTaken(w http.ResponseWriter, advisor string) bool {
	if slices.Contains(advisors, advisor) {
		return true
	}
	writeError(w, http.StatusBadRequest, "not an advisor to pick: "+advisor)
	return false
}

// setRunnerAdvisor: POST /v1/sessions/{id}/advisor with {advisor}. Like a
// model, an advisor applies when the session next starts; the answer says
// so and names the restart route.
func (s *Server) setRunnerAdvisor(w http.ResponseWriter, r *http.Request) {
	if !s.runnersReady(w) {
		return
	}
	var in struct{ Advisor string }
	if !decodeJSON(w, r, &in) {
		return
	}
	if !advisorTaken(w, in.Advisor) {
		return
	}
	id := r.PathValue("id")
	sess, err := s.runnerRows.RunnerSession(id)
	if err != nil {
		// Not "not an agent": an agent's advisor is its base's to name.
		log.Printf("sessions: row of %s not read for its advisor: %v", id, err)
		writeError(w, http.StatusInternalServerError, "the session could not be read, so its advisor was not set; try again")
		return
	}
	if sess != nil && sess.Kind == store.SessionAgent {
		writeError(w, http.StatusConflict, "an agent runs as its base says: change it in the agent editor")
		return
	}
	if err = s.runners.SetAdvisor(id, in.Advisor); err != nil {
		writeRunnerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"advisor": in.Advisor, "applies": "on the next restart", "restart": "/v1/sessions/" + id + "/restart",
	})
}
