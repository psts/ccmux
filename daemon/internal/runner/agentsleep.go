package runner

import (
	"errors"
	"log"

	"ccmux.dev/ccmuxd/internal/store"
	"github.com/google/uuid"
)

// A role agent's sleep, and the conversation it wakes in. A base says
// whether its agent continues its conversation or starts fresh each time it
// is started. In a tmux pane each start was a new process and so, by
// default, a new conversation. A session's row lives on across sleeps, so
// the fresh start is a mark on the row: the sleep sets it, the next start
// gives the session a new conversation and clears it. Whatever starts the
// session then (a message from the bus, a schedule, a click) gets the same
// answer, and a crash, a planned restart or a reboot, which never slept it,
// resume the conversation that was running.

// ErrPromptWaiting refuses a sleep while a prompt for the session has not
// reached its Claude: a prompt only wakes a session that is asleep when it
// is queued, so one queued a moment before the sleep would wait there until
// something else woke the session.
var ErrPromptWaiting = errors.New("a message for this session has not reached it yet; it reads that first")

// Sleep puts a session to sleep, and says whether its next start opens a
// new conversation. The unit is stopped here, before the row says asleep:
// a wake that follows then always finds it down and starts it, which is
// where the new conversation is made. A session already asleep only takes
// the mark; an ended one is ErrSessionDone.
func (s *Supervisor) Sleep(sessionID string, fresh bool) error {
	defer s.lock(sessionID)()
	sess, err := s.row(sessionID)
	if err != nil {
		return err
	}
	switch sess.Wanted {
	case store.SessionDone:
		return ErrSessionDone
	case store.SessionAsleep:
		return s.st.SetRunnerSessionFresh(sessionID, fresh, s.millis())
	}
	waiting, err := s.st.UndeliveredPrompts(sessionID)
	if err != nil {
		return err
	}
	if len(waiting) > 0 {
		return ErrPromptWaiting
	}
	if err := s.units.Stop(s.paths.Unit(sessionID)); err != nil {
		return err
	}
	if err := s.st.SleepRunnerSession(sessionID, fresh, s.millis()); err != nil {
		return err
	}
	s.announce(sessionID)
	return nil
}

// SetFresh says whether an asleep session's next start opens a new
// conversation, for a start that says so itself (a person choosing
// "continue" or "start over"). A running session keeps its conversation.
func (s *Supervisor) SetFresh(sessionID string, fresh bool) error {
	defer s.lock(sessionID)()
	sess, err := s.row(sessionID)
	if err != nil {
		return err
	}
	if sess.Wanted != store.SessionAsleep {
		return nil
	}
	return s.st.SetRunnerSessionFresh(sessionID, fresh, s.millis())
}

// freshen gives a session marked for a fresh start its new conversation,
// and returns the row as it now is. The mark is cleared in the same write,
// so a start that fails and is tried again does not open a second one.
func (s *Supervisor) freshen(sess store.RunnerSession) (store.RunnerSession, error) {
	if !sess.FreshNext {
		return sess, nil
	}
	conv := uuid.NewString()
	if err := s.st.RotateRunnerConversation(sess.ID, conv, s.millis()); err != nil {
		return sess, err
	}
	log.Printf("runner: session %s: starts a new conversation (%s)", short(sess.ID), short(conv))
	sess.Conversation, sess.FreshNext = conv, false
	return sess, nil
}

// keepAgentVersion records the base version an agent's session is about to
// start with, and tells whoever follows the row when it changed: the
// session's pane carries it, and a lens reads "the base moved on" from it.
func (s *Supervisor) keepAgentVersion(sess store.RunnerSession, version string) error {
	if sess.Kind != store.SessionAgent || version == sess.AgentVersion {
		return nil
	}
	if err := s.st.SetRunnerSessionAgentVersion(sess.ID, version); err != nil {
		return err
	}
	s.announce(sess.ID)
	return nil
}
