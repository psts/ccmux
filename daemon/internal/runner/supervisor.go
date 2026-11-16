package runner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
	"github.com/google/uuid"
)

// Store is the part of the registry the supervisor reads and writes
// (store/sessions.go).
type Store interface {
	AddRunnerSession(store.RunnerSession) error
	RunnerSession(id string) (*store.RunnerSession, error)
	RunnerSessions() ([]store.RunnerSession, error)
	SetRunnerSessionWanted(id, wanted string, now int64) error
	SetRunnerSessionModel(id, model string, now int64) error
	SetRunnerSessionNote(id, note string, now int64) error
	AddInboxPromptAndWake(p store.InboxPrompt, wake bool, now int64) error
	HasInboxPrompt(id string) (bool, error)
	UndeliveredPrompts(sessionID string) ([]store.InboxPrompt, error)
	MarkPromptsDelivered(ids []string, now int64) error
	UpsertSessionCard(store.SessionCard) error
	OpenSessionCards(sessionID string) ([]store.SessionCard, error)
	SetSessionCardState(sessionID, id, state string, now int64) error
}

// Supervisor keeps every session at what its row wants. It holds no state
// a restart would lose: a new supervisor over the same rows picks up where
// the last one stopped.
//
// Every change to one session, from the API or from reconcile, runs under
// that session's lock and reads its row fresh, so a stop, a wake or a
// park never acts on a copy another change has already made stale.
type Supervisor struct {
	st    Store
	units Units
	paths Paths
	cfg   Config
	dial  func(socket string) *Client
	now   func() time.Time
	every time.Duration
	kick  chan struct{}
	loop  sync.Mutex // one reconcile pass at a time
	locks sync.Map   // session id → *sync.Mutex
	fails failures   // starts that failed before systemd saw them
	// soon tends one session off the caller's goroutine, right after a
	// prompt was queued for it (tendSoon); a test swaps it for nothing and
	// reconciles by hand.
	soon func(sessionID string)

	// Changed hears a session's row each time what it wants changes (made,
	// woken, put to sleep, ended), with the row as it now is. The daemon
	// keeps the session's pane in step with it (manager/runnerpanes.go).
	// Called with the session's lock held: it must not call back in. Set
	// before Run; nil hears nothing.
	Changed func(store.RunnerSession)
}

// New makes a supervisor. Run starts its loop.
func New(st Store, units Units, paths Paths, cfg Config) *Supervisor {
	s := &Supervisor{st: st, units: units, paths: paths, cfg: cfg, dial: Dial, now: time.Now, every: 10 * time.Second, kick: make(chan struct{}, 1), fails: failures{seen: map[string][]time.Time{}}}
	s.soon = s.tendSoon
	return s
}

// announce tells Changed what a session's row now says. A row that cannot
// be read is logged: the next change, or the daemon's next start, says it.
func (s *Supervisor) announce(sessionID string) {
	if s.Changed == nil {
		return
	}
	sess, err := s.row(sessionID)
	if err != nil {
		log.Printf("runner: session %s: its change was not passed on: %v", short(sessionID), err)
		return
	}
	s.Changed(*sess)
}

