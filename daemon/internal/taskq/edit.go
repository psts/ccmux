package taskq

import (
	"fmt"
	"strings"

	"ccmux.dev/ccmuxd/internal/store"
)

// Edit is a change to a task a lens or a session asks for. A nil field is
// left alone. Plans belong to parts: EditPlan changes those.
type Edit struct {
	Title   *string `json:"title"`
	Status  *string `json:"status"`
	AfterID *int64  `json:"afterId"`
}

// Apply checks e against the task and the whole list and returns the task
// as it would be saved. It changes nothing on a refusal.
func Apply(t store.Task, all []store.Task, e Edit) (store.Task, error) {
	if e.Title != nil && !Editable(t.Status) {
		return t, fmt.Errorf("task #%d is %s; only a planning or waiting task can be edited", t.ID, t.Status)
	}
	if e.AfterID != nil && !Orderable(t.Status) {
		return t, fmt.Errorf("task #%d is %s; what it starts after can no longer change", t.ID, t.Status)
	}
	next, err := applyFields(t, all, e)
	if err != nil {
		return t, err
	}
	if e.Status != nil && *e.Status != t.Status {
		return moveTo(next, all, *e.Status)
	}
	return next, nil
}

// applyFields applies the title and "starts after" parts of e.
func applyFields(t store.Task, all []store.Task, e Edit) (store.Task, error) {
	next := t
	if e.Title != nil {
		next.Title = strings.TrimSpace(*e.Title)
		if err := CheckTitle(next.Title); err != nil {
			return t, err
		}
	}
	if e.AfterID != nil {
		if err := CheckAfter(all, t.ID, *e.AfterID); err != nil {
			return t, err
		}
		next.AfterID = *e.AfterID
	}
	return next, nil
}

// moveTo checks and makes a status change. A task put back in the queue has
// its "starts after" checked again (the task it waited for may have been
// cancelled meanwhile) and loses the note on why it stopped.
func moveTo(t store.Task, all []store.Task, to string) (store.Task, error) {
	if !CanMove(t.Status, to) {
		return t, fmt.Errorf("task #%d cannot go from %s to %s", t.ID, t.Status, to)
	}
	if to == Waiting {
		if err := CheckAfter(all, t.ID, t.AfterID); err != nil {
			return t, fmt.Errorf("%w; change what it starts after first", err)
		}
	}
	if t.Status == Cancelled {
		t.Note = ""
	}
	t.Status = to
	return t, nil
}

// EditPlan changes a part's plan while its task has not started.
func EditPlan(t store.Task, p store.Part, plan string) (store.Part, error) {
	if !Editable(t.Status) {
		return p, fmt.Errorf("task #%d is %s; plans change only before it starts", t.ID, t.Status)
	}
	if err := CheckPlan(plan); err != nil {
		return p, err
	}
	p.Plan = plan
	return p, nil
}

// Retry puts a failed part of a started task back to start again.
func Retry(t store.Task, p store.Part) (store.Part, error) {
	if p.Status != Failed || (t.Status != Running && t.Status != NeedsYou) {
		return p, fmt.Errorf("only a failed part of a running task can be retried")
	}
	p.Status, p.Note = Pending, ""
	return p, nil
}

// RetryTask is what Retry on part p does to its feature, given all its
// parts. While another part is at work or asking a person, only p goes back
// to start (it waits for a slot unless the feature holds one). When every
// unbuilt part has failed, the feature goes back in the queue as Waiting
// with each failed part to start again: it then shows, and starts, as a
// waiting task when its window has a slot free. The worktree is kept, so a
// restarted part carries on from its work.
func RetryTask(t store.Task, mine []store.Part, p store.Part) (store.Task, []store.Part, error) {
	next, err := Retry(t, p)
	if err != nil {
		return t, nil, err
	}
	var again []store.Part
	for _, o := range mine {
		switch o.Status {
		case Running, NeedsYou:
			return t, []store.Part{next}, nil
		case Failed:
			o.Status, o.Note = Pending, ""
			again = append(again, o)
		}
	}
	t.Status, t.Note = Waiting, ""
	return t, again, nil
}

// Requeued returns the parts to start again when a cancelled task goes back
// in the queue: every part not already built. A built part stays built.
func Requeued(parts []store.Part) []store.Part {
	var out []store.Part
	for _, p := range parts {
		if p.Status != Ready && p.Status != Pending {
			p.Status, p.Note = Pending, ""
			out = append(out, p)
		}
	}
	return out
}
