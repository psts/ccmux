package api

// The peers shim's `tasks` tool. A repo's session files a feature with its
// own part, adds its part to a feature another repo filed, lists, edits or
// cancels the features it has a part in. A worker building a part reports
// on it, and pulls in another repo by adding a part (tasks_worker.go). The
// repo is always the pane's own session's repo, never an argument, so a
// session only writes plans for the repo it knows.

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

type paneTaskReq struct {
	Action string `json:"action"`
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Plan   string `json:"plan"`
	// After is "starts after" this task number; on an edit, nil leaves it
	// alone and 0 clears it.
	After *int64 `json:"after"`
	// Draft files as planning instead of waiting; on an edit, nil leaves the
	// status alone and a value moves the task to planning or waiting.
	Draft *bool `json:"draft"`
	// Repo names the repo (its folder name) a worker adds a part for.
	Repo string `json:"repo"`
	// Status and Note are a worker's report on its own part.
	Status string `json:"status"`
	Note   string `json:"note"`
}

// taskFiler is who is asking: the repo, the session name stamped on what
// it files, and the pane (a worker is known by its pane).
type taskFiler struct{ repo, session, pane string }

// paneTasks: POST /v1/panes/{id}/tasks {action, ...} → {message, tasks}.
func (s *Server) paneTasks(w http.ResponseWriter, r *http.Request) {
	if !s.tasksReady(w) {
		return
	}
	who, status, msg := s.taskFilerOfPane(r.PathValue("id"))
	if msg != "" {
		writeError(w, status, msg)
		return
	}
	var req paneTaskReq
	if !decodeJSON(w, r, &req) {
		return
	}
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	message, status, err := s.applyPaneTask(who, req)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	tasks, parts, err := s.board()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	mine := repoTasks(tasks, parts, who.repo)
	writeJSON(w, http.StatusOK, map[string]any{"message": message, "tasks": s.taskViewsOf(mine, parts)})
}

// taskFilerOfPane names the repo a pane's session works in. A session on
// another host is unknown here: the queue runs only this daemon's repos.
func (s *Server) taskFilerOfPane(paneID string) (taskFiler, int, string) {
	ws := s.mgr.Workspace(s.mgr.WorkspaceForPane(paneID))
	if ws == nil {
		return taskFiler{}, http.StatusNotFound, "unknown pane: the task queue only takes tasks from sessions hosted on this daemon"
	}
	if _, err := os.Stat(filepath.Join(ws.RepoPath, ".git")); err != nil {
		return taskFiler{}, http.StatusBadRequest, "this session is not in a git repo, so there is nothing to build a task in"
	}
	return taskFiler{repo: ws.RepoPath, session: ws.Name, pane: paneID}, 0, ""
}

// repoTasks is the tasks with a part in repo.
func repoTasks(tasks []store.Task, parts []store.Part, repo string) []store.Task {
	in := map[int64]bool{}
	for _, p := range parts {
		if p.RepoPath == repo {
			in[p.TaskID] = true
		}
	}
	out := []store.Task{}
	for _, t := range tasks {
		if in[t.ID] {
			out = append(out, t)
		}
	}
	return out
}

func (s *Server) applyPaneTask(who taskFiler, req paneTaskReq) (string, int, error) {
	switch req.Action {
	case "file":
		return s.fileTask(who, req)
	case "add_part":
		return s.addPart(who, req)
	case "list":
		return "", 0, nil
	case "edit":
		return s.editPaneTask(who, req)
	case "cancel":
		cancel := taskq.Cancelled
		return s.changeMemberTask(who, req.ID, taskq.Edit{Status: &cancel}, "Cancelled")
	case "report":
		return s.reportPart(who, req)
	}
	return "", http.StatusBadRequest, errors.New("action must be one of file, add_part, list, edit, cancel, report")
}