// lock takes one session's lock and returns its release.
func (s *Supervisor) lock(id string) func() {
	m, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// row reads a session fresh; a missing one is ErrNoSession.
func (s *Supervisor) row(id string) (*store.RunnerSession, error) {
	sess, err := s.st.RunnerSession(id)
	if err != nil || sess == nil {
		return nil, orNoSession(err)
	}
	return sess, nil
}

func (s *Supervisor) millis() int64 { return s.now().UnixMilli() }

// Run reconciles now, then on every tick and every kick, until ctx ends.
func (s *Supervisor) Run(ctx context.Context) {
	t := time.NewTicker(s.every)
	defer t.Stop()
	for {
		s.Reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
	}
}

// Kick asks for a reconcile soon, without waiting for it.
func (s *Supervisor) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// NewSession is what a session is made from. WorkspaceID is the repo
// workspace it belongs to; with one, the session gets a pane id, which is
// what puts it on the bus and behind the alerts, the hooks and the proxy.
// Without one it runs alone, as every session did before panes.
type NewSession struct {
	Cwd, Name, Model, WorkspaceID string
}

// Create records a new session and starts it. Once the row exists the
// session is the caller's: a first start that fails is said in its note
// and tried again by reconcile (up to the start limit), never reported as
// a failed create the caller would retry into a second session.
func (s *Supervisor) Create(n NewSession) (store.RunnerSession, error) {
	now := s.millis()
	sess := store.RunnerSession{ID: uuid.NewString(), Name: n.Name, Cwd: n.Cwd, Model: n.Model, Wanted: store.SessionRunning, CreatedAt: now, UpdatedAt: now, WorkspaceID: n.WorkspaceID}
	if n.WorkspaceID != "" {
		sess.PaneID = uuid.NewString()
	}
	defer s.lock(sess.ID)()
	if err := s.st.AddRunnerSession(sess); err != nil {
		return sess, err
	}
	// Before the start: the runner's first hook and its bus registration
	// name the pane, which must be there to take them.
	s.announce(sess.ID)
	if err := s.start(sess); err != nil {
		sess.Note = s.startFailed(sess, err)
	}
	return sess, nil
}

// Send queues a prompt for a session and has it delivered soon. An asleep
// session is woken by the same database write that queues the prompt, so
// either both happened or neither did: an error means nothing was queued
// and sending again is safe.
func (s *Supervisor) Send(sessionID, text, sender string) (string, error) {
	return s.SendAs(sessionID, uuid.NewString(), text, sender)
}

// SendAs is Send for a caller that names the prompt itself (a UUID, since
// it becomes the prompt's id in the transcript). The same id sent again
// queues nothing and is not an error: the bus names a message's prompt
// after the message and hands it over again after a crash, and it must
// run once.
func (s *Supervisor) SendAs(sessionID, promptID, text, sender string) (string, error) {
	defer s.lock(sessionID)()
	sess, err := s.row(sessionID)
	if err != nil {
		return "", err
	}
	if sess.Wanted == store.SessionDone {
		return "", ErrSessionDone
	}
	if had, err := s.st.HasInboxPrompt(promptID); err != nil || had {
		return promptID, err
	}
	asleep := sess.Wanted == store.SessionAsleep
	p := store.InboxPrompt{ID: promptID, SessionID: sessionID, Text: text, Sender: sender, CreatedAt: s.millis()}
	if err := s.st.AddInboxPromptAndWake(p, asleep, s.millis()); err != nil {
		return "", err
	}
	if asleep {
		s.freshStarts(sessionID)
		s.announce(sessionID)
	}
	s.Kick()
	s.soon(sessionID)
	return p.ID, nil
}

// tendSoon delivers one session's inbox now, on its own goroutine, rather
// than when the reconcile pass reaches it: the pass handles every session
// in turn, and one runner slow to answer would hold up a prompt for
// another. It waits for the session's lock like any other change; a
// session that is not up yet is left to the pass, which starts it.
func (s *Supervisor) tendSoon(sessionID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := s.reconcileID(ctx, sessionID); err != nil {
			log.Printf("runner: session %s: %v", short(sessionID), err)
		}
	}()
}

// SetWanted sets what a session should be at and acts on it soon. Ending
// stops the unit first and records done only once it stopped, so a failed
// stop leaves a row that still says running and reconcile still sees.
// Done is final: a done session never runs again; start a new one.
func (s *Supervisor) SetWanted(sessionID, wanted string) error {
	defer s.lock(sessionID)()
	sess, err := s.row(sessionID)
	if err != nil {
		return err
	}
	if sess.Wanted == store.SessionDone {
		if wanted == store.SessionDone {
			return nil // ending an ended session is no change
		}
		return ErrSessionDone
	}
	if err := s.want(sessionID, wanted); err != nil {
		return err
	}
	s.announce(sessionID)
	s.Kick()
	return nil
}

// want is SetWanted's change itself, for a session that has not ended.
func (s *Supervisor) want(sessionID, wanted string) error {
	switch wanted {
	case store.SessionDone:
		if err := s.units.Stop(s.paths.Unit(sessionID)); err != nil {
			return err
		}
	case store.SessionRunning:
		return s.wake(sessionID)
	}
	return s.st.SetRunnerSessionWanted(sessionID, wanted, s.millis())
}

