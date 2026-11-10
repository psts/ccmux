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
// at least one part that start after nothing, or after a task that is done.
func Startable(tasks []store.Task, parts []store.Part) []store.Task {
	status := make(map[int64]string, len(tasks))
	for _, t := range tasks {
		status[t.ID] = t.Status
	}
	var out []store.Task
	for _, t := range tasks {
		free := t.AfterID == 0 || status[t.AfterID] == Done
		if t.Status == Waiting && free && len(PartsOf(parts, t.ID)) > 0 {
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

// SlotsHeld counts, per window, the tasks holding a slot: those with a part
// at work. Each window has its own slots. The slots count features, not
// Claudes, so every part of a feature runs at once. A feature whose every
// Claude waits on a person holds none: a question asked at night must not
// hold up the rest of the queue.
func SlotsHeld(tasks []store.Task, parts []store.Part) map[string]int {
	working := Working(parts)
	held := map[string]int{}
	for _, t := range tasks {
		if working[t.ID] && (t.Status == Running || t.Status == NeedsYou) {
			held[t.WindowID]++
		}
	}
	return held
}

// Working is the tasks with a part at work, by id: the ones holding a slot.
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
