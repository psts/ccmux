package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// Refreshing a long-running session. A session reads Claude Code, the SDK,
// the person's plugins, skills and CLAUDE.md, and its MCP servers when it
// starts, and a repo's lead can run for days without one. So a session
// that has run longer than Config.RefreshAfter is started again, on the
// same conversation, the first time it is truly idle; so is one whose
// skills or plugins (Config.StaleLoadouts), or Claude Code, the SDK or
// the runner's own code (Config.RunsNow), changed since it started, once
// per change, however young: no turn, no card, no message waiting, no background task, and its prompt cache already cold,
// so the restart costs nothing a next message would not have paid anyway.
//
// What decides is outside the daemon's memory, since every deploy restarts
// the daemon: the unit's own start time (systemd) and the last turn's end
// (the runner's snapshot). One session at a time: the next waits until the
// last one restarted answers, so a bad update stops at the first session.
// One that does not answer within refreshWait says so in its note, and
// the refreshes stay stopped until it answers or is ended. Asleep is not
// enough: a session that keeps failing to start is put to sleep by the
// supervisor itself (park), which is just what a broken update does.
//
// Left out: a role agent (its base's idle exit starts it fresh, and its
// sleep has one door, agentsleep.go) and a task's builder (a restart kills
// its dev servers, and it ends with its task).

const (
	// refreshRecheck is how long a session found busy, warm or working
	// in the background is left before it is looked at again.
	refreshRecheck = 10 * time.Minute
	// refreshWait is how long a restarted session has to answer before
	// its note says the refreshes stopped on it.
	refreshWait = 5 * time.Minute
	// refreshAsk bounds each question put to a runner.
	refreshAsk = 10 * time.Second
	// defaultCacheTTL is the cache lifetime of a turn that did not say.
	defaultCacheTTL = 5 * time.Minute
)

// refresher is the in-memory part: which session was just restarted, and
// when each other one is next worth asking. Losing it (a daemon restart)
// costs a few early questions, never a wrong restart.
type refresher struct {
	mu        sync.Mutex
	waiting   string
	waitUntil time.Time
	told      bool                 // the waiting session's note says the refreshes stopped on it
	next      map[string]time.Time // held: not asked again before this
	aged      map[string]time.Time // young: not due by its age before this
	// staleDone are the sessions restarted on a change (skills, plugins,
	// Claude Code, the SDK) that have not been seen up to date since: still
	// out of date after that restart, another would not help, so they wait
	// for their age.
	staleDone map[string]bool
}

// ageLater leaves id's age alone until at.
func (r *refresher) ageLater(id string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.aged == nil {
		r.aged = map[string]time.Time{}
	}
	r.aged[id] = at
}

func (r *refresher) agedNotBefore(id string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return now.Before(r.aged[id])
}

// staleTried says whether id was restarted on a change and not
// seen up to date since; markStale records such a restart (on true) or
// that it is up to date again (false).
func (r *refresher) staleTried(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.staleDone[id]
}

func (r *refresher) markStale(id string, tried bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !tried {
		delete(r.staleDone, id)
		return
	}
	if r.staleDone == nil {
		r.staleDone = map[string]bool{}
	}
	r.staleDone[id] = true
}

// later leaves id alone until at.
func (r *refresher) later(id string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.next == nil {
		r.next = map[string]time.Time{}
	}
	r.next[id] = at
}

func (r *refresher) notBefore(id string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return now.Before(r.next[id])
}

// refreshPass restarts at most one session that is due and idle.
func (s *Supervisor) refreshPass(ctx context.Context, list []store.RunnerSession) {
	if s.cfg.RefreshAfter <= 0 || s.refreshWaiting(ctx) {
		return
	}
	changed := s.changeCheck()
	for _, sess := range list {
		why := s.refreshDue(sess, changed)
		if why == "" {
			continue
		}
		if s.refreshIfIdle(ctx, sess.ID, why) {
			return
		}
	}
}

