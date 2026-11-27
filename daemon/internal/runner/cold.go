package runner

import (
	"context"
	"encoding/json"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// Cold says a session could end now and lose nothing a next message would
// not have paid for anyway: its last turn ended longer ago than its prompt
// cache lives, and nothing waits on it (no turn, no card, no message not yet
// delivered, no background task). A session that has not finished a turn
// yet is never cold: it has not answered what started it. One that does
// not answer is not cold either, unless it is asleep, which has no turn to
// cut. A main session's side helper ends on this (internal/threads
// sideexpire.go).
func (s *Supervisor) Cold(id string) bool { return s.quiet(id, true) }

// Finished is Cold without the wait for the cache: a turn has ended and
// nothing waits on the session. A repo's planner with an empty line ends
// on this (api.PlannerPass): its work is done, and keeping it for a cache
// it is unlikely to use again only kept it on the page.
func (s *Supervisor) Finished(id string) bool { return s.quiet(id, false) }

// quiet is Cold (cache: the cache must have run out too) or Finished.
func (s *Supervisor) quiet(id string, cache bool) bool {
	sess, err := s.row(id)
	if err != nil || sess.Wanted == store.SessionDone {
		return false
	}
	if s.refreshHeldByRows(id) != "" {
		return false
	}
	if sess.Wanted == store.SessionAsleep {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), refreshAsk)
	defer cancel()
	c := s.dial(s.paths.Socket(id))
	h, err := c.Hello(ctx)
	if err != nil || h.Busy || h.TurnOpen {
		return false
	}
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return false
	}
	return snapshotQuiet(snap.Body, s.now(), cache)
}

// snapshotQuiet reads a runner's snapshot: quiet when nothing in it holds a
// restart (snapshotHold; a warm cache counts only when cache is set) and a
// turn has ended.
func snapshotQuiet(body json.RawMessage, now time.Time, cache bool) bool {
	if hold := snapshotHold(body, now); hold != "" && (cache || hold != holdWarm) {
		return false
	}
	var snap struct {
		Result *struct {
			EndedAt int64 `json:"endedAt"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &snap); err != nil {
		return false
	}
	return snap.Result != nil && snap.Result.EndedAt != 0
}
