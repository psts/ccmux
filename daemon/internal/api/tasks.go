package api

// The task queue's lens routes (internal/taskq has the rules). A repo's
// session files a task through the peers tool (tasks_pane.go); the lenses
// list them, edit a plan before it runs, reorder, cancel and remove. The
// queue lives on the daemon the lenses talk to, so none of this is proxied.

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// taskStore is the slice of the registry the task queue needs.
type taskStore interface {
	AddTask(store.Task) (int64, error)
	Task(id int64) (*store.Task, error)
	Tasks() ([]store.Task, error)
	SaveTask(store.Task) (bool, error)
	SwapTaskPositions(a, b store.Task) error
	DeleteTask(id int64) (bool, error)
}

// SetTasks wires the task queue's store.
func (s *Server) SetTasks(st taskStore) { s.tasks = st }

func (s *Server) taskNow() int64 { return time.Now().UnixMilli() }

// taskView is a task on the wire: the repo's short name the lenses label it
// with, and what the rules allow on it now, so a lens draws only the buttons
// the daemon will accept (taskq has the rules, nothing else copies them).
type taskView struct {
	store.Task
	Repo      string   `json:"repo"`
	Moves     []string `json:"moves"`
	Editable  bool     `json:"editable"`
	Orderable bool     `json:"orderable"`
	Removable bool     `json:"removable"`
	// WorkspaceID is the session holding the task's worker pane, so a lens
	// can open it; empty once that pane is gone.
	WorkspaceID string `json:"workspaceId,omitempty"`
}

func (s *Server) taskViewsOf(list []store.Task) []taskView {
	out := make([]taskView, 0, len(list))
	for _, t := range list {
		ws := ""
		if t.PaneID != "" {
			ws = s.mgr.WorkspaceForPane(t.PaneID)
		}
		out = append(out, taskView{
			Task: t, Repo: filepath.Base(t.RepoPath), Moves: taskq.Moves(t.Status), WorkspaceID: ws,
			Editable: taskq.Editable(t.Status), Orderable: taskq.Orderable(t.Status),
			Removable: taskq.Removable(t.Status),
		})
	}
	return out
}

// tasksReady answers 503 when this daemon has no task store.
func (s *Server) tasksReady(w http.ResponseWriter) bool {
	if s.tasks == nil {
		writeError(w, http.StatusServiceUnavailable, "the task queue is not available on this daemon")
		return false
	}
	return true
}

// listTasks: GET /v1/tasks → {tasks, counts}.
func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	if !s.tasksReady(w) {
		return
	}
	all, err := s.tasks.Tasks()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": s.taskViewsOf(all), "counts": taskq.Counts(all)})
}

// taskOf loads {id} with the whole list, or writes the error and returns false.
func (s *Server) taskOf(w http.ResponseWriter, r *http.Request) (store.Task, []store.Task, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad task id")
		return store.Task{}, nil, false
	}
	all, err := s.tasks.Tasks()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return store.Task{}, nil, false
	}
	for _, t := range all {
		if t.ID == id {
			return t, all, true
		}
	}
	writeError(w, http.StatusNotFound, "no task #"+r.PathValue("id"))
	return store.Task{}, nil, false
}

// saveTask writes an edited task and tells the lenses.
func (s *Server) saveTask(t store.Task) error {
	t.UpdatedAt = s.taskNow()
	if _, err := s.tasks.SaveTask(t); err != nil {
		return err
	}
	s.mgr.TasksChanged()
	return nil
}

// patchTask: PATCH /v1/tasks/{id} with any of {title, plan, status, afterId}.
func (s *Server) patchTask(w http.ResponseWriter, r *http.Request) {
	if !s.tasksReady(w) {
		return
	}
	var e taskq.Edit
	if !decodeJSON(w, r, &e) {
		return
	}
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	t, all, ok := s.taskOf(w, r)
	if !ok {
		return
	}
	next, err := taskq.Apply(t, all, e)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err := s.saveTask(next); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.taskViewsOf([]store.Task{next})[0])
}

// moveTask: POST /v1/tasks/{id}/move {dir: "up"|"down"} trades places with
// the nearest editable task on that side. Only planning and waiting tasks
// move: the runner's order for the rest is already history.
func (s *Server) moveTask(w http.ResponseWriter, r *http.Request) {
	if !s.tasksReady(w) {
		return
	}
	var req struct {
		Dir string `json:"dir"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Dir != "up" && req.Dir != "down" {
		writeError(w, http.StatusBadRequest, `dir must be "up" or "down"`)
		return
	}
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	t, all, ok := s.taskOf(w, r)
	if !ok {
		return
	}
	if !taskq.Editable(t.Status) {
		writeError(w, http.StatusConflict, "only a planning or waiting task can be moved")
		return
	}
	other, found := taskq.Neighbour(all, t.ID, req.Dir == "up")
	if found {
		if err := s.tasks.SwapTaskPositions(t, other); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.mgr.TasksChanged()
	}
	writeJSON(w, http.StatusOK, map[string]any{"moved": found})
}

// deleteTask: DELETE /v1/tasks/{id}, for a done, failed or cancelled task.
func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request) {
	if !s.tasksReady(w) {
		return
	}
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	t, all, ok := s.taskOf(w, r)
	if !ok {
		return
	}
	if !taskq.Removable(t.Status) {
		writeError(w, http.StatusConflict, "cancel the task first; only a done, failed or cancelled task can be removed")
		return
	}
	if waiters := taskq.Waiters(all, t.ID); len(waiters) > 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("task #%d still starts after this one; change or cancel it first", waiters[0]))
		return
	}
	if _, err := s.tasks.DeleteTask(t.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mgr.TasksChanged()
	w.WriteHeader(http.StatusNoContent)
}
