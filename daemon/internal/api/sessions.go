package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
)

// Session runners (internal/runner): Claude sessions on the Agent SDK in
// their own systemd service. These routes are the first, lens-less surface:
// make one, prompt it, put it to sleep or end it, switch its model. Same
// access as every other /v1 route: the tailnet decides who reaches it.

// runnerSupervisor is what the routes ask of internal/runner.
type runnerSupervisor interface {
	Create(cwd, name, model string) (store.RunnerSession, error)
	Send(sessionID, text, sender string) (string, error)
	SetWanted(sessionID, wanted string) error
	SetModel(sessionID, model string) error
	Restart(sessionID string) error
}

// runnerRows is what the routes read.
type runnerRows interface {
	RunnerSessions() ([]store.RunnerSession, error)
	RunnerSession(id string) (*store.RunnerSession, error)
	UndeliveredPrompts(sessionID string) ([]store.InboxPrompt, error)
	OpenSessionCards(sessionID string) ([]store.SessionCard, error)
}

// SetRunners turns the session routes on.
func (s *Server) SetRunners(sup runnerSupervisor, rows runnerRows) {
	s.runners, s.runnerRows = sup, rows
}

func (s *Server) sessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/sessions", s.listRunnerSessions)
	mux.HandleFunc("POST /v1/sessions", s.createRunnerSession)
	mux.HandleFunc("GET /v1/sessions/{id}", s.getRunnerSession)
	mux.HandleFunc("POST /v1/sessions/{id}/prompt", s.promptRunnerSession)
	mux.HandleFunc("POST /v1/sessions/{id}/wanted", s.setRunnerWanted)
	mux.HandleFunc("POST /v1/sessions/{id}/model", s.setRunnerModel)
	mux.HandleFunc("POST /v1/sessions/{id}/restart", s.restartRunnerSession)
}

func (s *Server) runnersReady(w http.ResponseWriter) bool {
	if s.runners == nil || s.runnerRows == nil {
		writeError(w, http.StatusServiceUnavailable, "session runners are off on this host (they need systemd)")
		return false
	}
	return true
}

func (s *Server) listRunnerSessions(w http.ResponseWriter, r *http.Request) {
	if !s.runnersReady(w) {
		return
	}
	list, err := s.runnerRows.RunnerSessions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []store.RunnerSession{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": list})
}

func (s *Server) createRunnerSession(w http.ResponseWriter, r *http.Request) {
	if !s.runnersReady(w) {
		return
	}
	var in struct{ Cwd, Name, Model string }
	if !decodeJSON(w, r, &in) {
		return
	}
	if msg := sessionFolderProblem(in.Cwd); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	sess, err := s.runners.Create(filepath.Clean(in.Cwd), in.Name, in.Model)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

// sessionFolderProblem says why cwd cannot hold a session, or "".
func sessionFolderProblem(cwd string) string {
	if !filepath.IsAbs(cwd) {
		return "cwd must be an absolute folder"
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return "cwd is not a folder on this host: " + cwd
	}
	return ""
}

// getRunnerSession is one session with what still waits on it: prompts
// Claude does not have yet, and open cards.
func (s *Server) getRunnerSession(w http.ResponseWriter, r *http.Request) {
	if !s.runnersReady(w) {
		return
	}
	id := r.PathValue("id")
	sess, err := s.runnerRows.RunnerSession(id)
	if sess == nil {
		writeRunnerError(w, orNoRunner(err))
		return
	}
	waiting, err := s.runnerRows.UndeliveredPrompts(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cards, err := s.runnerRows.OpenSessionCards(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sess, "waiting": len(waiting), "cards": cards})
}

func orNoRunner(err error) error {
	if err != nil {
		return err
	}
	return store.ErrNoSession
}

func writeRunnerError(w http.ResponseWriter, err error) {
	if errors.Is(err, runner.ErrSessionDone) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, store.ErrNoSession) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func (s *Server) promptRunnerSession(w http.ResponseWriter, r *http.Request) {
	if !s.runnersReady(w) {
		return
	}
	var in struct{ Text, Sender string }
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Text == "" {
		writeError(w, http.StatusBadRequest, "empty prompt")
		return
	}
	id, err := s.runners.Send(r.PathValue("id"), in.Text, in.Sender)
	if err != nil {
		writeRunnerError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id})
}

func (s *Server) setRunnerWanted(w http.ResponseWriter, r *http.Request) {
	if !s.runnersReady(w) {
		return
	}
	var in struct{ Wanted string }
	if !decodeJSON(w, r, &in) {
		return
	}
	switch in.Wanted {
	case store.SessionRunning, store.SessionAsleep, store.SessionDone:
	default:
		writeError(w, http.StatusBadRequest, "wanted is running, asleep or done")
		return
	}
	if err := s.runners.SetWanted(r.PathValue("id"), in.Wanted); err != nil {
		writeRunnerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"wanted": in.Wanted})
}

func (s *Server) setRunnerModel(w http.ResponseWriter, r *http.Request) {
	if !s.runnersReady(w) {
		return
	}
	var in struct{ Model string }
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Model == "" {
		writeError(w, http.StatusBadRequest, "no model named")
		return
	}
	if err := s.runners.SetModel(r.PathValue("id"), in.Model); err != nil {
		writeRunnerError(w, err)
		return
	}
	// A model applies when the session next starts (runner.SetModel); the
	// lens says so and offers the restart route as "Restart now".
	id := r.PathValue("id")
	writeJSON(w, http.StatusOK, map[string]string{
		"model": in.Model, "applies": "on the next restart", "restart": "/v1/sessions/" + id + "/restart",
	})
}

// restartRunnerSession is "Restart now": the session starts again on its
// row as it is, so a model just set takes effect.
func (s *Server) restartRunnerSession(w http.ResponseWriter, r *http.Request) {
	if !s.runnersReady(w) {
		return
	}
	if err := s.runners.Restart(r.PathValue("id")); err != nil {
		writeRunnerError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"restarting": r.PathValue("id")})
}
