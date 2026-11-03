// Package taskq is the task queue: features a repo's session files to be
// built later, each by one part per repo, every part a Claude in its own
// worktree; a few features at a time. This file holds the rules every
// surface shares (the lens routes, the peers tool, the runner), so a move a
// lens may make and one the tool may make can never disagree.
package taskq

import (
	"errors"
	"fmt"
	"strings"

	"ccmux.dev/ccmuxd/internal/store"
)

// A task's statuses. planning and waiting are the human's (a draft, or
// ready to run); running, needs_you and ready_to_merge follow its parts
// (Rollup); done and cancelled are a person's call.
const (
	Planning     = "planning"
	Waiting      = "waiting"
	Running      = "running"
	NeedsYou     = "needs_you"
	ReadyToMerge = "ready_to_merge"
	Done         = "done"
	Cancelled    = "cancelled"
)

// A part's statuses. Running and NeedsYou are shared with tasks.
const (
	Pending = "pending" // not started (or stopped, to start again)
	Ready   = "ready"   // its Claude reported the work done
	Failed  = "failed"  // it could not start, or its Claude went away
)

// Statuses is every task status in the order the lenses show them.
var Statuses = []string{Planning, Waiting, Running, NeedsYou, ReadyToMerge, Done, Cancelled}

const (
	MaxTitle = 120
	MaxPlan  = 20000
)

// humanMoves is what a lens or a session may do to a task's status: file a
// draft, pull a waiting task back to draft, cancel anything not finished
// (a running one's Claudes are closed), put a cancelled one back in the
// queue, and mark built work merged (by hand until the runner merges).
var humanMoves = map[string][]string{
	Planning:     {Waiting, Cancelled},
	Waiting:      {Planning, Cancelled},
	Running:      {Cancelled},
	NeedsYou:     {Cancelled},
	ReadyToMerge: {Done, Cancelled},
	Cancelled:    {Waiting},
}

// Moves lists the statuses a human may move a task to from status, so the
// lenses offer exactly the buttons the daemon will accept.
func Moves(status string) []string { return append([]string{}, humanMoves[status]...) }

// CanMove reports whether a human may move a task from one status to another.
func CanMove(from, to string) bool {
	for _, s := range humanMoves[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Editable reports whether a task's title, its parts' plans and its order
// may still change: only before it starts, so no worker reads a plan that
// moved under it.
func Editable(status string) bool { return status == Planning || status == Waiting }

// Orderable reports whether what a task starts after may change: before it
// runs, and on a cancelled one about to go back in the queue.
func Orderable(status string) bool { return Editable(status) || status == Cancelled }

// Removable reports whether a task may be deleted from the list for good.
func Removable(status string) bool { return status == Done || status == Cancelled }

// CheckTitle judges a task's title.
func CheckTitle(title string) error {
	if t := strings.TrimSpace(title); t == "" || len(t) > MaxTitle {
		return fmt.Errorf("title must be 1 to %d characters", MaxTitle)
	}
	return nil
}

// CheckPlan judges a part's plan.
func CheckPlan(plan string) error {
	if p := strings.TrimSpace(plan); p == "" || len(p) > MaxPlan {
		return fmt.Errorf("plan must be 1 to %d characters", MaxPlan)
	}
	return nil
}

// CheckAfter judges "task id starts after task after" against the whole list
// (id 0 for a task not filed yet). It refuses a missing or cancelled task and
// any chain that leads back to id, so the runner can never wait on a loop.
func CheckAfter(all []store.Task, id, after int64) error {
	if after == 0 {
		return nil
	}
	byID := make(map[int64]store.Task, len(all))
	for _, t := range all {
		byID[t.ID] = t
	}
	dep, ok := byID[after]
	if !ok {
		return fmt.Errorf("there is no task #%d to start after", after)
	}
	if dep.Status == Cancelled {
		return fmt.Errorf("task #%d is cancelled, so nothing can wait for it", after)
	}
	for seen := 0; dep.ID != 0; seen++ {
		if dep.ID == id || seen > len(all) {
			return errors.New("that order makes a loop: these tasks would wait for each other forever")
		}
		dep = byID[dep.AfterID]
	}
	return nil
}

// Counts tallies the tasks by status, every status present even at zero.
func Counts(all []store.Task) map[string]int {
	out := make(map[string]int, len(Statuses))
	for _, s := range Statuses {
		out[s] = 0
	}
	for _, t := range all {
		out[t.Status]++
	}
	return out
}

// Neighbour finds the nearest task with the same status before (up) or after
// id in queue order, the one a move trades places with. Same status, so a
// move is always visible in a list the lenses split by status.
func Neighbour(all []store.Task, id int64, up bool) (store.Task, bool) {
	at := -1
	for i, t := range all {
		if t.ID == id {
			at = i
		}
	}
	if at < 0 {
		return store.Task{}, false
	}
	step := 1
	if up {
		step = -1
	}
	for i := at + step; i >= 0 && i < len(all); i += step {
		if all[i].Status == all[at].Status {
			return all[i], true
		}
	}
	return store.Task{}, false
}

// Waiters lists the tasks still to run that start after id. A task with
// waiters is not removed: they would be left waiting on nothing.
func Waiters(all []store.Task, id int64) []int64 {
	var out []int64
	for _, t := range all {
		if t.AfterID == id && !Removable(t.Status) {
			out = append(out, t.ID)
		}
	}
	return out
}
