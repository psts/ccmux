package api

import (
	"errors"
	"net/http"
	"strconv"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
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
	List() ([]store.Thread, error)
	Detail(threadID int64) (threads.Detail, error)
	Merge(threadID int64, by string) (string, error)
	StopMerge(threadID int64, by string) (string, error)
	End(threadID int64, by string) (string, error)
	StandDown(threadID int64, repo, by string) (string, error)
	Tool(sessionID, action, repo string) (string, error)
	LeadChanged(sessionID string)
	// StartBuilder starts one part of a queued task as a member of the
	// task's thread; the task queue calls it (tasks_runner.go).
	StartBuilder(taskq.Builder) (string, error)
}

// SetThreads turns the thread routes on.
func (s *Server) SetThreads(t threadService) { s.threads = t }

func (s *Server) threadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/threads", s.createThread)
	mux.HandleFunc("GET /v1/threads", s.listThreads)
	mux.HandleFunc("GET /v1/threads/{id}", s.getThread)
	mux.HandleFunc("POST /v1/threads/{id}/merge", s.threadAction(func(t threadService, id int64, _ string) (string, error) {
		return t.Merge(id, threads.ByPerson)
	}))
	// A merge runs as a job in its own service (internal/jobs); this stops
	// the one that is running. 409 with the reason when there is none.
	mux.HandleFunc("POST /v1/threads/{id}/stop-merge", s.threadAction(func(t threadService, id int64, _ string) (string, error) {
		return t.StopMerge(id, threads.ByPerson)
	}))
	mux.HandleFunc("POST /v1/threads/{id}/end", s.threadAction(func(t threadService, id int64, _ string) (string, error) {
		return t.End(id, threads.ByPerson)
	}))
	mux.HandleFunc("POST /v1/threads/{id}/stand-down", s.threadAction(func(t threadService, id int64, repo string) (string, error) {
		return t.StandDown(id, repo, threads.ByPerson)
	}))
	mux.HandleFunc("POST /v1/panes/{id}/first-edit", s.paneFirstEdit)
	mux.HandleFunc("POST /v1/panes/{id}/thread", s.paneThread)
}

// threadLeadChanged tells the threads service a session's title, goal or
// model changed: a thread follows its lead's (threads.LeadChanged).
func (s *Server) threadLeadChanged(sessionID string) {
	if s.threads != nil {
		s.threads.LeadChanged(sessionID)
	}
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
	var in struct{ WorkspaceID, Title, Model, Advisor string }
	if !decodeJSON(w, r, &in) || !advisorTaken(w, in.Advisor) {
		return
	}
	if in.WorkspaceID == "" {
		writeError(w, http.StatusBadRequest, "name the repo session that leads the thread (workspaceId)")
		return
	}
	th, lead, err := s.threads.Create(threads.New{WorkspaceID: in.WorkspaceID, Title: sessionTitle(in.Title), Model: in.Model, Advisor: in.Advisor})
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

// listThreads: GET /v1/threads is every thread whose lead has not ended.
// Its members are sessions (GET /v1/sessions, each with its threadId and
// role), so the list is the rows alone.
func (s *Server) listThreads(w http.ResponseWriter, r *http.Request) {
	if !s.threadsReady(w) {
		return
	}
	list, err := s.threads.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []store.Thread{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": list})
}

// threadID reads the route's thread number, answering 400 itself when it
// is not one.
func threadID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "a thread is named by its number")
		return 0, false
	}
	return id, true
}

// getThread: GET /v1/threads/{id} is one thread with its members, each with
// what its branch holds against its repo's trunk, and its merge job while
// one runs or the last one did not land (threads.MergeView).
func (s *Server) getThread(w http.ResponseWriter, r *http.Request) {
	if !s.threadsReady(w) {
		return
	}
	id, ok := threadID(w, r)
	if !ok {
		return
	}
	d, err := s.threads.Detail(id)
	if err != nil {
		writeThreadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// writeThreadError answers a thread action that was refused: 404 for a
// thread that is not there, 409 with the reason for anything else (a merge
// that cannot start, a helper that is not there): the reason is the answer.
func writeThreadError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNoThread) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeError(w, http.StatusConflict, err.Error())
}

// threadAction is a route a person's page calls on a thread: merge, stop
// its merge, end, or stand one helper down ({repo}). 200 with what was done, in words. The
// thread's lead is told a person did it (threads.ByPerson).
func (s *Server) threadAction(run func(t threadService, id int64, repo string) (string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.threadsReady(w) {
			return
		}
		id, ok := threadID(w, r)
		if !ok {
			return
		}
		var in struct{ Repo string }
		if r.ContentLength != 0 && !decodeJSON(w, r, &in) {
			return
		}
		said, err := run(s.threads, id, in.Repo)
		if err != nil {
			writeThreadError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": said})
	}
}

// paneThread: POST /v1/panes/{id}/thread {action, repo?} is the shim's
// thread tool, called by the session in that pane about its own thread.
// Which thread, and whether the caller leads it, come from the pane: a
// session cannot act on a thread it is not in. Same access as the tasks
// tool's route beside it.
func (s *Server) paneThread(w http.ResponseWriter, r *http.Request) {
	if !s.threadsReady(w) {
		return
	}
	var in struct{ Action, Repo string }
	if !decodeJSON(w, r, &in) {
		return
	}
	info, ok := s.mgr.RunnerPane(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusConflict, threads.ErrNotMember.Error())
		return
	}
	said, err := s.threads.Tool(info.SessionID, in.Action, in.Repo)
	if err != nil {
		writeThreadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": said})
}
