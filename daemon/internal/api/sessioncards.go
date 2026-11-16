package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
)

// What a human does to a runner session from its page besides typing: answer
// a card it raised (a permission, a question) and stop its turn. Both go
// straight to the session's runner, which is the one that knows whether the
// card still waits and whether a turn runs.

// runnerActionWithin bounds one call to a runner made for a click.
const runnerActionWithin = 20 * time.Second

// answerRunnerCard: POST /v1/sessions/{id}/cards/{card} with
// {kind:"permission", reply:"once"|"always"|"reject"} or
// {kind:"question", answers:[[label,...],...]} or {kind:"question", reject:true}.
// 204 when the runner took it; 409 when the card no longer waits (the first
// answer won, or it went with a restarted runner); 502 when the runner did
// not answer. A taken answer closes the card's row here, rather than at the
// supervisor's next pass, so nothing reads it as open in between.
func (s *Server) answerRunnerCard(w http.ResponseWriter, r *http.Request) {
	if !s.doorsReady(w) {
		return
	}
	id := r.PathValue("id")
	if sess, err := s.runnerRows.RunnerSession(id); sess == nil {
		writeRunnerError(w, orNoRunner(err))
		return
	}
	var a runner.Answer
	if !decodeJSON(w, r, &a) {
		return
	}
	a.ID = r.PathValue("card")
	ctx, cancel := context.WithTimeout(r.Context(), runnerActionWithin)
	defer cancel()
	answered, err := s.doors.Dial(id).AnswerCard(ctx, a)
	switch {
	case errors.Is(err, runner.ErrBadAnswer):
		writeError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeError(w, http.StatusBadGateway, "the session's runner did not take the answer: "+err.Error())
	case !answered:
		writeError(w, http.StatusConflict, "that card is no longer waiting: it was answered already, or the session restarted")
	default:
		if err := s.runnerRows.SetSessionCardState(id, a.ID, store.CardAnswered, time.Now().UnixMilli()); err != nil {
			log.Printf("sessions: %s: card %s was answered but its row was not closed (the next pass closes it): %v", id, a.ID, err)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// abortRunnerSession: POST /v1/sessions/{id}/abort is Stop: the running
// turn ends, the session stays. 409 when the session's runner is not up, so
// there is no turn to stop.
func (s *Server) abortRunnerSession(w http.ResponseWriter, r *http.Request) {
	if !s.doorsReady(w) {
		return
	}
	sess, state := s.sessionState(r.PathValue("id"))
	if sess == nil {
		writeRunnerError(w, store.ErrNoSession)
		return
	}
	if state != store.SessionRunning {
		writeError(w, http.StatusConflict, "the session is not running, so there is no turn to stop")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), runnerActionWithin)
	defer cancel()
	if err := s.doors.Dial(sess.ID).Abort(ctx, sess.ID); err != nil {
		writeError(w, http.StatusBadGateway, "the session's runner did not take the stop: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
