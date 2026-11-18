package taskq

import (
	"errors"
	"reflect"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

// The order the runner serves windows in: priority 1 first, then 2, a
// window with no priority after every numbered one, and ties (the same
// number, or both unset) keep queue order.
func TestByPriority(t *testing.T) {
	task := func(id int64, w string) store.Task { return store.Task{ID: id, WindowID: w} }
	cases := []struct {
		name  string
		tasks []store.Task
		prio  map[string]int
		want  []int64
	}{
		{"no priorities keep queue order", []store.Task{task(1, "a"), task(2, "b"), task(3, "a")}, nil, []int64{1, 2, 3}},
		{"1 before 2", []store.Task{task(1, "b"), task(2, "a")}, map[string]int{"a": 1, "b": 2}, []int64{2, 1}},
		{"unset goes last", []store.Task{task(1, "none"), task(2, "a")}, map[string]int{"a": 9}, []int64{2, 1}},
		{"ties keep queue order", []store.Task{task(1, "a"), task(2, "b"), task(3, "a"), task(4, "b")}, map[string]int{"a": 2, "b": 2}, []int64{1, 2, 3, 4}},
		{"unset ties keep queue order", []store.Task{task(1, "x"), task(2, "a"), task(3, "y")}, map[string]int{"a": 1}, []int64{2, 1, 3}},
		{"every task of the window moves with it", []store.Task{task(1, "b"), task(2, "a"), task(3, "b"), task(4, "a")}, map[string]int{"a": 1, "b": 2}, []int64{2, 4, 1, 3}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []int64
			for _, i := range ByPriority(c.tasks, c.prio) {
				got = append(got, c.tasks[i].ID)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("order = %v, want %v", got, c.want)
			}
		})
	}
}

// With one slot across windows, the window at priority 1 starts first,
// though its task was filed after the other's.
func TestRunner_PriorityTakesTheLastSlot(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.r.TotalSlots = func() (int, error) { return 1, nil }
	g.r.WindowPriorities = func() (map[string]int, error) { return map[string]int{"old": 2, "new": 1}, nil }
	g.feature(1, 0, "app")
	g.feature(2, 0, "blog")
	g.inWindow(1, "old")
	g.inWindow(2, "new")
	g.tick()
	if g.st.task(1).Status != Waiting || g.st.task(2).Status != Running {
		t.Fatalf("statuses = %s %s, want waiting running", g.st.task(1).Status, g.st.task(2).Status)
	}
}

// A window first in line with no slot of its own free is still skipped:
// the next window gets the slot.
func TestRunner_FullPriorityWindowPassesTheSlotOn(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.r.TotalSlots = func() (int, error) { return 1, nil }
	g.r.WindowSlots = func() (map[string]int, error) { return map[string]int{"first": 0}, nil }
	g.r.WindowPriorities = func() (map[string]int, error) { return map[string]int{"first": 1, "second": 2}, nil }
	g.feature(1, 0, "app")
	g.feature(2, 0, "blog")
	g.inWindow(1, "first")
	g.inWindow(2, "second")
	g.tick()
	if g.st.task(1).Status != Waiting || g.st.task(2).Status != Running {
		t.Fatalf("statuses = %s %s, want waiting running", g.st.task(1).Status, g.st.task(2).Status)
	}
}

// Unreadable priorities do not stop the queue: it runs in queue order.
func TestRunner_UnreadablePrioritiesRunInQueueOrder(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.r.TotalSlots = func() (int, error) { return 1, nil }
	g.r.WindowPriorities = func() (map[string]int, error) { return nil, errors.New("database is locked") }
	g.feature(1, 0, "app")
	g.feature(2, 0, "blog")
	g.inWindow(1, "old")
	g.inWindow(2, "new")
	g.tick()
	if g.st.task(1).Status != Running || g.st.task(2).Status != Waiting {
		t.Fatalf("statuses = %s %s, want running waiting", g.st.task(1).Status, g.st.task(2).Status)
	}
}

// Of two built features, the one in the window at priority 1 merges first.
func TestMerge_PriorityMergesFirst(t *testing.T) {
	g := newRig(t)
	g.r.WindowPriorities = func() (map[string]int, error) { return map[string]int{"low": 2, "high": 1}, nil }
	g.feature(1, 0, "app")
	g.feature(2, 0, "backend")
	g.inWindow(1, "low")
	g.inWindow(2, "high")
	g.tick()
	g.doneParts(1)
	g.doneParts(2)
	g.checks.hold = make(chan struct{})
	g.r.Tick()
	g.r.Tick()
	first, second := g.st.task(1).Status, g.st.task(2).Status
	close(g.checks.hold)
	g.r.merges.Wait()
	if first != ReadyToMerge || second != Merging {
		t.Fatalf("while one merges: low %s, high %s; want the high one merging", first, second)
	}
}
