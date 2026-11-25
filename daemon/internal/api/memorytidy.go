package api

// The nightly memory tidy-up's switch per repo (internal/memtidy): on
// unless switched off, from a repo's menu on the thread page, which also
// shows what last night cost. Like the to-do lists it is this daemon's
// own and never proxied. Both routes are gated by the local API token
// (localgate.go): a walled session must not switch it.
//
//	GET /v1/workspaces/{id}/memory-tidy  {on, running, last: {at, result, costUSD, conversations, todos} | null}
//	PUT /v1/workspaces/{id}/memory-tidy  {on: bool} -> the same as GET
//
// running is false on a daemon that runs no tidy-up (a scratch daemon,
// no claude on the host): the switch is kept, and nothing reads it.

import (
	"errors"
	"net/http"

	"ccmux.dev/ccmuxd/internal/memtidy"
	"ccmux.dev/ccmuxd/internal/model"
)

// memoryTidy is what the routes read and write (memtidy.Switches).
type memoryTidy interface {
	State(repo string) (memtidy.Switch, error)
	Set(repo string, on bool) error
	Last(repo string) (*memtidy.Night, error)
}

// SetMemoryTidy wires the switches; running says whether this daemon
// tidies at all.
func (s *Server) SetMemoryTidy(sw memoryTidy, running bool) {
	s.memTidy, s.memTidyRunning = sw, running
}

// AddTodo files an item on a repo's to-do list, as the lenses' route
// does: the folder is keyed to its repo, and the page hears of it.
func (s *Server) AddTodo(repo, text, urgency, by string) error {
	if s.todos == nil {
		return errors.New("the to-do lists are not available on this daemon")
	}
	key, err := s.todoKeys.Of(repo)
	if err != nil {
		return err
	}
	_, _, err = s.todos.Add(key, text, urgency, by)
	return err
}

func (s *Server) memoryTidyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workspaces/{id}/memory-tidy", s.getMemoryTidy)
	mux.HandleFunc("PUT /v1/workspaces/{id}/memory-tidy", s.putMemoryTidy)
}

// memoryTidyRepo is the workspace's repo folder, or an answer already
// written: 503 without switches, 404 for a workspace this daemon does not
// hold, 400 for a role agent's (its memory is in its own home).
func (s *Server) memoryTidyRepo(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.memTidy == nil {
		writeError(w, http.StatusServiceUnavailable, "the nightly memory tidy-up is not wired on this daemon")
		return "", false
	}
	id := r.PathValue("id")
	var ws *model.Workspace
	if s.mgr != nil {
		ws = s.mgr.Workspace(id)
	}
	if ws == nil {
		writeError(w, http.StatusNotFound, "unknown workspace "+id)
		return "", false
	}
	if ws.Agent != "" {
		writeError(w, http.StatusBadRequest, id+" is an agent's session: its memory is not tidied")
		return "", false
	}
	return ws.RepoPath, true
}

func (s *Server) getMemoryTidy(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.memoryTidyRepo(w, r)
	if ok {
		s.writeMemoryTidy(w, repo)
	}
}

func (s *Server) putMemoryTidy(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.memoryTidyRepo(w, r)
	if !ok {
		return
	}
	var in struct {
		On *bool `json:"on"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.On == nil {
		writeError(w, http.StatusBadRequest, `say {"on": true} or {"on": false}`)
		return
	}
	if err := s.memTidy.Set(repo, *in.On); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeMemoryTidy(w, repo)
}

// writeMemoryTidy answers with the switch and last night. A switch that
// cannot be read is an error, never a guess.
func (s *Server) writeMemoryTidy(w http.ResponseWriter, repo string) {
	sw, err := s.memTidy.State(repo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	last, err := s.memTidy.Last(repo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"on": sw.On, "running": s.memTidyRunning, "last": last})
}
