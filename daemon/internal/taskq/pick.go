package taskq

import (
	"fmt"

	"ccmux.dev/ccmuxd/internal/store"
)

// Ready lists the waiting tasks free to start, in queue order: those that
// start after nothing, or after a task that is done (built and merged).
func Ready(all []store.Task) []store.Task {
	status := make(map[int64]string, len(all))
	for _, t := range all {
		status[t.ID] = t.Status
	}
	var out []store.Task
	for _, t := range all {
		if t.Status == Waiting && (t.AfterID == 0 || status[t.AfterID] == Done) {
			out = append(out, t)
		}
	}
	return out
}

// CountRunning counts the tasks holding a slot. A task that needs a person does
// not: its Claude is waiting, not working, and a question asked at night
// must not hold up the rest of the queue until morning.
func CountRunning(all []store.Task) int {
	n := 0
	for _, t := range all {
		if t.Status == Running {
			n++
		}
	}
	return n
}

// Report applies a worker's own report on its task: "done" (the work is
// committed on its branch) or "stuck" (it needs a person).
func Report(t store.Task, kind, note string, now int64) (store.Task, error) {
	if t.Status != Running && t.Status != NeedsYou {
		return t, fmt.Errorf("task #%d is %s, so there is nothing to report on", t.ID, t.Status)
	}
	switch kind {
	case "done":
		t.Status, t.FinishedAt = ReadyToMerge, now
	case "stuck":
		t.Status = NeedsYou
	default:
		return t, fmt.Errorf(`status must be "done" or "stuck"`)
	}
	t.Note = note
	return t, nil
}