// fileTask files a feature with the caller's repo's part.
func (s *Server) fileTask(who taskFiler, req paneTaskReq) (string, int, error) {
	if err := firstErr(taskq.CheckTitle(req.Title), taskq.CheckPlan(req.Plan)); err != nil {
		return "", http.StatusBadRequest, err
	}
	all, err := s.tasks.Tasks()
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	after := deref(req.After)
	if err := taskq.CheckAfter(all, 0, after); err != nil {
		return "", http.StatusBadRequest, err
	}
	status := taskq.Waiting
	if deref(req.Draft) {
		status = taskq.Planning
	}
	now := s.taskNow()
	id, err := s.tasks.AddTaskWithPart(
		store.Task{Title: strings.TrimSpace(req.Title), Status: status, AfterID: after, FiledBy: who.session, CreatedAt: now, UpdatedAt: now},
		store.Part{RepoPath: who.repo, Plan: req.Plan, Status: taskq.Pending, FiledBy: who.session, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	s.mgr.TasksChanged()
	return fileMessage(id, status, after), 0, nil
}

func fileMessage(id int64, status string, after int64) string {
	msg := fmt.Sprintf("Filed task #%d with this repo's part", id)
	if status == taskq.Planning {
		msg += fmt.Sprintf(" as a draft. If other repos have parts, give them the number #%d over the bus so each adds its own part (action add_part); then mark it ready (edit, draft false)", id)
	} else {
		msg += ": it waits in the queue for a free slot"
	}
	if after != 0 {
		msg += fmt.Sprintf(", and will not start before task #%d is done and merged", after)
	}
	return msg + "."
}

// editPaneTask changes a task this repo has a part in: its title, what it
// starts after, draft or ready, and this repo's own plan. Everything is
// checked before anything is saved, so a refused edit changes nothing.
func (s *Server) editPaneTask(who taskFiler, req paneTaskReq) (string, int, error) {
	t, all, status, err := s.memberTask(who, req.ID)
	if err != nil {
		return "", status, err
	}
	next, err := taskq.Apply(t, all, taskEditOf(req))
	if err != nil {
		return "", http.StatusConflict, err
	}
	part, status, err := s.planEditOf(who, t, req.Plan)
	if err != nil {
		return "", status, err
	}
	if part != nil {
		if err := s.savePart(*part); err != nil {
			return "", http.StatusInternalServerError, err
		}
	}
	if err := s.saveTaskChange(t, next); err != nil {
		return "", http.StatusInternalServerError, err
	}
	return fmt.Sprintf("Updated task #%d (%s).", t.ID, next.Status), 0, nil
}

// taskEditOf is the task-level part of an edit request.
func taskEditOf(req paneTaskReq) taskq.Edit {
	e := taskq.Edit{AfterID: req.After}
	if req.Title != "" {
		e.Title = &req.Title
	}
	if req.Draft != nil {
		status := taskq.Waiting
		if *req.Draft {
			status = taskq.Planning
		}
		e.Status = &status
	}
	return e
}

// planEditOf checks a new plan for this repo's part of task t; nil when the
// edit carries no plan.
func (s *Server) planEditOf(who taskFiler, t store.Task, plan string) (*store.Part, int, error) {
	if plan == "" {
		return nil, 0, nil
	}
	parts, err := s.tasks.Parts()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	for _, p := range taskq.PartsOf(parts, t.ID) {
		if p.RepoPath != who.repo {
			continue
		}
		next, err := taskq.EditPlan(t, p, plan)
		if err != nil {
			return nil, http.StatusConflict, err
		}
		return &next, 0, nil
	}
	return nil, http.StatusNotFound, fmt.Errorf("this repo has no part in task #%d; add one with add_part", t.ID)
}

// changeMemberTask applies e to a task the caller's repo has a part in.
// Any other task is a 404, never a change to work the session does not know.
func (s *Server) changeMemberTask(who taskFiler, id int64, e taskq.Edit, verb string) (string, int, error) {
	t, all, status, err := s.memberTask(who, id)
	if err != nil {
		return "", status, err
	}
	next, err := taskq.Apply(t, all, e)
	if err != nil {
		return "", http.StatusConflict, err
	}
	if err := s.saveTaskChange(t, next); err != nil {
		return "", http.StatusInternalServerError, err
	}
	return fmt.Sprintf("%s task #%d (%s).", verb, id, next.Status), 0, nil
}

// memberTask loads task id with the whole list, if the caller's repo has a
// part in it.
func (s *Server) memberTask(who taskFiler, id int64) (store.Task, []store.Task, int, error) {
	tasks, parts, err := s.board()
	if err != nil {
		return store.Task{}, nil, http.StatusInternalServerError, err
	}
	for _, t := range repoTasks(tasks, parts, who.repo) {
		if t.ID == id {
			return t, tasks, 0, nil
		}
	}
	return store.Task{}, nil, http.StatusNotFound, fmt.Errorf("no task #%d with a part in this repo", id)
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
