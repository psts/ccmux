package api

// The peers shim's `tasks` tool: a repo's session files the plan it just
// made with its human, and can list, edit or cancel that repo's tasks. The
// repo is the pane's own session's repo, never an argument, so a session can
// only file and touch work in the repo it knows.

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
}

// taskFiler is who is asking: the repo the task belongs to and the session
// name stamped on it.
type taskFiler struct{ repo, session string }

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
	all, err := s.tasks.Tasks()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": message, "tasks": taskViewsOf(repoTasks(all, who.repo))})
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
	return taskFiler{repo: ws.RepoPath, session: ws.Name}, 0, ""
}

func repoTasks(all []store.Task, repo string) []store.Task {
	out := []store.Task{}
	for _, t := range all {
		if t.RepoPath == repo {
			out = append(out, t)
		}
	}
	return out
}

func (s *Server) applyPaneTask(who taskFiler, req paneTaskReq) (string, int, error) {
	switch req.Action {
	case "file":
		return s.fileTask(who, req)
	case "list":
		return "", 0, nil
	case "edit":
		return s.editPaneTask(who, req)
	case "cancel":
		cancel := taskq.Cancelled
		return s.changePaneTask(who, req.ID, taskq.Edit{Status: &cancel}, "Cancelled")
	}
	return "", http.StatusBadRequest, errors.New("action must be one of file, list, edit, cancel")
}

func (s *Server) fileTask(who taskFiler, req paneTaskReq) (string, int, error) {
	if err := taskq.CheckText(req.Title, req.Plan); err != nil {
		return "", http.StatusBadRequest, err
	}
	all, err := s.tasks.Tasks()
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	after := int64(0)
	if req.After != nil {
		after = *req.After
	}
	if err := taskq.CheckAfter(all, 0, after); err != nil {
		return "", http.StatusBadRequest, err
	}
	status := taskq.Waiting
	if req.Draft != nil && *req.Draft {
		status = taskq.Planning
	}
	now := s.taskNow()
	id, err := s.tasks.AddTask(store.Task{
		RepoPath: who.repo, Title: strings.TrimSpace(req.Title), Plan: req.Plan, Status: status,
		AfterID: after, FiledBy: who.session, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	s.mgr.TasksChanged()
	return fileMessage(id, status, after), 0, nil
}

func fileMessage(id int64, status string, after int64) string {
	msg := fmt.Sprintf("Filed task #%d", id)
	if status == taskq.Planning {
		msg += " as a draft: it will not run until it is marked ready"
	} else {
		msg += ": it waits in the queue for a free slot"
	}
	if after != 0 {
		msg += fmt.Sprintf(", and not before task #%d is done and merged", after)
	}
	return msg + ". Tell other repos this number if their part should start after it."
}

func (s *Server) editPaneTask(who taskFiler, req paneTaskReq) (string, int, error) {
	var e taskq.Edit
	if req.Title != "" {
		e.Title = &req.Title
	}
	if req.Plan != "" {
		e.Plan = &req.Plan
	}
	e.AfterID = req.After
	if req.Draft != nil {
		status := taskq.Waiting
		if *req.Draft {
			status = taskq.Planning
		}
		e.Status = &status
	}
	return s.changePaneTask(who, req.ID, e, "Updated")
}

// changePaneTask applies e to one of this repo's tasks. Another repo's id is
// a 404, never a change to work the session does not know.
func (s *Server) changePaneTask(who taskFiler, id int64, e taskq.Edit, verb string) (string, int, error) {
	t, err := s.tasks.Task(id)
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	if t == nil || t.RepoPath != who.repo {
		return "", http.StatusNotFound, fmt.Errorf("no task #%d in this repo", id)
	}
	all, err := s.tasks.Tasks()
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	next, err := taskq.Apply(*t, all, e)
	if err != nil {
		return "", http.StatusConflict, err
	}
	if err := s.saveTask(next); err != nil {
		return "", http.StatusInternalServerError, err
	}
	return fmt.Sprintf("%s task #%d (%s).", verb, id, next.Status), 0, nil
}
