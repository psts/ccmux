package api

// The task queue's lens routes (internal/taskq has the rules). A task is a
// feature with one part per repo; repo sessions file and add parts through
// the peers tool (tasks_pane.go). The lenses list them, edit before they
// run, reorder, cancel, retry a failed part and remove. The queue lives on
// the daemon the lenses talk to, so none of this is proxied.

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
	AddTaskWithPart(store.Task, store.Part) (int64, error)
	Task(id int64) (*store.Task, error)
	Tasks() ([]store.Task, error)
	SaveTask(store.Task) (bool, error)
	SwapTaskPositions(a, b store.Task) error
	DeleteTask(id int64) (bool, error)
	AddPart(store.Part) (int64, error)
	Parts() ([]store.Part, error)
	SavePart(store.Part) (bool, error)
}

// SetTasks wires the task queue's store.
func (s *Server) SetTasks(st taskStore) { s.tasks = st }

func (s *Server) taskNow() int64 { return time.Now().UnixMilli() }

// taskView is a task on the wire with its parts, and what the rules allow
// on it now, so a lens draws only the buttons the daemon will accept (taskq
// has the rules, nothing else copies them).
type taskView struct {
	store.Task
	Parts     []partView `json:"parts"`
	Moves     []string   `json:"moves"`
	Editable  bool       `json:"editable"`
	Orderable bool       `json:"orderable"`
	Removable bool       `json:"removable"`
}

// partView is a part with its repo's short name, its Claude's bus name, and
// the session holding its pane (empty once that pane is gone), so a lens
// can open it.
type partView struct {
	store.Part
	Repo        string `json:"repo"`
	BusName     string `json:"busName"`
	WorkspaceID string `json:"workspaceId,omitempty"`
	Retryable   bool   `json:"retryable"`
}

func (s *Server) taskViewsOf(tasks []store.Task, parts []store.Part) []taskView {
	out := make([]taskView, 0, len(tasks))
	for _, t := range tasks {
		v := taskView{
			Task: t, Parts: []partView{}, Moves: taskq.Moves(t.Status),
			Editable: taskq.Editable(t.Status), Orderable: taskq.Orderable(t.Status),
			Removable: taskq.Removable(t.Status),
		}
		for _, p := range taskq.PartsOf(parts, t.ID) {
			v.Parts = append(v.Parts, s.partViewOf(t, p))
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) partViewOf(t store.Task, p store.Part) partView {
	v := partView{Part: p, Repo: filepath.Base(p.RepoPath), BusName: taskq.BusName(p.RepoPath, t.ID)}
	if p.PaneID != "" {
		v.WorkspaceID = s.mgr.WorkspaceForPane(p.PaneID)
	}
	_, err := taskq.Retry(t, p)
	v.Retryable = err == nil
	return v
}

// board reads every task and part.
func (s *Server) board() ([]store.Task, []store.Part, error) {
	tasks, err := s.tasks.Tasks()
	if err != nil {
		return nil, nil, err
	}
	parts, err := s.tasks.Parts()
	return tasks, parts, err
}

// tasksReady answers 503 when this daemon has no task store.
func (s *Server) tasksReady(w http.ResponseWriter) bool {
	if s.tasks == nil {
		writeError(w, http.StatusServiceUnavailable, "the task queue is not available on this daemon")
		return false
	}
	return true
}

// listTasks: GET /v1/tasks → {tasks, counts}: every window's tasks, each
// stamped with its windowId, and counts keyed by window id, so a lens draws
// each window's own queue row.
func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	if !s.tasksReady(w) {
		return
	}
	tasks, parts, err := s.board()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	counts := map[string]map[string]int{}
	for _, t := range tasks {
		if _, done := counts[t.WindowID]; !done {
			counts[t.WindowID] = taskq.Counts(taskq.InWindow(tasks, t.WindowID))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": s.taskViewsOf(tasks, parts), "counts": counts})
}

// taskOf loads {id} with its window's queue, which its order, "starts
// after" and waiters are all within; or writes the error and returns false.
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
			return t, taskq.InWindow(all, t.WindowID), true
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

// patchTask: PATCH /v1/tasks/{id} with any of {title, status, afterId}. A
// cancelled task queued again has its unbuilt parts set to start again.
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
	if err := s.saveTaskChange(t, next); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": next.ID, "status": next.Status})
}

// saveTaskChange saves an edited task with what its status change means
// for its parts: a cancel, or a merge marked by hand, closes every one of
// its Claudes now, idle ones that reported done too (not on the runner's
// next pass, which a quick Remove or requeue would beat, leaving them
// running untracked); a cancelled task queued again gets its unbuilt parts
// set to start again. Worktrees and branches stay for the person.
func (s *Server) saveTaskChange(was, next store.Task) error {
	parts, err := s.tasks.Parts()
	if err != nil {
		return err
	}
	var changed []store.Part
	switch {
	case (next.Status == taskq.Cancelled || next.Status == taskq.Done) && was.Status != next.Status:
		changed, err = s.stopWorkers(taskq.PartsOf(parts, next.ID))
	case was.Status == taskq.Cancelled && next.Status == taskq.Waiting:
		changed = taskq.Requeued(taskq.PartsOf(parts, next.ID))
	}
	if err != nil {
		return err
	}
	for _, p := range changed {
		p.UpdatedAt = s.taskNow()
		if _, err := s.tasks.SavePart(p); err != nil {
			return err
		}
	}
	return s.saveTask(next)
}

// stopWorkers closes every part's Claude, and returns the parts that were
// still at work, stopped, to start again if the task is queued again.
func (s *Server) stopWorkers(parts []store.Part) ([]store.Part, error) {
	var out []store.Part
	for _, p := range parts {
		if p.Worktree != "" {
			if err := s.mgr.ClearTaskFolder(p.Worktree); err != nil {
				return nil, fmt.Errorf("closing the %s worker: %w", filepath.Base(p.RepoPath), err)
			}
		}
		if p.Status == taskq.Running || p.Status == taskq.NeedsYou {
			p.Status, p.Note, p.PaneID = taskq.Pending, "stopped: the task was cancelled", ""
			out = append(out, p)
		}
	}
	return out, nil
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

// deleteTask: DELETE /v1/tasks/{id}, for a done or cancelled task.
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
		writeError(w, http.StatusConflict, "cancel the task first; only a done or cancelled task can be removed")
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
