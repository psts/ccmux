package api

import (
	"net/http"
	"strconv"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// patchPart: PATCH /v1/tasks/{id}/parts/{part} with {plan} before the task
// runs, or {retry: true} on a failed part of a running task.
func (s *Server) patchPart(w http.ResponseWriter, r *http.Request) {
	if !s.tasksReady(w) {
		return
	}
	var req struct {
		Plan  *string `json:"plan"`
		Retry bool    `json:"retry"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	t, _, ok := s.taskOf(w, r)
	if !ok {
		return
	}
	p, ok := s.partOf(w, r, t.ID)
	if !ok {
		return
	}
	if req.Retry {
		s.retryPart(w, t, p)
		return
	}
	next := p
	if req.Plan != nil {
		var err error
		if next, err = taskq.EditPlan(t, p, *req.Plan); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
	}
	if err := s.savePart(next); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.partViewOf(t, next))
}

// retryPart starts failed part p again (taskq.RetryTask): a feature with
// nothing else at work or asking goes back in the queue as Waiting with
// every failed part, so it waits its turn and shows as waiting.
func (s *Server) retryPart(w http.ResponseWriter, t store.Task, p store.Part) {
	all, err := s.tasks.Parts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	next, again, err := taskq.RetryTask(t, taskq.PartsOf(all, t.ID), p)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	view := p
	for _, o := range again {
		if err := s.savePart(o); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if o.ID == p.ID {
			view = o
		}
	}
	if next.Status != t.Status {
		if err := s.saveTask(next); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, s.partViewOf(next, view))
}

// partOf loads {part} of task taskID, or writes the error and returns false.
func (s *Server) partOf(w http.ResponseWriter, r *http.Request, taskID int64) (store.Part, bool) {
	id, err := strconv.ParseInt(r.PathValue("part"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad part id")
		return store.Part{}, false
	}
	parts, err := s.tasks.Parts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return store.Part{}, false
	}
	for _, p := range taskq.PartsOf(parts, taskID) {
		if p.ID == id {
			return p, true
		}
	}
	writeError(w, http.StatusNotFound, "no such part in this task")
	return store.Part{}, false
}

// savePart writes an edited part and tells the lenses.
func (s *Server) savePart(p store.Part) error {
	p.UpdatedAt = s.taskNow()
	if _, err := s.tasks.SavePart(p); err != nil {
		return err
	}
	s.mgr.TasksChanged()
	return nil
}

// sendBackPart: POST /v1/tasks/{id}/parts/{part}/send-back, a person's
// "Send back to worker" on a feature that failed to merge. The runner takes
// the queue's lock itself, so this route does not.
func (s *Server) sendBackPart(w http.ResponseWriter, r *http.Request) {
	if !s.tasksReady(w) {
		return
	}
	if s.taskRunner == nil {
		writeError(w, http.StatusServiceUnavailable, "the task runner is not running on this daemon")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	part, err2 := strconv.ParseInt(r.PathValue("part"), 10, 64)
	if err != nil || err2 != nil {
		writeError(w, http.StatusBadRequest, "bad task or part id")
		return
	}
	if err := s.taskRunner.SendBackPart(id, part); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
