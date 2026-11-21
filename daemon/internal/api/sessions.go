package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/loadout"
	"ccmux.dev/ccmuxd/internal/model"
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
	SendPrompt(sessionID string, p runner.Prompt) (string, error)
	MediaDir(sessionID string) string
	SetWanted(sessionID, wanted string) error
	SetModel(sessionID, model string) error
	SetAdvisor(sessionID, advisor string) error
	Restart(sessionID string) error
	StartOver(sessionID string) error
	RestartBetweenTurns(sessionID string) error
	Liveness(sessionID string) (string, error)
	Sleep(sessionID string, fresh bool) error
	SetFresh(sessionID string, fresh bool) error
	ReplyCard(ctx context.Context, sessionID string, r runner.CardReply) (bool, error)
	Rewind(ctx context.Context, sessionID, to string, dry bool) (runner.Rewind, error)
}

// runnerRows is what the routes read.
type runnerRows interface {
	RunnerSessions() ([]store.RunnerSession, error)
	RunnerSession(id string) (*store.RunnerSession, error)
	UndeliveredPrompts(sessionID string) ([]store.InboxPrompt, error)
	OpenSessionCards(sessionID string) ([]store.SessionCard, error)
	SetSessionCardState(sessionID, id, state string, now int64) error
	RunnerLead(workspaceID string) (*store.RunnerSession, error)
	AgentSession(workspaceID string) (*store.RunnerSession, error)
	SetRunnerSessionTitle(id, title string, now int64) error
	SetRunnerSessionGoal(id, goal string, now int64) error
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
//
// Loadout is what the session loaded from the capability catalog at its
// last start (internal/loadout): the entries, and notes on what was skipped
// and why. LoadoutStale says a running session's list has moved on since
// (its group's or repo's choices, or a version of something it loaded), so
// the page can offer a restart; nothing restarts by itself. A running
// session with no loadout started before loadouts did and is stale too.
type sessionView struct {
	store.RunnerSession
	State        string               `json:"state"`
	Loadout      *agent.LoadoutRecord `json:"loadout,omitempty"`
	LoadoutStale bool                 `json:"loadoutStale,omitempty"`
}

// stateUnknown is a session's state when it could not be read.
const stateUnknown = "unknown"

func (s *Server) viewOf(sess store.RunnerSession) sessionView {
	return s.viewWith(s.sessionLoadouts(), sess)
}

// viewWith is viewOf against choices already read (sessionLoadouts), for a
// list of sessions.
func (s *Server) viewWith(snap *loadout.Snapshot, sess store.RunnerSession) sessionView {
	rec, stale := loadoutOf(snap, sess)
	return sessionView{RunnerSession: sess, State: s.stateOf(sess.ID), Loadout: rec, LoadoutStale: stale}
}

// stateOf is where a session is now: running, asleep or done, or unknown
// when that could not be asked.
func (s *Server) stateOf(sessionID string) string {
	state, err := s.runners.Liveness(sessionID)
	if err != nil {
		log.Printf("sessions: state of %s not read, served as %q: %v", sessionID, stateUnknown, err)
		return stateUnknown
	}
	return state
}

func (s *Server) sessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/sessions", s.listRunnerSessions)
	mux.HandleFunc("POST /v1/sessions", s.createRunnerSession)
	mux.HandleFunc("GET /v1/sessions/{id}", s.getRunnerSession)
	mux.HandleFunc("POST /v1/sessions/{id}/prompt", s.promptRunnerSession)
	mux.HandleFunc("POST /v1/sessions/{id}/wanted", s.setRunnerWanted)
	mux.HandleFunc("POST /v1/sessions/{id}/model", s.setRunnerModel)
	mux.HandleFunc("POST /v1/sessions/{id}/advisor", s.setRunnerAdvisor)
	mux.HandleFunc("POST /v1/sessions/{id}/restart", s.restartRunnerSession)
	mux.HandleFunc("POST /v1/sessions/{id}/start-over", s.startOverRunnerSession)
	mux.HandleFunc("POST /v1/sessions/{id}/acted", s.actedRunnerSession)
	mux.HandleFunc("POST /v1/sessions/{id}/title", s.setRunnerTitle)
	// The thread page (sessionstream.go, sessioncards.go).
	mux.HandleFunc("GET /v1/sessions/{id}/ws", s.streamRunnerSession)
	mux.HandleFunc("GET /v1/sessions/{id}/messages", s.runnerSessionMessages)
	mux.HandleFunc("GET /v1/sessions/{id}/conversations", s.listPastConversations)
	mux.HandleFunc("GET /v1/sessions/{id}/conversations/{conv}", s.pastConversation)
	mux.HandleFunc("POST /v1/sessions/{id}/cards/{card}", s.answerRunnerCard)
	mux.HandleFunc("POST /v1/sessions/{id}/abort", s.abortRunnerSession)
	mux.HandleFunc("POST /v1/sessions/{id}/rewind", s.rewindRunnerSession)
	mux.HandleFunc("POST /v1/sessions/{id}/mode", s.setRunnerMode)
	mux.HandleFunc("POST /v1/sessions/{id}/tasks/{task}/stop", s.stopRunnerTask)
	mux.HandleFunc("GET /v1/sessions/{id}/files", s.sessionFiles)
	mux.HandleFunc("POST /v1/sessions/{id}/images", s.uploadRunnerImage)
	mux.HandleFunc("GET /v1/sessions/{id}/media/{name}", s.runnerMedia)
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
	snap := s.sessionLoadouts()
	for _, sess := range list {
		views = append(views, s.viewWith(snap, sess))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": views})
}

// createRunnerSession: POST /v1/sessions {cwd, name?, model?, workspaceId?,
// kind?, title?}. The session joins a repo workspace: the one named, else
// the one whose folder is cwd or holds it. With neither it is refused
// (409): a session in no workspace would be on no bus and raise no alert,
// which is the session nobody can see that this route exists to stop
// making. A worktree outside its repo's folder names its workspace.
//
// kind "lead" asks for the repo's own session, of which a workspace has
// one: when it is there already (whoever made it, a moment ago or last
// week) the answer is that session with 200, never a second lead. Anything
// else is a thread, made new each time (201).
func (s *Server) createRunnerSession(w http.ResponseWriter, r *http.Request) {
	if !s.runnersReady(w) {
		return
	}
	var in struct{ Cwd, Name, Model, WorkspaceID, Kind, Title, Advisor string }
	if !decodeJSON(w, r, &in) {
		return
	}
	if msg := newSessionProblem(in.Cwd, in.Kind, in.Advisor); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	cwd := filepath.Clean(in.Cwd)
	wsID, status, msg := s.sessionWorkspace(in.WorkspaceID, cwd)
	if msg != "" {
		writeError(w, status, msg)
		return
	}
	if in.Kind == store.SessionLead && s.answerLead(w, wsID) {
		return
	}
	sess, err := s.runners.Create(runner.NewSession{Cwd: cwd, Name: in.Name, Model: in.Model, WorkspaceID: wsID, Kind: in.Kind, Title: sessionTitle(in.Title), Advisor: in.Advisor})
	if errors.Is(err, store.ErrLeadExists) && s.answerLead(w, wsID) {
		return // another window made the lead between the look and the insert
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.viewOf(sess))
}

// newSessionProblem says why a session cannot be made as asked, or "".
func newSessionProblem(cwd, kind, advisor string) string {
	if kind != "" && kind != store.SessionLead {
		return `kind is "lead" (the repo's own session) or left out (a thread)`
	}
	if !slices.Contains(advisors, advisor) {
		return "not an advisor to pick: " + advisor
	}
	return sessionFolderProblem(cwd)
}

// answerLead answers with the workspace's lead when it has one, and says
// whether it did.
func (s *Server) answerLead(w http.ResponseWriter, wsID string) bool {
	lead := s.leadOf(wsID)
	if lead == nil {
		return false
	}
	writeJSON(w, http.StatusOK, s.viewOf(*lead))
	return true
}

// leadOf is a workspace's lead session that has not ended, or nil. A row
// that could not be read is logged and reads as none: the caller then
// makes one, and the store refuses a second lead (store.ErrLeadExists).
func (s *Server) leadOf(wsID string) *store.RunnerSession {
	lead, err := s.runnerRows.RunnerLead(wsID)
	if err != nil {
		log.Printf("sessions: lead of workspace %s not read: %v", wsID, err)
	}
	return lead
}

// ensureLead is a repo session's lead, made (on model, "" for the default)
// when the workspace has none: what a page's own first message to a repo
// does, for a daemon route that has something to say to the repo's session.
func (s *Server) ensureLead(ws *model.Workspace, modelName string) (store.RunnerSession, error) {
	if lead := s.leadOf(ws.ID); lead != nil {
		return *lead, nil
	}
	sess, err := s.runners.Create(runner.NewSession{Cwd: ws.RepoPath, Model: modelName, WorkspaceID: ws.ID, Kind: store.SessionLead})
	if errors.Is(err, store.ErrLeadExists) {
		if lead := s.leadOf(ws.ID); lead != nil {
			return *lead, nil // another page made it between the look and the insert
		}
	}
	return sess, err
}

// maxSessionTitle bounds a title: it is one line in a sidebar row.
const maxSessionTitle = 60

// sessionTitle is a title as it is kept: its first line, trimmed and cut.
func sessionTitle(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > maxSessionTitle {
		return strings.TrimSpace(string(r[:maxSessionTitle-1])) + "…"
	}
	return line
}

// setRunnerTitle: POST /v1/sessions/{id}/title {title} names a session.
func (s *Server) setRunnerTitle(w http.ResponseWriter, r *http.Request) {
	if !s.runnersReady(w) {
		return
	}
	var in struct{ Title string }
	if !decodeJSON(w, r, &in) {
		return
	}
	id, title := r.PathValue("id"), sessionTitle(in.Title)
	if err := s.runnerRows.SetRunnerSessionTitle(id, title, time.Now().UnixMilli()); err != nil {
		writeRunnerError(w, err)
		return
	}
	s.sessionChanged(id)
	writeJSON(w, http.StatusOK, map[string]string{"title": title})
}

// titleFromFirstPrompt names a thread that has no title yet after what it
// was first asked. A lead is named after its repo by the page, and a thread
// someone named keeps its name.
func (s *Server) titleFromFirstPrompt(sessionID, text string) {
	title := sessionTitle(text)
	sess, err := s.runnerRows.RunnerSession(sessionID)
	if err != nil || sess == nil || sess.Kind == store.SessionLead || sess.Title != "" || title == "" {
		return
	}
	if err := s.runnerRows.SetRunnerSessionTitle(sessionID, title, time.Now().UnixMilli()); err != nil {
		log.Printf("sessions: %s: its title was not kept: %v", sessionID, err)
		return
	}
	s.sessionChanged(sessionID)
}

// sessionChanged tells every lens a session's row changed in a way its page
// shows (its title, its goal), so they read the list again.
func (s *Server) sessionChanged(sessionID string) {
	if s.mgr == nil {
		return
	}
	sess, err := s.runnerRows.RunnerSession(sessionID)
	if err != nil || sess == nil {
		return
	}
	s.threadLeadChanged(sessionID)
	s.mgr.SessionsChanged(sess.WorkspaceID)
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
	var capped errAgentCap
	if errors.Is(err, runner.ErrSessionDone) || errors.Is(err, runner.ErrPromptWaiting) || errors.Is(err, runner.ErrCleanRefused) || errors.As(err, &capped) {
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
	var in struct {
		Text, Sender string
		// Images are names POST .../images gave: pictures that go to Claude
		// with the text. A picture with no words is still a prompt.
		Images []string
		// Clean asks for a new conversation first (runner/cleanstart.go):
		// the message's own key, the same when the page sends it again.
		Clean string
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Text == "" && len(in.Images) == 0 {
		writeError(w, http.StatusBadRequest, "empty prompt")
		return
	}
	sessionID := r.PathValue("id")
	if s.agentWakeRefused(w, sessionID) {
		return
	}
	if len(in.Images) == 0 && in.Clean == "" {
		id, err := s.runners.Send(sessionID, in.Text, in.Sender)
		s.answerPrompt(w, sessionID, in.Text, id, err)
		return
	}
	if msg := s.promptImagesProblem(sessionID, in.Images); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	id, err := s.runners.SendPrompt(sessionID, runner.Prompt{Text: in.Text, Sender: in.Sender, Images: in.Images, Clean: in.Clean})
	s.answerPrompt(w, sessionID, in.Text, id, err)
}

// startOverRunnerSession: POST /v1/sessions/{id}/start-over gives the
// session a new conversation with no message (runner.StartOver); a main
// session's side helpers are stood down by the runner's CleanStarted. 409
// while Claude is in a turn, and for an agent.
func (s *Server) startOverRunnerSession(w http.ResponseWriter, r *http.Request) {
	if !s.runnersReady(w) {
		return
	}
	id := r.PathValue("id")
	if err := s.runners.StartOver(id); err != nil {
		writeRunnerError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"startedOver": id})
}

func (s *Server) answerPrompt(w http.ResponseWriter, sessionID, text, id string, err error) {
	if err != nil {
		writeRunnerError(w, err)
		return
	}
	s.titleFromFirstPrompt(sessionID, text)
	s.personTyped(sessionID)
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id})
}

