package api

// Where the to-do lists meet the task queue. "Make task" on the thread page
// hands an item to its repo's session, which files it with the tasks tool
// and names the item (file's todo). From then on the item follows that
// task: tasked while it is on its way, done once it is merged, open again
// if it is cancelled or removed.

import (
	"errors"
	"fmt"
	"log"
	"net/http"

	"ccmux.dev/ccmuxd/internal/taskq"
	"ccmux.dev/ccmuxd/internal/todos"
)

// todoToFile says why who cannot file item id as a task, before anything
// is filed: a wrong number refuses the filing, with nothing changed. 0 is
// a filing for no item.
func (s *Server) todoToFile(who taskFiler, id int64) (int, error) {
	if id == 0 {
		return 0, nil
	}
	if s.todos == nil {
		return http.StatusServiceUnavailable, errors.New("the to-do lists are not available on this daemon; file without todo")
	}
	key, err := s.todoKeys.Of(who.repo)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	if err := s.todos.Fileable(id, key); err != nil {
		return todoStatus(err), fmt.Errorf("%w; file again with the right todo number, or without one", err)
	}
	return 0, nil
}

// todoFiled records that item id was filed as task taskID and returns what
// the filing's message says of it. The task is filed either way: a link
// that could not be written is said, and logged, and the item stays as it
// was for a person to tick off.
func (s *Server) todoFiled(id, taskID int64) string {
	if id == 0 {
		return ""
	}
	if err := s.todos.Filed(id, taskID); err != nil {
		log.Printf("todos: #%d was not linked to task #%d: %v", id, taskID, err)
		return fmt.Sprintf(" To-do #%d could NOT be linked to it (%v): mark it done yourself when the task is merged.", id, err)
	}
	return fmt.Sprintf(" To-do #%d now follows it: tasked, and done when the task is merged.", id)
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
