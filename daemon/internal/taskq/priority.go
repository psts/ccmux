package taskq

import (
	"log"
	"sort"

	"ccmux.dev/ccmuxd/internal/store"
)

// ByPriority is the order the runner serves tasks in, as indices into
// tasks: by their window's priority, 1 first, a window with none after
// every numbered one. The sort is stable, so tasks of the same priority
// keep queue order. It only decides anything where windows compete for
// the same slot: the cap across windows (TotalSlots), or the one merge at
// a time.
func ByPriority(tasks []store.Task, prio map[string]int) []int {
	rank := func(i int) int {
		if n, ok := prio[tasks[i].WindowID]; ok && n > 0 {
			return n
		}
		return int(^uint(0) >> 1) // unset: after every number
	}
	order := make([]int, len(tasks))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return rank(order[a]) < rank(order[b]) })
	return order
}

// inOrder is tasks rearranged by order (ByPriority), a copy.
func inOrder(tasks []store.Task, order []int) []store.Task {
	out := make([]store.Task, len(order))
	for k, i := range order {
		out[k] = tasks[i]
	}
	return out
}

// priorityOrder reads every window's priority once per pass. Unlike the
// slots, an unreadable list stops nothing: a wrong order is harmless, so
// the pass runs in queue order and says why.
func (r *Runner) priorityOrder(tasks []store.Task) []int {
	var prio map[string]int
	if r.WindowPriorities != nil {
		var err error
		if prio, err = r.WindowPriorities(); err != nil {
			log.Printf("tasks: window priorities unreadable, this pass runs in queue order: %v", err)
			prio = nil
		}
	}
	return ByPriority(tasks, prio)
}
