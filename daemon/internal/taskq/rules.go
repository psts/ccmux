// Package taskq is the task queue: features a repo's session files to be
// built later, each by one part per repo, every part a Claude in its own
// worktree; a few features at a time. This file holds the rules every
// surface shares (the lens routes, the peers tool, the runner), so a move a
// lens may make and one the tool may make can never disagree.
package taskq

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"ccmux.dev/ccmuxd/internal/store"
)

// A task's statuses. planning and waiting are the human's (a draft, or
// ready to run); running, needs_you and ready_to_merge follow its parts
// (Rollup); merging and merge_failed are the runner's merge (merge.go);
// done is merged, by the runner or by hand; cancelled is a person's call.
const (
	Planning     = "planning"
	Waiting      = "waiting"
	Running      = "running"
	NeedsYou     = "needs_you"
	ReadyToMerge = "ready_to_merge"
	Merging      = "merging"
	MergeFailed  = "merge_failed"
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
var Statuses = []string{Planning, Waiting, Running, NeedsYou, ReadyToMerge, Merging, MergeFailed, Done, Cancelled}

const (
	MaxTitle = 120
	MaxPlan  = 20000
)

// humanMoves is what a lens or a session may do to a task's status: file a
// draft, pull a waiting task back to draft, cancel anything not finished
// (a running one's Claudes are closed), put a cancelled one back in the
// queue, mark built work merged by hand, and send a failed merge back to
// be tried again. A merge in progress is the runner's alone.
var humanMoves = map[string][]string{
	Planning:     {Waiting, Cancelled},
	Waiting:      {Planning, Cancelled},
	Running:      {Cancelled},
	NeedsYou:     {Cancelled},
	ReadyToMerge: {Done, Cancelled},
	MergeFailed:  {ReadyToMerge, Done, Cancelled},
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

// Orderable reports whether the tasks a task starts after may change: before it
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

// AfterList tidies a "starts after" list as it was asked for: no 0 (the
// one-number way to say "none") and each task once, in the order given.
func AfterList(ids []int64) []int64 {
	var out []int64
	for _, id := range ids {
		if id != 0 && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// CheckAfter judges "task id starts after every task in afters" against the
// whole list (id 0 for a task not filed yet). It refuses a missing or
// cancelled task and any chain that leads back to id, so the runner can
// never wait on a loop.
func CheckAfter(all []store.Task, id int64, afters []int64) error {
	byID := tasksByID(all)
	for _, after := range afters {
		dep, ok := byID[after]
		if !ok {
			return fmt.Errorf("there is no task #%d to start after", after)
		}
		if dep.Status == Cancelled {
			return fmt.Errorf("task #%d is cancelled, so nothing can wait for it", after)
		}
	}
	if loops(byID, afters, map[int64]bool{id: true}, map[int64]bool{}) {
		return errors.New("that order makes a loop: these tasks would wait for each other forever")
	}
	return nil
}

// loops reports whether following "starts after" from ids comes back to a
// task whose own links are still being followed (on): a loop. cleared
// holds the tasks already followed to the end, so two tasks that wait on
// the same third are not taken for one.
func loops(byID map[int64]store.Task, ids []int64, on, cleared map[int64]bool) bool {
	for _, id := range ids {
		if on[id] {
			return true
		}
		if cleared[id] {
			continue
		}
		on[id] = true
		if loops(byID, byID[id].AfterIDs, on, cleared) {
			return true
		}
		on[id], cleared[id] = false, true
	}
	return false
}

func tasksByID(all []store.Task) map[int64]store.Task {
	byID := make(map[int64]store.Task, len(all))
	for _, t := range all {
		byID[t.ID] = t
	}
	return byID
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

// Neighbour finds the task id trades places with on a move up or down: the
// nearest one before (up) or after it in queue order with the same status,
// and for a waiting task the same Ahead as well. Lenses list Waiting by
// Ahead, then queue order, so that neighbour is the row next to it on
// screen and the move always shows.
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
	same := func(o store.Task) bool {
		return o.Status == all[at].Status && (o.Status != Waiting || Ahead(all, o.ID) == Ahead(all, id))
	}
	for i := at + step; i >= 0 && i < len(all); i += step {
		if same(all[i]) {
			return all[i], true
		}
	}
	return store.Task{}, false
}

// Ahead counts the unfinished tasks id waits on: the ones it starts after,
// the ones those start after, and so on, each once. 0 can start as soon as
// a slot frees. -1 when that reaches a cancelled or removed task, or loops:
// it holds until a person acts.
func Ahead(all []store.Task, id int64) int {
	w := aheadWalk{byID: tasksByID(all), on: map[int64]bool{id: true}, counted: map[int64]bool{}}
	if !w.follow(w.byID[id].AfterIDs) {
		return -1
	}
	return len(w.counted)
}

// aheadWalk follows "starts after" from one task: on is the tasks whose
// own links are being followed now, counted the unfinished ones met.
type aheadWalk struct {
	byID        map[int64]store.Task
	on, counted map[int64]bool
}

// follow walks ids and what they wait on in turn, stopping at a done task:
// nothing behind it is waited for. False when a task on the way is gone or
// cancelled, or the walk comes back to one it is still following.
func (w *aheadWalk) follow(ids []int64) bool {
	for _, id := range ids {
		dep, ok := w.byID[id]
		if !ok || dep.Status == Cancelled || w.on[id] {
			return false
		}
		if dep.Status == Done || w.counted[id] {
			continue
		}
		w.on[id], w.counted[id] = true, true
		if !w.follow(dep.AfterIDs) {
			return false
		}
		w.on[id] = false
	}
	return true
}

// Waiters lists the tasks still to run that start after id. A task with
// waiters is not removed: they would be left waiting on nothing.
func Waiters(all []store.Task, id int64) []int64 {
	var out []int64
	for _, t := range all {
		if slices.Contains(t.AfterIDs, id) && !Removable(t.Status) {
			out = append(out, t.ID)
		}
	}
	return out
}
