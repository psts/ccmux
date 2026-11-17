package api

import (
	"errors"
	"net/http"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/threads"
)

// Threads with helpers (internal/threads): a thread is one piece of work
// with its own team of runner sessions, a lead and a helper per other repo.
// These routes are how the thread page starts and reads one; a member's own
// calls come in on its pane (threads_pane.go). Same access as every other
// /v1 route.

// threadService is what the routes ask of internal/threads.
type threadService interface {
	Create(threads.New) (store.Thread, store.RunnerSession, error)
	FirstEdit(sessionID string) (threads.Moved, error)
}

// SetThreads turns the thread routes on.
func (s *Server) SetThreads(t threadService) { s.threads = t }

func (s *Server) threadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/threads", s.createThread)
	mux.HandleFunc("POST /v1/panes/{id}/first-edit", s.paneFirstEdit)
}

func (s *Server) threadsReady(w http.ResponseWriter) bool {
	if !s.runnersReady(w) {
		return false
	}
	if s.threads == nil {
		writeError(w, http.StatusServiceUnavailable, "threads are off on this host")
		return false
	}
	return true
}

// createThread: POST /v1/threads {workspaceId, title?, model?} starts a
// thread led from that repo: 201 with the thread and its lead session, which
// starts in the repo's own folder, reading only. Its first message is a
// prompt to that session, like any other.
func (s *Server) createThread(w http.ResponseWriter, r *http.Request) {
	if !s.threadsReady(w) {
		return
	}
	var in struct{ WorkspaceID, Title, Model string }
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.WorkspaceID == "" {
		writeError(w, http.StatusBadRequest, "name the repo session that leads the thread (workspaceId)")
		return
	}
	th, lead, err := s.threads.Create(threads.New{WorkspaceID: in.WorkspaceID, Title: sessionTitle(in.Title), Model: in.Model})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"thread": th, "session": s.viewOf(lead)})
}

// paneFirstEdit: POST /v1/panes/{id}/first-edit. A thread member's runner
// asks this from the tool call it just refused (claudeserve/readonly.mjs):
// the member is about to change something, so it gets its own worktree. The
// answer is what the refused call is told, and what the member is told when
// it starts again there. Loopback only, like agent-signal: the runner is on
// this host with the pane's own environment.
func (s *Server) paneFirstEdit(w http.ResponseWriter, r *http.Request) {
	if !requireLoopback(w, r) || !s.threadsReady(w) {
		return
	}
	info, ok := s.mgr.RunnerPane(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown pane")
		return
	}
	moved, err := s.threads.FirstEdit(info.SessionID)
	if errors.Is(err, threads.ErrNotMember) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, moved)
}
