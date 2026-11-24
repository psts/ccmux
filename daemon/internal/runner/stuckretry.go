package runner

import (
	"fmt"
	"log"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// Trying a stuck session again by itself. A session the daemon gave up
// starting (park: the start limit, starts that failed before systemd saw
// them, walls that refused) sleeps marked stuck, and a prompt or a wake
// tries it again. A cause that has passed (a deploy that swapped the
// binary under five quick starts, seen live 2026-11-20 on x-publish) would
// leave it asleep until then. So it tries again by itself, later each time
// (stuckBackoff), as long as it has work: a person's session always, a
// role agent only with a message waiting (it sleeps between messages
// anyway). A task's builder is left to its queue, which fails the part.

// stuckBackoff is how long a stuck session waits before each try; the
// last one repeats.
var stuckBackoff = []time.Duration{10 * time.Minute, 30 * time.Minute, time.Hour}

// stuckFresh is how long after its last try a session counts its tries
// from the first again.
const stuckFresh = 6 * time.Hour

// stuckTries is the in-memory part: when each stuck session was first seen
// stuck since its last try, and how many tries it has had. Losing it (a
// daemon restart) only starts the count over.
type stuckTries struct {
	mu   sync.Mutex
	seen map[string]stuckState
}

type stuckState struct {
	since time.Time // seen stuck since
	last  time.Time // its last try
	tries int
}

// due says whether id, stuck now, waits no longer; it starts the wait the
// first time it is seen stuck.
func (t *stuckTries) due(id string, now time.Time) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen == nil {
		t.seen = map[string]stuckState{}
	}
	st, ok := t.seen[id]
	if !ok || st.since.IsZero() {
		if ok && now.Sub(st.last) > stuckFresh {
			st.tries = 0
		}
		st.since = now
		t.seen[id] = st
	}
	wait := stuckBackoff[min(st.tries, len(stuckBackoff)-1)]
	return st.tries + 1, !now.Before(st.since.Add(wait))
}

// forget drops what is kept of a session that is no longer stuck, keeping
// only when it was last tried (for stuckFresh): a later park starts a
// fresh wait.
func (t *stuckTries) forget(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if st, ok := t.seen[id]; ok && !st.since.IsZero() {
		st.since = time.Time{}
		t.seen[id] = st
	}
}

// tried records a try: the next wait starts when it is seen stuck again.
func (t *stuckTries) tried(id string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.seen[id]
	st.tries++
	st.last, st.since = now, time.Time{}
	t.seen[id] = st
}

// retryStuck tries again each stuck session that has work and has waited
// its turn. The next reconcile starts it.
func (s *Supervisor) retryStuck(list []store.RunnerSession) {
	now := s.now()
	for _, sess := range list {
		if !sess.Stuck || sess.Wanted != store.SessionAsleep {
			s.stuck.forget(sess.ID) // woken some other way: its next park waits afresh
			continue
		}
		try, ok := s.stuck.due(sess.ID, now) // its wait starts when it is first seen stuck, work or not
		if !ok || !s.stuckHasWork(sess) {
			continue
		}
		woke, err := s.retryOne(sess.ID, try)
		if err != nil {
			log.Printf("runner: session %s: stuck, trying again failed: %v", short(sess.ID), err)
		}
		if woke {
			s.stuck.tried(sess.ID, now)
		}
	}
}

// stuckHasWork says whether a stuck session is worth trying again by
// itself (see above). Messages that cannot be read count as none: the
// next pass asks again.
func (s *Supervisor) stuckHasWork(sess store.RunnerSession) bool {
	switch {
	case sess.Role == store.RoleBuilder:
		return false
	case sess.Kind != store.SessionAgent:
		return true
	}
	open, err := s.st.UndeliveredPrompts(sess.ID)
	if err != nil {
		log.Printf("runner: session %s: stuck, its messages not read: %v", short(sess.ID), err)
		return false
	}
	return len(open) > 0
}

// retryOne wakes a session that is still stuck, as a person's wake would,
// says so in its note, and says whether it woke it. A wake whose note
// alone was not saved still woke it.
func (s *Supervisor) retryOne(id string, try int) (bool, error) {
	defer s.lock(id)()
	sess, err := s.row(id)
	if err != nil || !sess.Stuck || sess.Wanted != store.SessionAsleep {
		return false, err
	}
	err = s.wake(id)
	if now, rerr := s.row(id); rerr != nil || now.Wanted != store.SessionRunning {
		return false, err
	}
	log.Printf("runner: session %s: stuck, trying again by itself (try %d)", short(id), try)
	s.note(id, fmt.Sprintf("trying again by itself after it could not start (try %d)", try))
	s.announce(id)
	return true, err
}