// changeCheck is this pass's check of what changed under a session since
// it started (its skills and plugins, Claude Code, the SDK or the runner's
// own code): why, or "".
// Nil when there is nothing to check against.
func (s *Supervisor) changeCheck() func(store.RunnerSession) string {
	var stale func(store.RunnerSession) bool
	if s.cfg.StaleLoadouts != nil {
		stale = s.cfg.StaleLoadouts()
	}
	runs := ""
	if s.cfg.RunsNow != nil {
		runs = s.cfg.RunsNow()
	}
	if stale == nil && runs == "" {
		return nil
	}
	return func(sess store.RunnerSession) string {
		if stale != nil && stale(sess) {
			return "its skills or plugins changed"
		}
		if runs != "" && s.startedOnOther(sess.ID, runs) {
			return "Claude Code, the SDK or ccmux's session runner was updated"
		}
		return ""
	}
}

// startedOnOther says whether the session's spec marks another Claude
// Code, SDK or runner code than runs. A spec that cannot be read, or marks nothing (an
// older daemon wrote it), says no: its age refreshes it.
func (s *Supervisor) startedOnOther(id, runs string) bool {
	sp, err := ReadSpec(s.paths.SpecFile(id))
	return err == nil && sp.Runs != "" && sp.Runs != runs
}

// refreshWaiting says whether the refreshes wait on the last restarted
// session: until it answers, or is ended.
func (s *Supervisor) refreshWaiting(ctx context.Context) bool {
	s.refresh.mu.Lock()
	id, until, told := s.refresh.waiting, s.refresh.waitUntil, s.refresh.told
	s.refresh.mu.Unlock()
	if id == "" {
		return false
	}
	if s.refreshAnswered(ctx, id) {
		s.refresh.mu.Lock()
		s.refresh.waiting, s.refresh.told = "", false
		s.refresh.mu.Unlock()
		return false
	}
	if !told && !s.now().Before(until) {
		s.note(id, fmt.Sprintf("did not come back within %s of its restart for updates; no other session is restarted for updates until it does (an update may be broken)", refreshWait))
		s.announce(id)
		s.refresh.mu.Lock()
		s.refresh.told = true
		s.refresh.mu.Unlock()
	}
	return true
}

// refreshAnswered says whether the waiting session is past waiting for:
// it answers, or it was ended. An asleep one is still waited for: it may
// have been parked for failing to start on the update.
func (s *Supervisor) refreshAnswered(ctx context.Context, id string) bool {
	sess, err := s.row(id)
	if errors.Is(err, store.ErrNoSession) || (err == nil && sess.Wanted == store.SessionDone) {
		return true
	}
	ask, cancel := context.WithTimeout(ctx, refreshAsk)
	defer cancel()
	_, err = s.dial(s.paths.Socket(id)).Hello(ask)
	return err == nil
}

// refreshDue is the cheap part: why a running person's session, not
// asked about lately, should be started again ("" when it should not):
// something changed under it (refreshChanged), or its unit has run
// longer than RefreshAfter. One that is not old yet is not asked about
// its age again until it will be.
func (s *Supervisor) refreshDue(sess store.RunnerSession, changed func(store.RunnerSession) string) string {
	if sess.Wanted != store.SessionRunning || sess.Kind == store.SessionAgent || sess.Role == store.RoleBuilder {
		return ""
	}
	now := s.now()
	if s.refresh.notBefore(sess.ID, now) {
		return ""
	}
	if why := s.refreshChanged(sess, changed); why != "" {
		return why
	}
	if s.refresh.agedNotBefore(sess.ID, now) {
		return ""
	}
	since, err := s.units.Since(s.paths.Unit(sess.ID))
	if err != nil || since.IsZero() {
		return "" // not running now: reconcile starts it, which starts its clock
	}
	if due := since.Add(s.cfg.RefreshAfter); now.Before(due) {
		s.refresh.ageLater(sess.ID, due)
		return ""
	}
	return fmt.Sprintf("it had run %s", s.cfg.RefreshAfter)
}