// wake makes a session want to run with a fresh count of starts: systemd's
// start limit and this supervisor's failure count both reset, and the note
// that said why it slept is cleared.
func (s *Supervisor) wake(sessionID string) error {
	s.freshStarts(sessionID)
	if err := s.st.SetRunnerSessionWanted(sessionID, store.SessionRunning, s.millis()); err != nil {
		return err
	}
	return s.st.SetRunnerSessionNote(sessionID, "", s.millis())
}

// freshStarts clears both counts of failed starts, systemd's and this
// supervisor's, for a session that is being woken.
func (s *Supervisor) freshStarts(sessionID string) {
	if err := s.units.ResetFailed(s.paths.Unit(sessionID)); err != nil {
		log.Printf("runner: session %s: reset-failed: %v", short(sessionID), err)
	}
	s.fails.reset(sessionID)
}

// note records why the daemon acted on a session; a note that cannot be
// written is logged with what it would have said.
func (s *Supervisor) note(sessionID, text string) {
	log.Printf("runner: session %s: %s", short(sessionID), text)
	if err := s.st.SetRunnerSessionNote(sessionID, text, s.millis()); err != nil {
		log.Printf("runner: session %s: note not saved: %v", short(sessionID), err)
	}
}

// SetModel sets the model a session starts on. It applies at the
// session's next start, never to a turn already running: switching a live
// session's model waits until what the SDK does with a model it will not
// take is known. Restart is the "apply now" next to it.
func (s *Supervisor) SetModel(sessionID, model string) error {
	defer s.lock(sessionID)()
	sess, err := s.row(sessionID)
	if err != nil {
		return err
	}
	if sess.Wanted == store.SessionDone {
		return ErrSessionDone
	}
	return s.st.SetRunnerSessionModel(sessionID, model, s.millis())
}

// Restart starts a session again now, from its row as it is (its model,
// most of all), and resumes its conversation. A turn that was running is
// cut off, and the resumed session is told so and checks before redoing
// anything. An asleep session is woken, which starts it the same way. A
// session whose unit is down is started once: systemd's restart starts a
// stopped unit, so nothing starts it first.
func (s *Supervisor) Restart(sessionID string) error {
	defer s.lock(sessionID)()
	sess, err := s.row(sessionID)
	if err != nil {
		return err
	}
	switch sess.Wanted {
	case store.SessionDone:
		return ErrSessionDone
	case store.SessionAsleep:
		if err := s.wake(sessionID); err != nil {
			return err
		}
		s.announce(sessionID)
		s.Kick()
		return nil
	}
	return s.restart(*sess)
}

// Liveness says where a session is: running (it should run and its unit is
// up or coming up), asleep (put to sleep, or it should run and its unit is
// down until the next reconcile starts it), or done (ended, or no such
// session). The row says what should be, systemd what is. When either
// cannot be asked it is an error, never an answer: a registry that is
// locked for a moment must not read as a session that has ended.
func (s *Supervisor) Liveness(sessionID string) (string, error) {
	sess, err := s.row(sessionID)
	switch {
	case errors.Is(err, store.ErrNoSession):
		return store.SessionDone, nil
	case err != nil:
		return "", err
	case sess.Wanted != store.SessionRunning:
		return sess.Wanted, nil
	}
	active, err := s.units.Active(s.paths.Unit(sessionID))
	if err != nil {
		return "", err
	}
	if !active {
		return store.SessionAsleep, nil
	}
	return store.SessionRunning, nil
}

// ReplyCard answers a card a session's runner has pending (Client.ReplyCard).
// It changes no row, so it takes no lock: the runner settles the card, and
// the next reconcile records that.
func (s *Supervisor) ReplyCard(ctx context.Context, sessionID string, r CardReply) (bool, error) {
	return s.dial(s.paths.Socket(sessionID)).ReplyCard(ctx, r)
}

// ErrSessionDone is a prompt for a session that has ended: nothing would
// ever deliver it.
var ErrSessionDone = errors.New("this session has ended; start a new one")

func orNoSession(err error) error {
	if err != nil {
		return err
	}
	return store.ErrNoSession
}

