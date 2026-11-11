package taskq

import (
	"log"

	"ccmux.dev/ccmuxd/internal/store"
)

// slotBook is one pass's count of slots: what each window holds and may
// hold, and what the whole daemon holds and may hold. A feature starts only
// where both its window and the daemon have room. A nil book means the
// numbers were unreadable: nothing takes a slot that pass.
type slotBook struct {
	held  map[string]int          // features holding a slot, per window (SlotsHeld)
	total int                     // features holding a slot, every window
	limit func(window string) int // the window's own number, or Runner.Slots
	cap   int                     // the most at once across windows; 0 for none
}

// free reports whether window w may start one more feature now.
func (b *slotBook) free(w string) bool {
	if b == nil {
		return false
	}
	return b.held[w] < b.limit(w) && (b.cap == 0 || b.total < b.cap)
}

// take counts a feature just started in window w.
func (b *slotBook) take(w string) {
	b.held[w]++
	b.total++
}

// slotBook reads every window's own number and the cap across windows once
// per pass and counts what tasks and parts already hold. It returns nil,
// logged, when either number is unreadable.
func (r *Runner) slotBook(tasks []store.Task, parts []store.Part) *slotBook {
	var own map[string]int
	if r.WindowSlots != nil {
		var err error
		if own, err = r.WindowSlots(); err != nil {
			log.Printf("tasks: window slots unreadable, nothing new starts this pass: %v", err)
			return nil
		}
	}
	b := &slotBook{held: SlotsHeld(tasks, parts, r.parked)}
	if r.TotalSlots != nil {
		var err error
		if b.cap, err = r.TotalSlots(); err != nil {
			log.Printf("tasks: the cap across windows is unreadable, nothing new starts this pass: %v", err)
			return nil
		}
	}
	for _, n := range b.held {
		b.total += n
	}
	b.limit = func(w string) int {
		if n, ok := own[w]; ok {
			return n
		}
		return r.Slots
	}
	return b
}
