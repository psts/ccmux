package taskq

import (
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"ccmux.dev/ccmuxd/internal/store"
)

// The outbox. What the queue has to tell a worker about a failed merge is a
// row (store.TaskNotice), written in the same step as the rows that send
// the part back (sendBack). It is delivered from that row: at once, and
// then on every pass until the bus has taken it. So a daemon that stops in
// between loses nothing. And it cannot say a thing twice: the notice has a
// fixed id, the bus keeps one message per id, and a notice the bus took is
// only then written down as sent.

// NoticeLimit is how long a notice is tried before the part asks for a
// person: far longer than a worker's Claude needs to be back on the bus
// after a daemon restart (seconds), so what is left is one that will never
// hear it (its Claude runs without the bus). It counts from this daemon's
// own first failed try (Runner.missed), not from when the notice was made:
// a daemon that was down for an hour has not tried for an hour.
const NoticeLimit = 10 * time.Minute

// keepNotices is how long an ended notice's row is kept, to look into.
const keepNotices = 14 * 24 * time.Hour

// noteUntold is a sent-back part's note when its worker never heard why.
const noteUntold = "sent back after a failed merge, but its worker could not be told why; open its session and tell it (the task's note has the reason)"

// noticeID is the fixed id of the notice that sends part p back. From a
// merge job's own end it is made from the job and the part, so the same
// failure is the same message however often the job's end is acted on. A
// send-back with no job behind it (a person's, or one that waited for a
// slot) gets an id of its own: what must stay fixed is the id of one row
// of the outbox across its deliveries, and the row keeps it.
func noticeID(p store.Part, jobID string) string {
	if jobID == "" {
		jobID = uuid.NewString()
	}
	return fmt.Sprintf("%s:part-%d:send-back", jobID, p.ID)
}

// tell delivers one notice and writes it down as sent. A notice that was
// delivered and not written down is delivered again later, which the bus
// takes as said (its id is in the log), and is written down then.
func (r *Runner) tell(n store.TaskNotice) error {
	if err := r.Host.TellWorker(n.PaneID, n.ID, n.Text); err != nil {
		return err
	}
	if err := r.Store.EndTaskNotice(n.ID, r.Now().UnixMilli(), ""); err != nil {
		log.Printf("tasks: #%d: its worker was told and the notice is not marked as sent, so it is said again (and heard once): %v", n.TaskID, err)
	}
	return nil
}

// deliverNotices tries every waiting notice once. It returns the parts
// whose worker has still not been told (the watcher leaves those alone),
// and whether it changed a part: a notice nobody took for NoticeLimit is
// given up on, and its part asks for a person. Under the lock; parts is
// updated in place.
func (r *Runner) deliverNotices(parts []store.Part) (untold map[int64]bool, changed bool) {
	waiting, err := r.Store.WaitingTaskNotices()
	if err != nil {
		log.Printf("tasks: the notices to workers were not read, none is delivered this pass: %v", err)
		return nil, false
	}
	untold = map[int64]bool{}
	missed := map[string]time.Time{}
	for _, n := range waiting {
		i := partIndex(parts, n)
		if i < 0 {
			r.endNotice(n, "its part is no longer waiting to hear it")
			continue
		}
		_, missedBefore := r.missed[n.ID]
		err := r.tell(n)
		if err != nil && !missedBefore {
			log.Printf("tasks: #%d: its worker (pane %s) did not take a notice; it is tried again on every pass for %s: %v", n.TaskID, n.PaneID, NoticeLimit, err)
		}
		switch {
		case err == nil:
		case r.Now().Sub(r.missedSince(n.ID)) <= NoticeLimit:
			untold[n.PartID] = true
			missed[n.ID] = r.missed[n.ID]
		default:
			changed = r.giveUp(n, &parts[i], err) || changed
		}
	}
	r.missed = missed // only the notices that still wait: the rest are over
	return untold, changed
}

// missedSince is when this daemon first failed to deliver a notice: now,
// the first time it is asked. Under the lock.
func (r *Runner) missedSince(id string) time.Time {
	if at, ok := r.missed[id]; ok {
		return at
	}
	if r.missed == nil {
		r.missed = map[string]time.Time{}
	}
	r.missed[id] = r.Now()
	return r.missed[id]
}

// partIndex finds the part a notice is for, as long as the notice still
// holds: the part is at work in the pane the notice names. -1 for one that
// was cancelled, retried or removed since.
func partIndex(parts []store.Part, n store.TaskNotice) int {
	for i, p := range parts {
		if p.ID == n.PartID && p.Status == Running && p.PaneID == n.PaneID {
			return i
		}
	}
	return -1
}

// giveUp ends a notice nobody took in time, and hands its part to a
// person, with the reason in the log and on the notice's row.
func (r *Runner) giveUp(n store.TaskNotice, p *store.Part, cause error) bool {
	log.Printf("tasks: #%d part in %s: its worker never heard why its merge failed (%v); it waits for a person", n.TaskID, p.RepoPath, cause)
	r.endNotice(n, cause.Error())
	next := *p
	next.Status, next.Note = NeedsYou, noteUntold
	if !r.savePart(next) {
		return false
	}
	*p = next
	return true
}

func (r *Runner) endNotice(n store.TaskNotice, why string) {
	if err := r.Store.EndTaskNotice(n.ID, 0, why); err != nil {
		log.Printf("tasks: #%d: a notice that ended (%s) still reads as waiting: %v", n.TaskID, why, err)
	}
}

// pruneNotices removes the rows of notices that ended long ago.
func (r *Runner) pruneNotices() {
	if err := r.Store.PruneTaskNotices(r.Now().Add(-keepNotices).UnixMilli()); err != nil {
		log.Printf("tasks: old notices to workers were not removed: %v", err)
	}
}
