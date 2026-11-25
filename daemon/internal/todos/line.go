package todos

import "ccmux.dev/ccmuxd/internal/store"

// The planning line. "Make task" puts an item in its repo's line (status
// planning) and sends it to the repo's planner, a session that plans each
// item and files it as a task (internal/api planner.go). An item leaves the
// line when it is filed (Filed), ticked off, or put back on the list.

// Plan puts item id in its repo's planning line, at the end, and returns it
// as it is now and as it was (for Unplan). An item already in the line
// keeps its place and comes back unchanged: sending it again is the
// caller's. A done item is no task, and one a task has is that task's.
func (b *Book) Plan(id int64) (now, was store.Todo, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, err := b.item(id)
	if err != nil {
		return store.Todo{}, store.Todo{}, err
	}
	switch {
	case t.Status == Done:
		return t, t, problem(Conflict, "to-do #%d is done; put it back on the list before making it a task", t.ID)
	case t.Status == Tasked && t.TaskID != 0:
		return t, t, problem(Conflict, "to-do #%d is task #%d already", t.ID, t.TaskID)
	case t.Status == Planning:
		return t, t, nil
	}
	was = t
	// Handed on anew: a task it had before (cancelled, since the item was
	// open) and what was noted of that are history.
	t.Status, t.TaskID, t.Note, t.PlanningAt = Planning, 0, "", b.now()
	now, err = b.save(t)
	return now, was, err
}

// Unplan takes an item out of the planning line again because it reached no
// planner: it is as it was before Plan (was, Plan's own answer). An item
// that has left the line since (filed, ticked off, put back) is left alone.
func (b *Book) Unplan(was store.Todo) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, err := b.item(was.ID)
	if err != nil {
		return err
	}
	if t.Status != Planning || was.Status == Planning {
		return nil
	}
	t.Status, t.TaskID, t.Note, t.PlanningAt = was.Status, was.TaskID, was.Note, 0
	_, err = b.save(t)
	return err
}

// Line is a repo's planning line, every repo's when repo is "": the items
// handed to a planner and not filed yet, the longest in the line first.
func (b *Book) Line(repo string) ([]store.Todo, error) {
	return b.Store.PlanningTodos(repo)
}
