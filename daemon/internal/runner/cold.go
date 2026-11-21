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
func (s *Supervisor) Cold(id string) bool {
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
	return snapshotCold(snap.Body, s.now())
}

// snapshotCold reads a runner's snapshot: cold when nothing in it holds a
// restart (snapshotHold) and a turn has ended, its cache's lifetime ago.
func snapshotCold(body json.RawMessage, now time.Time) bool {
	if snapshotHold(body, now) != "" {
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