// personTyped tells the task queue's watchdog that a person sent this
// session a prompt, when the session is a task's builder (the manager
// keeps the mark for no other pane). A tmux worker's own hook says so; a
// builder's is not heard (manager.ApplyAttention), and its runner's "busy"
// a second later looks the same for a person's prompt and a teammate's bus
// message. The mark is what puts a part that reported "stuck" back to work
// once a person has answered it in its session.
func (s *Server) personTyped(sessionID string) {
	if s.mgr == nil {
		return
	}
	sess, err := s.runnerRows.RunnerSession(sessionID)
	if err != nil || sess == nil || sess.PaneID == "" {
		return
	}
	s.mgr.NoteTaskPrompt(sess.PaneID)
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
	handled, err := s.setAgentWanted(r.PathValue("id"), in.Wanted)
	if !handled {
		err = s.runners.SetWanted(r.PathValue("id"), in.Wanted)
	}
	if err != nil {
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
	if sess, _ := s.runnerRows.RunnerSession(r.PathValue("id")); sess != nil && sess.Kind == store.SessionAgent {
		writeError(w, http.StatusConflict, "an agent runs on the model its base names: change it in the agent editor")
		return
	}
	if err := s.runners.SetModel(r.PathValue("id"), in.Model); err != nil {
		writeRunnerError(w, err)
		return
	}
	// A model applies when the session next starts (runner.SetModel); the
	// lens says so and offers the restart route as "Restart now". A thread's
	// helpers follow their lead's model, at their own next start.
	id := r.PathValue("id")
	s.threadLeadChanged(id)
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
	if s.agentWakeRefused(w, r.PathValue("id")) {
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