// refreshChanged is why the session should start again on what changed
// under it, "" when nothing did or a restart was already tried on it: one
// still out of date after its restart waits for its age.
func (s *Supervisor) refreshChanged(sess store.RunnerSession, changed func(store.RunnerSession) string) string {
	if changed == nil {
		return ""
	}
	why := changed(sess)
	if why == "" {
		s.refresh.markStale(sess.ID, false)
		return ""
	}
	if s.refresh.staleTried(sess.ID) {
		return ""
	}
	return why
}

// refreshIfIdle restarts the session when nothing it does would be lost,
// and says whether it did. why is what made it due (refreshDue).
func (s *Supervisor) refreshIfIdle(ctx context.Context, id, why string) bool {
	defer s.lock(id)()
	sess, err := s.row(id)
	if err != nil || sess.Wanted != store.SessionRunning {
		return false
	}
	if hold := s.refreshHold(ctx, id); hold != "" {
		s.refresh.later(id, s.now().Add(refreshRecheck))
		return false
	}
	if err := s.restart(*sess); err != nil {
		log.Printf("runner: session %s: refresh restart failed, tried again later: %v", short(id), err)
		s.refresh.later(id, s.now().Add(refreshRecheck))
		return false
	}
	s.refresh.markStale(id, true) // what it loads now is current, whatever made it due
	s.note(id, fmt.Sprintf("started again to pick up updates (Claude Code, plugins, skills) because %s; its conversation goes on", why))
	s.announce(id)
	s.refresh.mu.Lock()
	s.refresh.waiting, s.refresh.waitUntil = id, s.now().Add(refreshWait)
	s.refresh.mu.Unlock()
	return true
}

// refreshHold is why the session must not be restarted now, "" when it
// may: cheapest questions first, its full snapshot last.
func (s *Supervisor) refreshHold(ctx context.Context, id string) string {
	ask, cancel := context.WithTimeout(ctx, refreshAsk)
	defer cancel()
	c := s.dial(s.paths.Socket(id))
	h, err := c.Hello(ask)
	switch {
	case err != nil:
		return "not answering"
	case h.Busy || h.TurnOpen:
		return "in a turn"
	}
	if why := s.refreshHeldByRows(id); why != "" {
		return why
	}
	snap, err := c.Snapshot(ask)
	if err != nil {
		return "no snapshot"
	}
	return snapshotHold(snap.Body, s.now())
}

// refreshHeldByRows is what the registry says waits on the session: a card
// (a restart expires it) or a message not yet delivered.
func (s *Supervisor) refreshHeldByRows(id string) string {
	cards, err := s.st.OpenSessionCards(id)
	if err != nil || len(cards) > 0 {
		return "a card is open"
	}
	open, err := s.st.UndeliveredPrompts(id)
	if err != nil || len(open) > 0 {
		return "a message waits"
	}
	return ""
}

// holdWarm is snapshotHold's word for a warm cache, the one hold
// Finished does not wait for.
const holdWarm = "cache warm"

// snapshotHold reads a runner's snapshot: a background task (a restart
// kills it), a question or permission it shows, or a warm cache (a restart
// now would pay for it) holds the restart. A session with no finished turn since it started has no cache.
func snapshotHold(body json.RawMessage, now time.Time) string {
	var snap struct {
		Result *struct {
			EndedAt    int64 `json:"endedAt"`
			CacheTTLMs int64 `json:"cacheTtlMs"`
		} `json:"result"`
		Info struct {
			Tasks []json.RawMessage `json:"tasks"`
		} `json:"info"`
		Permissions []json.RawMessage `json:"permissions"`
		Questions   []json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(body, &snap); err != nil {
		return "snapshot unreadable"
	}
	if len(snap.Info.Tasks) > 0 {
		return "background task running"
	}
	if len(snap.Permissions)+len(snap.Questions) > 0 {
		return "a card is open"
	}
	if snap.Result == nil || snap.Result.EndedAt == 0 {
		return ""
	}
	ttl := time.Duration(snap.Result.CacheTTLMs) * time.Millisecond
	if ttl <= 0 {
		ttl = defaultCacheTTL
	}
	if now.Before(time.UnixMilli(snap.Result.EndedAt).Add(ttl)) {
		return holdWarm
	}
	return ""
}
