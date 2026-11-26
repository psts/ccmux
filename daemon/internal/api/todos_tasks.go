package api

// Where the to-do lists meet the task queue. "Make task" on the thread page
// hands an item to its repo's planner (makeTodoTask, planner.go), which
// files it with the tasks tool and names the item (file's todo). From then
// on the item follows that task: tasked while it is on its way, done once
// it is merged, open again if it is cancelled or removed.

import (
	"errors"
	"fmt"
	"log"
	"net/http"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
	"ccmux.dev/ccmuxd/internal/todos"
)

// makeTodoTask: POST /v1/todos/{id}/task {workspaceId, sender, model} →
// {todo, sessionId}. The thread page's "Make task": the item goes into its
// repo's planning line and to the repo's planner (planner.go; made on model
// when the repo has none), which plans it and files it. sessionId is the
// planner's. The repo's main session is never prompted. An item already in
// the line is sent again. workspaceId is the row the button was on; without
// it the first session of the item's repo is used.
func (s *Server) makeTodoTask(w http.ResponseWriter, r *http.Request) {
	if !s.todosReady(w) || !s.runnersReady(w) {
		return
	}
	var in struct{ WorkspaceID, Sender, Model string }
	if !decodeJSON(w, r, &in) {
		return
	}
	id, ok := todoID(w, r)
	if !ok {
		return
	}
	item, err := s.todos.Get(id)
	if err != nil {
		writeTodoError(w, err)
		return
	}
	ws, status, err := s.todoTaskSession(item, in.WorkspaceID)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	s.plannerMu.Lock()
	defer s.plannerMu.Unlock()
	next, sessionID, status, err := s.handToPlanner(id, ws, in.Model, in.Sender)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"todo": next, "sessionId": sessionID})
}

// todoTaskSession is the repo session an item's planner is made in, or why
// there is none: a done item is no task, the session must be one of the
// item's own repo, and it must be in a ccmux window, whose queue the task
// goes into (the tasks tool refuses a session that is in none).
func (s *Server) todoTaskSession(item store.Todo, wsID string) (*model.Workspace, int, error) {
	if item.Status == todos.Done {
		return nil, http.StatusConflict, fmt.Errorf("to-do #%d is done; put it back on the list before making it a task", item.ID)
	}
	ws := s.todoRepoSession(item.Repo, wsID)
	if ws == nil {
		return nil, http.StatusConflict, fmt.Errorf("no session on this daemon is open for %s: open the repo in ccmux first", item.Repo)
	}
	window, err := s.mgr.WindowOfWorkspace(ws.ID)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("reading %s's window: %w", ws.Name, err)
	}
	if window == "" {
		return nil, http.StatusConflict, fmt.Errorf("%s is in no ccmux window, and each window has its own task queue: put the session in a window first", ws.Name)
	}
	return ws, 0, nil
}

// todoRepoSession is the repo session named wsID when it is one of repo's
// (a list's key), else the first of repo's sessions; nil for none.
func (s *Server) todoRepoSession(repo, wsID string) *model.Workspace {
	repos := s.todoRepos()
	if ws := s.mgr.Workspace(wsID); ws != nil && repos[ws.ID] == repo {
		return ws
	}
	if wsID != "" {
		return nil // the row named is not this repo's: never another session by guess
	}
	for _, ws := range s.mgr.List() {
		if repos[ws.ID] == repo {
			return ws
		}
	}
	return nil
}

// todoToFile says why who cannot file the items ids as a task, before
// anything is filed: a wrong number refuses the filing, with nothing
// changed. None is a filing for no item.
func (s *Server) todoToFile(who taskFiler, ids todoList) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if s.todos == nil {
		return http.StatusServiceUnavailable, errors.New("the to-do lists are not available on this daemon; file without todo")
	}
	key, err := s.todoKeys.Of(who.repo)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	for _, id := range ids {
		if err := s.todos.Fileable(id, key); err != nil {
			return todoStatus(err), fmt.Errorf("%w; file again with the right todo number, or without one", err)
		}
	}
	return 0, nil
}

// todoFiled records that the items ids were filed as task taskID and
// returns what the filing's message says of each. The task is filed either
// way: a link that could not be written is said, and logged, and the item
// stays as it was for a person to tick off.
func (s *Server) todoFiled(ids todoList, taskID int64) string {
	said := ""
	for _, id := range ids {
		if err := s.todos.Filed(id, taskID); err != nil {
			log.Printf("todos: #%d was not linked to task #%d: %v", id, taskID, err)
			said += fmt.Sprintf(" To-do #%d could NOT be linked to it (%v): mark it done yourself when the task is merged.", id, err)
			continue
		}
		said += fmt.Sprintf(" To-do #%d now follows it: tasked, and done when the task is merged.", id)
	}
	return said
}

// settleTodos brings every item filed as a task in line with its task
// (todos.Book.Settle). It runs after each change to a task's status, from
// the routes and from the runner, and must not take tasksMu: the routes
// call it holding that lock.
func (s *Server) settleTodos() {
	if s.todos == nil || s.tasks == nil {
		return
	}
	if err := s.todos.Settle(s.todoTaskState); err != nil {
		log.Printf("todos: items were not settled with their tasks: %v", err)
	}
}

// todoTaskState is where task id is, as an item that follows it reads it.
func (s *Server) todoTaskState(id int64) string {
	t, err := s.tasks.Task(id)
	if err != nil {
		log.Printf("todos: task #%d not read, its items are left as they are: %v", id, err)
		return ""
	}
	switch {
	case t == nil:
		return todos.TaskGone
	case t.Status == taskq.Done:
		return todos.TaskMerged
	case t.Status == taskq.Cancelled:
		return todos.TaskCancelled
	}
	return todos.TaskActive
}
