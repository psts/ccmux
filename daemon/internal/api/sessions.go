package api

import (
	"context"
	"errors"
	"log"
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
//
// A session belongs to a repo workspace and has a pane id there
// (manager/runnerpanes.go), which is what puts it on the bus and behind the
// alerts. What the bus needs of a session is in sessions_bus.go.

// runnerSupervisor is what the routes ask of internal/runner.
type runnerSupervisor interface {
	Create(runner.NewSession) (store.RunnerSession, error)
	Send(sessionID, text, sender string) (string, error)
	SendAs(sessionID, promptID, text, sender string) (string, error)
	SetWanted(sessionID, wanted string) error
	SetModel(sessionID, model string) error
	Restart(sessionID string) error
	Liveness(sessionID string) (string, error)
	ReplyCard(ctx context.Context, sessionID string, r runner.CardReply) (bool, error)
}

// runnerRows is what the routes read.
type runnerRows interface {
	RunnerSessions() ([]store.RunnerSession, error)
	RunnerSession(id string) (*store.RunnerSession, error)
	UndeliveredPrompts(sessionID string) ([]store.InboxPrompt, error)
	OpenSessionCards(sessionID string) ([]store.SessionCard, error)
	SetSessionCardState(sessionID, id, state string, now int64) error
}

// SetRunners turns the session routes on.
func (s *Server) SetRunners(sup runnerSupervisor, rows runnerRows) {
	s.runners, s.runnerRows = sup, rows
	s.wirePeersRunners()
}

// sessionView is a session as the routes serve it: its row (with its pane
// id, its workspace and its pane's attention) and where it is now: running,
// asleep or done, or unknown when the registry or systemd could not be
// asked. The row's own "wanted" is still there to read; what is not known
// is never served as one of the other three.
type sessionView struct {
	store.RunnerSession
	State string `json:"state"`
}

// stateUnknown is a session's state when it could not be read.
const stateUnknown = "unknown"

func (s *Server) viewOf(sess store.RunnerSession) sessionView {
	state, err := s.runners.Liveness(sess.ID)
	if err != nil {
		log.Printf("sessions: state of %s not read, served as %q: %v", sess.ID, stateUnknown, err)
		state = stateUnknown
	}
	return sessionView{RunnerSession: sess, State: state}
}

func (s *Server) sessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/sessions", s.listRunnerSessions)
	mux.HandleFunc("POST /v1/sessions", s.createRunnerSession)
	mux.HandleFunc("GET /v1/sessions/{id}", s.getRunnerSession)
	mux.HandleFunc("POST /v1/sessions/{id}/prompt", s.promptRunnerSession)
	mux.HandleFunc("POST /v1/sessions/{id}/wanted", s.setRunnerWanted)
	mux.HandleFunc("POST /v1/sessions/{id}/model", s.setRunnerModel)
	mux.HandleFunc("POST /v1/sessions/{id}/restart", s.restartRunnerSession)
	mux.HandleFunc("POST /v1/sessions/{id}/acted", s.actedRunnerSession)
	// The thread page (sessionstream.go, sessioncards.go).
	mux.HandleFunc("GET /v1/sessions/{id}/ws", s.streamRunnerSession)
	mux.HandleFunc("GET /v1/sessions/{id}/messages", s.runnerSessionMessages)
	mux.HandleFunc("POST /v1/sessions/{id}/cards/{card}", s.answerRunnerCard)
	mux.HandleFunc("POST /v1/sessions/{id}/abort", s.abortRunnerSession)
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
	views := make([]sessionView, 0, len(list))
	for _, sess := range list {
		views = append(views, s.viewOf(sess))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": views})
}

// createRunnerSession: POST /v1/sessions {cwd, name?, model?, workspaceId?}.
// The session joins a repo workspace: the one named, else the one whose
// folder is cwd or holds it. With neither it is refused (409): a session in
// no workspace would be on no bus and raise no alert, which is the session
// nobody can see that this route exists to stop making. A worktree outside
// its repo's folder names its workspace.
func (s *Server) createRunnerSession(w http.ResponseWriter, r *http.Request) {
	if !s.runnersReady(w) {
		return
	}
	var in struct{ Cwd, Name, Model, WorkspaceID string }
	if !decodeJSON(w, r, &in) {
		return
	}
	if msg := sessionFolderProblem(in.Cwd); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	cwd := filepath.Clean(in.Cwd)
	wsID, status, msg := s.sessionWorkspace(in.WorkspaceID, cwd)
	if msg != "" {
		writeError(w, status, msg)
		return
	}
	sess, err := s.runners.Create(runner.NewSession{Cwd: cwd, Name: in.Name, Model: in.Model, WorkspaceID: wsID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.viewOf(sess))
}

// sessionWorkspace is the workspace a new session joins, or why it has
// none (a status and a message).
func (s *Server) sessionWorkspace(named, cwd string) (wsID string, status int, msg string) {
	if named != "" {
		if ws := s.mgr.Workspace(named); ws == nil || ws.Agent != "" {
			return "", http.StatusBadRequest, "workspaceId is not a repo session on this host: " + named
		}
		return named, 0, ""
	}
	if wsID = s.mgr.RunnerWorkspaceFor(cwd); wsID == "" {
		return "", http.StatusConflict, "no ccmux session holds " + cwd + ": open the repo in ccmux first, or name its session with workspaceId"
	}
	return wsID, 0, ""
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
	writeJSON(w, http.StatusOK, map[string]any{"session": s.viewOf(*sess), "waiting": len(waiting), "cards": cards})
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

// actedRunnerSession: POST /v1/sessions/{id}/acted. A human dealt with what
// the session was waiting to show them, so its claim (done, needs input) is
// retired everywhere. It is a runner session's only "seen": no lens looking
// at its workspace's terminal has seen it (manager.MarkActed).
func (s *Server) actedRunnerSession(w http.ResponseWriter, r *http.Request) {
	if !s.runnersReady(w) {
		return
	}
	sess, err := s.runnerRows.RunnerSession(r.PathValue("id"))
	if sess == nil {
		writeRunnerError(w, orNoRunner(err))
		return
	}
	s.mgr.MarkActed(sess.WorkspaceID, sess.PaneID)
	w.WriteHeader(http.StatusNoContent)
}
