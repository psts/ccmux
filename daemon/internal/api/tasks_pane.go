package api

// The peers shim's `tasks` tool. A repo's session files a whole feature,
// its own part and every other repo's in its window, in one step, and lists,
// edits or cancels the features it has a part in. A worker building a part
// reports on it, and pulls in another repo by adding a part
// (tasks_worker.go). The filing repo is always the pane's own session's;
// the other repos are only ever ones in the same ccmux window.

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
	// Parts are the other repos' parts of a feature being filed.
	Parts []partReq `json:"parts"`
	// Draft is only read to refuse it: a session still holding the old tool
	// would file a draft for others to add to, and a draft read as a plain
	// filing starts with the lead's part alone.
	Draft *bool `json:"draft"`
	// Repo names a repo by its folder name: the one a worker adds a part
	// for, or the part an edit's plan replaces (this repo's when empty).
	Repo string `json:"repo"`
	// Status and Note are a worker's report on its own part.
	Status string `json:"status"`
	Note   string `json:"note"`
	// Hard says a part is hard to build, so its Claude starts with the
	// stronger model (taskq.HardRun): this repo's part on a filing, the one
	// an edit's plan would go to on an edit, where nil leaves it as it is.
	Hard *bool `json:"hard"`
}

// partReq is one other repo's part of a feature being filed, hard to
// build or not (paneTaskReq.Hard).
type partReq struct {
	Repo string `json:"repo"`
	Plan string `json:"plan"`
	Hard bool   `json:"hard"`
}

// taskFiler is who is asking: the repo, the session name stamped on what
// it files, the pane (a worker is known by its pane), and the ccmux window
// its session is in, whose queue it files into ("" for none).
type taskFiler struct{ repo, session, pane, window string }

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
	window, err := s.mgr.WindowOfWorkspace(ws.ID)
	if err != nil {
		return taskFiler{}, http.StatusInternalServerError, "reading this session's window: " + err.Error()
	}
	return taskFiler{repo: ws.RepoPath, session: ws.Name, pane: paneID, window: window}, 0, ""
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
	if req.Draft != nil {
		return "", http.StatusBadRequest, errors.New("drafts are gone: file the whole feature in one call, this repo's part as plan and every other repo's in parts (restart this session for the current tasks tool)")
	}
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

// fileTask files a whole feature at once from the session it was planned
// in: this repo's part (plan) and every other repo's (parts), all in this
// session's window. It waits in the queue at once; nothing else has to act.
func (s *Server) fileTask(who taskFiler, req paneTaskReq) (string, int, error) {
	if who.window == "" {
		return "", http.StatusConflict, errors.New("this session is in no ccmux window, and each window has its own task queue; put the session in a window first")
	}
	if err := taskq.CheckTitle(req.Title); err != nil {
		return "", http.StatusBadRequest, err
	}
	parts, err := s.partsToFile(who, req)
	if err != nil {
		return "", http.StatusBadRequest, err
	}
	all, err := s.tasks.Tasks()
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	after := deref(req.After)
	if err := taskq.CheckAfter(taskq.InWindow(all, who.window), 0, after); err != nil {
		return "", http.StatusBadRequest, err
	}
	now := s.taskNow()
	for i := range parts {
		parts[i].Status, parts[i].FiledBy, parts[i].CreatedAt, parts[i].UpdatedAt = taskq.Pending, who.session, now, now
	}
	id, err := s.tasks.AddTaskWithParts(
		store.Task{Title: strings.TrimSpace(req.Title), Status: taskq.Waiting, AfterID: after, FiledBy: who.session, WindowID: who.window, CreatedAt: now, UpdatedAt: now},
		parts)
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	s.mgr.TasksChanged()
	return fileMessage(id, parts, after), 0, nil
}

