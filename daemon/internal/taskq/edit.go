package taskq

import (
	"fmt"
	"strings"

	"ccmux.dev/ccmuxd/internal/store"
)

// Edit is a change a lens or a session asks for. A nil field is left alone.
type Edit struct {
	Title   *string `json:"title"`
	Plan    *string `json:"plan"`
	Status  *string `json:"status"`
	AfterID *int64  `json:"afterId"`
}

// touchesText reports whether e changes what the builder will read.
func (e Edit) touchesText() bool { return e.Title != nil || e.Plan != nil }

// Orderable reports whether a task's "starts after" may change: before it
// runs, and on a cancelled or failed one about to go back in the queue.
func Orderable(status string) bool {
	return Editable(status) || status == Cancelled || status == Failed
}

// Apply checks e against the task and the whole list and returns the task
// as it would be saved. It changes nothing on a refusal.
func Apply(t store.Task, all []store.Task, e Edit) (store.Task, error) {
	if e.touchesText() && !Editable(t.Status) {
		return t, fmt.Errorf("task #%d is %s; only a planning or waiting task can be edited", t.ID, t.Status)
	}
	if e.AfterID != nil && !Orderable(t.Status) {
		return t, fmt.Errorf("task #%d is %s; what it starts after can no longer change", t.ID, t.Status)
	}
	next, err := applyText(t, all, e)
	if err != nil {
		return t, err
	}
	if e.Status != nil && *e.Status != t.Status {
		return moveTo(next, all, *e.Status)
	}
	return next, nil
}

// moveTo checks and makes a status change.
func moveTo(t store.Task, all []store.Task, to string) (store.Task, error) {
	if err := checkMove(t, all, to); err != nil {
		return t, err
	}
	if t.Status == Failed || t.Status == Cancelled {
		t.Note = "" // back in the queue: why it stopped last time is history
	}
	t.Status = to
	return t, nil
}

// checkMove judges a status change. A task put back in the queue has its
// "starts after" checked again: the task it waited for may have been
// cancelled or removed while it sat out.
func checkMove(t store.Task, all []store.Task, to string) error {
	if !CanMove(t.Status, to) {
		return fmt.Errorf("task #%d cannot go from %s to %s", t.ID, t.Status, to)
	}
	if to == Waiting {
		if err := CheckAfter(all, t.ID, t.AfterID); err != nil {
			return fmt.Errorf("%w; change what it starts after first", err)
		}
	}
	return nil
}

// applyText applies the title, plan and "starts after" parts of e.
func applyText(t store.Task, all []store.Task, e Edit) (store.Task, error) {
	next := t
	if e.Title != nil {
		next.Title = strings.TrimSpace(*e.Title)
	}
	if e.Plan != nil {
		next.Plan = *e.Plan
	}
	if err := CheckText(next.Title, next.Plan); err != nil {
		return t, err
	}
	if e.AfterID != nil {
		if err := CheckAfter(all, t.ID, *e.AfterID); err != nil {
			return t, err
		}
		next.AfterID = *e.AfterID
	}
	return next, nil
}
