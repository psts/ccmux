package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
)

// What the thread page does to a running session beside prompting it:
// switch its permission mode, stop a background task (runner/live.go).
// Both go straight to the session's runner, like Stop.

// setRunnerMode: POST /v1/sessions/{id}/mode with {mode}. 204 when the
// runner took it; 409 in its words when it did not (a mode it does not
// offer) or the session is not running; 502 when it did not answer.
func (s *Server) setRunnerMode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode string `json:"mode"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	s.runnerAction(w, r, "switch modes", func(ctx context.Context, sr SessionRunner) error { return sr.SetMode(ctx, in.Mode) })
}

// stopRunnerTask: POST /v1/sessions/{id}/tasks/{task}/stop. Answers as
// setRunnerMode does.
func (s *Server) stopRunnerTask(w http.ResponseWriter, r *http.Request) {
	task := r.PathValue("task")
	s.runnerAction(w, r, "stop a task", func(ctx context.Context, sr SessionRunner) error { return sr.StopTask(ctx, task) })
}

// runnerAction runs one page action on a running session's runner.
func (s *Server) runnerAction(w http.ResponseWriter, r *http.Request, what string, act func(context.Context, SessionRunner) error) {
	if !s.doorsReady(w) {
		return
	}
	sess, state := s.sessionState(r.PathValue("id"))
	if sess == nil {
		writeRunnerError(w, store.ErrNoSession)
		return
	}
	if state != store.SessionRunning {
		writeError(w, http.StatusConflict, "the session is not running, so there is nothing to "+what+" in")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), runnerActionWithin)
	defer cancel()
	err := act(ctx, s.doors.Dial(sess.ID))
	switch {
	case errors.Is(err, runner.ErrRunnerSaidNo):
		writeError(w, http.StatusConflict, strings.TrimPrefix(err.Error(), runner.ErrRunnerSaidNo.Error()+": "))
	case err != nil:
		writeError(w, http.StatusBadGateway, "the session's runner did not take it: "+err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