// partsToFile is a feature's parts: this repo's, always (the filing session
// reaches its task through its own part: list, edit, cancel), then each
// named repo's, found among the sessions in this one's window. One part per
// repo, each with a plan and what its Claude starts with (taskq.WithRun).
func (s *Server) partsToFile(who taskFiler, req paneTaskReq) ([]store.Part, error) {
	var out []store.Part
	seen := map[string]bool{}
	add := func(repo, plan string, hard bool) error {
		name := filepath.Base(repo)
		if seen[repo] {
			return fmt.Errorf("%s has two parts; give each repo one plan", name)
		}
		if err := taskq.CheckPlan(plan); err != nil {
			return fmt.Errorf("%s's part: %w", name, err)
		}
		seen[repo] = true
		out = append(out, taskq.WithRun(store.Part{RepoPath: repo, Plan: plan}, hard))
		return nil
	}
	if err := add(who.repo, req.Plan, deref(req.Hard)); err != nil {
		return nil, err
	}
	if len(req.Parts) == 0 {
		return out, nil
	}
	peers := s.mgr.WindowPeers(who.pane)
	for _, p := range req.Parts {
		repo, err := repoNamed(peers, p.Repo)
		if err != nil {
			return nil, fmt.Errorf("%w (a feature's parts are repos in this session's ccmux window)", err)
		}
		if err := add(repo, p.Plan, p.Hard); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func fileMessage(id int64, parts []store.Part, after int64) string {
	names := make([]string, len(parts))
	for i, p := range parts {
		names[i] = filepath.Base(p.RepoPath)
	}
	msg := fmt.Sprintf("Filed task #%d with parts for %s: it waits in the queue for a free slot", id, strings.Join(names, ", "))
	if after != 0 {
		msg += fmt.Sprintf(", and will not start before task #%d is done and merged", after)
	}
	return msg + "."
}

// editPaneTask changes a task this repo has a part in: its title, what it
// starts after, and one part's plan and whether it is hard (this repo's
// part, or the one req.Repo names). Everything is checked before anything
// is saved, so a refused edit changes nothing.
func (s *Server) editPaneTask(who taskFiler, req paneTaskReq) (string, int, error) {
	t, all, status, err := s.memberTask(who, req.ID)
	if err != nil {
		return "", status, err
	}
	next, err := taskq.Apply(t, all, taskEditOf(req))
	if err != nil {
		return "", http.StatusConflict, err
	}
	part, status, err := s.partEditOf(t, partRepoOf(who, req), req)
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
	return e
}

// partRepoOf is the folder name of the part an edit is for: the one named,
// else the caller's own repo's.
func partRepoOf(who taskFiler, req paneTaskReq) string {
	if name := strings.TrimSpace(req.Repo); name != "" {
		return name
	}
	return filepath.Base(who.repo)
}

// partEditOf checks an edit's changes to one part of task t, the one in
// the repo whose folder is named name: a new plan, and whether the part is
// hard. nil when the edit carries neither.
func (s *Server) partEditOf(t store.Task, name string, req paneTaskReq) (*store.Part, int, error) {
	if req.Plan == "" && req.Hard == nil {
		return nil, 0, nil
	}
	next, status, err := s.partNamed(t, name)
	if err != nil {
		return nil, status, err
	}
	if req.Plan != "" {
		if next, err = taskq.EditPlan(t, next, req.Plan); err != nil {
			return nil, http.StatusConflict, err
		}
	}
	if req.Hard != nil {
		if next, err = taskq.EditRun(t, next, *req.Hard); err != nil {
			return nil, http.StatusConflict, err
		}
	}
	return &next, 0, nil
}

// partNamed is the part of task t in the repo whose folder is named name.
func (s *Server) partNamed(t store.Task, name string) (store.Part, int, error) {
	parts, err := s.tasks.Parts()
	if err != nil {
		return store.Part{}, http.StatusInternalServerError, err
	}
	for _, p := range taskq.PartsOf(parts, t.ID) {
		if filepath.Base(p.RepoPath) == name {
			return p, 0, nil
		}
	}
	return store.Part{}, http.StatusNotFound, fmt.Errorf("task #%d has no part in %s", t.ID, name)
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

// memberTask loads task id with its window's queue, if the caller's repo
// has a part in it.
func (s *Server) memberTask(who taskFiler, id int64) (store.Task, []store.Task, int, error) {
	tasks, parts, err := s.board()
	if err != nil {
		return store.Task{}, nil, http.StatusInternalServerError, err
	}
	for _, t := range repoTasks(tasks, parts, who.repo) {
		if t.ID == id {
			return t, taskq.InWindow(tasks, t.WindowID), 0, nil
		}
	}
	return store.Task{}, nil, http.StatusNotFound, fmt.Errorf("no task #%d with a part in this repo", id)
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
