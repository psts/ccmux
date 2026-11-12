package taskq

import (
	"fmt"

	"ccmux.dev/ccmuxd/internal/store"
)

// PartsOf picks one task's parts out of the whole list.
func PartsOf(parts []store.Part, taskID int64) []store.Part {
	var out []store.Part
	for _, p := range parts {
		if p.TaskID == taskID {
			out = append(out, p)
		}
	}
	return out
}

// Startable lists the waiting tasks free to start, in queue order: those with
// at least one part whose every "starts after" task is done. One that was
// removed is not done, so its waiter holds until a person acts.
func Startable(tasks []store.Task, parts []store.Part) []store.Task {
	status := make(map[int64]string, len(tasks))
	for _, t := range tasks {
		status[t.ID] = t.Status
	}
	free := func(t store.Task) bool {
		for _, after := range t.AfterIDs {
			if status[after] != Done {
				return false
			}
		}
		return true
	}
	var out []store.Task
	for _, t := range tasks {
		if t.Status == Waiting && free(t) && len(PartsOf(parts, t.ID)) > 0 {
			out = append(out, t)
		}
	}
	return out
}

// InWindow is the tasks of one ccmux window's queue. Its order, "starts
// after" and neighbours are all within it: each window is its own queue.
func InWindow(tasks []store.Task, windowID string) []store.Task {
	out := []store.Task{}
	for _, t := range tasks {
		if t.WindowID == windowID {
			out = append(out, t)
		}
	}
	return out
}

// SlotsHeld counts, per window, the tasks holding a slot. Each window has
// its own slots. The slots count features, not Claudes, so every part of a
// feature runs at once. A feature holds its slot from its start until it is
// merged: while a part is at work, and then while it waits its turn to merge
// and its checks run. The checks are as heavy as the build was, and the next
// feature should start from a trunk that has this one in it; a slot given up
// at "done" started the next build on top of the checks, which then failed
// on tests that only lose to a busy machine.
//
// Two kinds of waiting on a person hold none, so a night is never lost to
// one: a feature whose every Claude waits on a person (a question), and a
// built one in parked, which waits on a person's own folder (pickMerge). A
// failed merge holds none either: it takes a slot again when it goes back
// to its worker.
func SlotsHeld(tasks []store.Task, parts []store.Part, parked map[int64]bool) map[string]int {
	working := Working(parts)
	held := map[string]int{}
	for _, t := range tasks {
		if holdsSlot(t, working[t.ID], parked[t.ID]) {
			held[t.WindowID]++
		}
	}
	return held
}

// holdsSlot is the rule for one task: at work, or built and on its way in.
func holdsSlot(t store.Task, working, parked bool) bool {
	switch t.Status {
	case Running, NeedsYou:
		return working
	case ReadyToMerge:
		return !parked
	case Merging:
		return true
	}
	return false
}

// Working is the tasks with a part at work, by id.
func Working(parts []store.Part) map[int64]bool {
	working := map[int64]bool{}
	for _, p := range parts {
		if p.Status == Running {
			working[p.TaskID] = true
		}
	}
	return working
}

// Rollup is a started task's status from its parts: needs_you when any part
// needs a person or failed (someone must decide), else running while any
// part is still to finish, else ready_to_merge.
func Rollup(parts []store.Part) string {
	busy := false
	for _, p := range parts {
		switch p.Status {
		case NeedsYou, Failed:
			return NeedsYou
		case Running, Pending:
			busy = true
		}
	}
	if busy {
		return Running
	}
	return ReadyToMerge
}

// Report applies a worker's own report on its part: "done" (the work is
// committed on its branch) or "stuck" (it needs a person).
func Report(p store.Part, kind, note string, now int64) (store.Part, error) {
	if p.Status != Running && p.Status != NeedsYou {
		return p, fmt.Errorf("this part is %s, so there is nothing to report on", p.Status)
	}
	switch kind {
	case "done":
		p.Status, p.FinishedAt = Ready, now
	case "stuck":
		p.Status = NeedsYou
	default:
		return p, fmt.Errorf(`status must be "done" or "stuck"`)
	}
	p.Note = note
	return p, nil
}
