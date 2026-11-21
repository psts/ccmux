package runner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
	"github.com/google/uuid"
)

// A clean start (store/cleanstart.go): the person's message opens a new
// conversation instead of continuing one whose prompt cache has gone cold.
// It rides on the message itself (Prompt.Clean), so the move and the
// message happen under one hold of the session's lock: nothing can land
// in the old conversation between them.

// ErrCleanRefused is a clean start that cannot be made now, in its words.
var ErrCleanRefused = errors.New("no clean start")

// cleanAsk bounds asking a runner whether it is between turns.
const cleanAsk = 5 * time.Second

// cleanStart gives sess a new conversation when key asks for one, and
// starts its runner again on it. Called with the session's lock held. A
// key whose clean start was made already is a message sent again: it goes
// into the conversation that start made. An agent is refused (its base
// says when it starts fresh), and so is a session in the middle of a turn,
// which a restart would cut off.
func (s *Supervisor) cleanStart(sess *store.RunnerSession, key string) error {
	if key == "" || sess.LeftFor(key) {
		return nil
	}
	if sess.Kind == store.SessionAgent {
		return fmt.Errorf("%w: an agent starts fresh by its own base's rule", ErrCleanRefused)
	}
	running := sess.Wanted != store.SessionAsleep
	if running && s.busyNow(sess.ID) {
		return fmt.Errorf("%w: Claude is working right now; send it again when the turn has ended", ErrCleanRefused)
	}
	conv, now := uuid.NewString(), s.millis()
	left := store.LeftConversation{ID: sess.ConversationID(), At: now, Key: key}
	if err := s.st.StartCleanConversation(sess.ID, left, conv, now); err != nil {
		return err
	}
	sess.Conversation, sess.FreshNext, sess.Left = conv, false, append(sess.Left, left)
	log.Printf("runner: session %s: a clean start, from %s to %s", short(sess.ID), short(left.ID), short(conv))
	if s.CleanStarted != nil {
		// Off this session's lock: what it hears takes other locks (a side
		// thread's), and a person's stand-down holds that thread's lock while
		// it sends this session a notice, which waits for this one.
		go s.CleanStarted(*sess)
	}
	s.announce(sess.ID)
	if running {
		s.restartClean(*sess)
	}
	return nil // asleep: the message wakes it, and its start reads the new conversation
}

// StartOver gives a session a new conversation with no message: its
// person's "Start over" (and /clear typed on the thread page). The old
// conversation is kept above a divider, as a clean start's is, and a restart
// later comes back on the new one, which Claude Code's own /clear does not
// (it switches conversation inside the runner, and the daemon resumes the
// old one at the next start). Refused like a clean start: an agent, or a
// session in the middle of a turn.
func (s *Supervisor) StartOver(sessionID string) error {
	defer s.lock(sessionID)()
	sess, err := s.row(sessionID)
	if err != nil {
		return err
	}
	if sess.Wanted == store.SessionDone {
		return ErrSessionDone
	}
	return s.cleanStart(sess, "over-"+uuid.NewString())
}

// busyNow says whether a session's runner is in the middle of a turn. One
// that does not answer is starting or down, and has no turn to cut.
func (s *Supervisor) busyNow(sessionID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), cleanAsk)
	defer cancel()
	h, err := s.dial(s.paths.Socket(sessionID)).Hello(ctx)
	return err == nil && h.Busy
}

// restartClean starts the runner again on the row's new conversation. One
// that did not restart is marked for a restart between turns, which tend
// does before it delivers the inbox: the message waits for the new
// conversation rather than going into the old one.
func (s *Supervisor) restartClean(sess store.RunnerSession) {
	if err := s.restart(sess); err != nil {
		log.Printf("runner: session %s: clean start did not restart it (%v); restarting it at the next pass", short(sess.ID), err)
		s.due.Store(sess.ID, struct{}{})
		s.Kick()
	}
}
