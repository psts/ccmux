package api

import "errors"

// What the weekly sweep and the disk check need of the server
// (cmd/ccmuxd/sweep.go): a to-do that is filed once per thing, and the
// disks' line in GET /v1/health.

// AddTodoOnce files an item on a repo's to-do list unless an unfinished
// one there starts with prefix (todos.Book.AddOnce): a thing whose
// numbers change from one look to the next is one item. The folder is
// keyed to its repo, and the page hears of a new item. added says
// whether the item is new.
func (s *Server) AddTodoOnce(repo, prefix, text, urgency, by string) (added bool, err error) {
	if s.todos == nil {
		return false, errors.New("the to-do lists are not available on this daemon")
	}
	key, err := s.todoKeys.Of(repo)
	if err != nil {
		return false, err
	}
	_, added, err = s.todos.AddOnce(key, prefix, text, urgency, by)
	return added, err
}

// SetDiskHealth supplies the "disks" part of GET /v1/health.
func (s *Server) SetDiskHealth(f func() any) { s.diskHealth = f }