// Reconcile brings every session to what it wants. One failing session is
// logged and the others go on. The list only names the sessions: each is
// handled under its own lock from its row read fresh at that moment.
func (s *Supervisor) Reconcile(ctx context.Context) {
	s.loop.Lock()
	defer s.loop.Unlock()
	list, err := s.st.RunnerSessions()
	if err != nil {
		log.Printf("runner: sessions unreadable: %v", err)
		return
	}
	for _, listed := range list {
		if err := s.reconcileID(ctx, listed.ID); err != nil {
			log.Printf("runner: session %s: %v", short(listed.ID), err)
		}
	}
}

func (s *Supervisor) reconcileID(ctx context.Context, id string) error {
	defer s.lock(id)()
	sess, err := s.row(id)
	if err != nil || sess.Wanted == store.SessionDone {
		return err
	}
	return s.reconcileOne(ctx, *sess)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (s *Supervisor) reconcileOne(ctx context.Context, sess store.RunnerSession) error {
	active, err := s.units.Active(s.paths.Unit(sess.ID))
	if err != nil {
		return err
	}
	if sess.Wanted != store.SessionRunning {
		if active {
			return s.units.Stop(s.paths.Unit(sess.ID))
		}
		return nil
	}
	if !active {
		return s.startOrPark(sess)
	}
	return s.tend(ctx, sess)
}

// startOrPark starts a session that should run. Past the cap it is put to
// sleep with a note instead, and a prompt or a wake brings it back. The cap
// counts both kinds of failed start: systemd's start limit (crashes, and
// starts systemd refused) and starts that failed before systemd saw them
// (no node, the sidecar would not install, the spec would not write).
func (s *Supervisor) startOrPark(sess store.RunnerSession) error {
	result, err := s.units.Result(s.paths.Unit(sess.ID))
	if err != nil {
		return err
	}
	if result == startLimitHit {
		return s.park(sess.ID, fmt.Sprintf("put to sleep: it stopped %d times in %d minutes; send it a prompt or wake it to try again", startLimitBurst, startLimitWindow/60))
	}
	if err := s.start(sess); err != nil {
		s.startFailed(sess, err)
		return err
	}
	return nil
}

// startFailed counts a start that failed before its runner came up and
// says so in the note; at the cap the session is put to sleep. It returns
// the note.
func (s *Supervisor) startFailed(sess store.RunnerSession, cause error) string {
	n := s.fails.add(sess.ID, s.now())
	if n >= startLimitBurst {
		text := fmt.Sprintf("put to sleep: it failed to start %d times in %d minutes (last: %v); send it a prompt or wake it to try again", n, startLimitWindow/60, cause)
		if err := s.park(sess.ID, text); err != nil {
			log.Printf("runner: session %s: %v", short(sess.ID), err)
		}
		return text
	}
	text := fmt.Sprintf("start failed (%d of %d), trying again: %v", n, startLimitBurst, cause)
	s.note(sess.ID, text)
	return text
}

func (s *Supervisor) park(id, text string) error {
	if err := s.st.SetRunnerSessionWanted(id, store.SessionAsleep, s.millis()); err != nil {
		return err
	}
	s.note(id, text)
	s.announce(id)
	return nil
}

// writeSpec writes the session's spec fresh from its row (the sidecar, the
// model or the session's own environment may have changed).
func (s *Supervisor) writeSpec(sess store.RunnerSession) error {
	sp, err := s.cfg.BuildSpec(sess, s.paths)
	if err != nil {
		return err
	}
	return WriteSpec(s.paths.SpecFile(sess.ID), sp)
}

// start writes the spec and starts the session's unit. The runner resumes
// its transcript itself.
func (s *Supervisor) start(sess store.RunnerSession) error {
	if err := s.writeSpec(sess); err != nil {
		return err
	}
	return s.units.Start(s.paths.Unit(sess.ID))
}

// restart writes the spec and restarts the session's unit, in that order:
// the new process must read the new spec, and a spec that cannot be built
// leaves the running one alone.
func (s *Supervisor) restart(sess store.RunnerSession) error {
	if err := s.writeSpec(sess); err != nil {
		return err
	}
	return s.units.Restart(s.paths.Unit(sess.ID))
}

// tend is a running session's upkeep: check its protocol, settle its
// cards, deliver its inbox. A runner whose socket is not up yet is left
// for the next tick.
func (s *Supervisor) tend(ctx context.Context, sess store.RunnerSession) error {
	c := s.dial(s.paths.Socket(sess.ID))
	h, err := c.Hello(ctx)
	if err != nil {
		return fmt.Errorf("not answering yet: %w", err)
	}
	s.cameUp(sess)
	if h.Protocol != Protocol {
		return s.restartStale(sess, h)
	}
	if err := s.syncCards(ctx, c, sess.ID, h.Boot); err != nil {
		return err
	}
	return s.flush(ctx, c, sess.ID)
}

// restartStale moves a runner on another protocol onto the installed one,
// between turns: a busy one is left to finish.
func (s *Supervisor) restartStale(sess store.RunnerSession, h Hello) error {
	if h.Busy {
		return fmt.Errorf("runner speaks protocol %d, this daemon %d; restarting it once its turn ends", h.Protocol, Protocol)
	}
	return s.restart(sess)
}

// flush delivers a session's inbox: prompts Claude already has are marked,
// the rest are sent in order. A prompt counts as delivered only once the
// runner reports it in the transcript, so one sent but lost in a crash is
// sent again, and the runner drops a resend it already has.
func (s *Supervisor) flush(ctx context.Context, c *Client, sessionID string) error {
	open, err := s.st.UndeliveredPrompts(sessionID)
	if err != nil || len(open) == 0 {
		return err
	}
	ids := make([]string, len(open))
	for i, p := range open {
		ids[i] = p.ID
	}
	have, err := c.Delivered(ctx, ids)
	if err != nil {
		return err
	}
	if err := s.st.MarkPromptsDelivered(have, s.millis()); err != nil {
		return err
	}
	return s.sendMissing(ctx, c, open, have)
}

func (s *Supervisor) sendMissing(ctx context.Context, c *Client, open []store.InboxPrompt, have []string) error {
	got := make(map[string]bool, len(have))
	for _, id := range have {
		got[id] = true
	}
	for _, p := range open {
		if got[p.ID] {
			continue
		}
		if _, err := c.Prompt(ctx, p.ID, p.Text); err != nil {
			return err
		}
	}
	return nil
}

// syncCards records the cards a runner has pending and settles the rest: a
// card from an earlier runner process expired with it; one from this
// process that is no longer pending was answered.
func (s *Supervisor) syncCards(ctx context.Context, c *Client, sessionID, boot string) error {
	pending, err := c.Cards(ctx)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for kind, cards := range pending {
		for _, card := range cards {
			live[card.ID] = true
			if err := s.st.UpsertSessionCard(store.SessionCard{ID: card.ID, SessionID: sessionID, Kind: kind, Body: string(card.Body), Boot: boot, CreatedAt: s.millis()}); err != nil {
				return err
			}
		}
	}
	return s.settleCards(sessionID, boot, live)
}

func (s *Supervisor) settleCards(sessionID, boot string, live map[string]bool) error {
	open, err := s.st.OpenSessionCards(sessionID)
	if err != nil {
		return err
	}
	var errs []error
	for _, card := range open {
		switch {
		case card.Boot != boot:
			errs = append(errs, s.st.SetSessionCardState(sessionID, card.ID, store.CardExpired, s.millis()))
		case !live[card.ID]:
			errs = append(errs, s.st.SetSessionCardState(sessionID, card.ID, store.CardAnswered, s.millis()))
		}
	}
	return errors.Join(errs...)
}

// cameUp is a runner answering: its failed starts are behind it, and a note
// about them is cleared.
func (s *Supervisor) cameUp(sess store.RunnerSession) {
	s.fails.reset(sess.ID)
	if sess.Note == "" {
		return
	}
	if err := s.st.SetRunnerSessionNote(sess.ID, "", s.millis()); err != nil {
		log.Printf("runner: session %s: note not cleared: %v", short(sess.ID), err)
	}
}

// failures counts each session's failed starts inside the start window.
// It is memory only: a daemon restart starts every count again, and
// systemd's own start limit still holds across it.
type failures struct {
	mu   sync.Mutex
	seen map[string][]time.Time
}

func (f *failures) add(id string, now time.Time) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	cut := now.Add(-startLimitWindow * time.Second)
	kept := []time.Time{now}
	for _, t := range f.seen[id] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	f.seen[id] = kept
	return len(kept)
}

func (f *failures) reset(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.seen, id)
}
