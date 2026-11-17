package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
)

// The thread page's stream of one runner session. A runner numbers the
// events it sends and keeps the newest ones; this relay reads them off the
// runner's socket and hands them to a browser, which says at connect which
// runner process it was reading (boot) and the last number it has (after).
// The same process and no gap: the stream goes on from there. Anything
// else (first connect, a restarted runner, events dropped): one snapshot of
// the conversation, then the events after it. So the page has every event
// once and in order across a daemon restart (the browser dials again with
// its numbers) and a runner restart (a new boot, so a new snapshot).
//
// Nothing is ever dropped to keep up: a browser that does not read makes a
// write fail, the socket closes, and its next connect continues from its
// numbers.

// SessionRunner is one runner's socket as the thread page's routes use it
// (runner.Client).
type SessionRunner interface {
	Hello(ctx context.Context) (runner.Hello, error)
	Snapshot(ctx context.Context) (runner.Snapshot, error)
	Events(ctx context.Context, boot string, after int64, fn func(runner.Event) error) error
	Abort(ctx context.Context, sessionID string) error
	AnswerCard(ctx context.Context, a runner.Answer) (bool, error)
}

// RunnerDoors is how the thread page's routes reach a session beyond its
// row: its runner's socket, and its conversation read from disk for when no
// runner is up.
type RunnerDoors struct {
	Dial func(sessionID string) SessionRunner
	// History is a session's conversation from Claude Code's own store, as
	// the JSON list of messages a snapshot holds.
	History func(ctx context.Context, sess store.RunnerSession) (json.RawMessage, error)
}

// SetRunnerDoors turns the thread page's routes on. SetRunners comes first.
func (s *Server) SetRunnerDoors(d RunnerDoors) { s.doors = d }

func (s *Server) doorsReady(w http.ResponseWriter) bool {
	if !s.runnersReady(w) {
		return false
	}
	if s.doors.Dial == nil || s.doors.History == nil {
		writeError(w, http.StatusServiceUnavailable, "session runners are off on this host (they need systemd)")
		return false
	}
	return true
}

// streamFrame is one frame of /v1/sessions/{id}/ws, and the body of
// GET /v1/sessions/{id}/messages (a snapshot frame).
//
//	state     where the session is: running, asleep, done, unknown, starting
//	          (its runner is not answering yet) or updating (its runner is an
//	          older build, restarted when its turn ends)
//	snapshot  the conversation (Data) as of event Seq of runner process Boot;
//	          Boot is "" for one read from disk, which no events follow
//	event     event number Seq (Data is the runner's {type, properties})
type streamFrame struct {
	T     string          `json:"t"`
	State string          `json:"state,omitempty"`
	Note  string          `json:"note,omitempty"`
	Boot  string          `json:"boot,omitempty"`
	Seq   int64           `json:"seq,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

const (
	stateStarting = "starting"
	stateUpdating = "updating"
)

// streamWriteWithin bounds one frame's write. A browser that takes longer
// is gone, or not reading; either way it dials again and continues.
const streamWriteWithin = 30 * time.Second

// sessionStream is one browser's stream of one session.
type sessionStream struct {
	s      *Server
	id     string
	cancel context.CancelFunc
	conn   *websocket.Conn
	wmu    sync.Mutex
	// boot and after are the browser's place: the runner process it is
	// reading and the last event it has. resync asks for a snapshot even on
	// the same process (the events from there could not be read).
	boot   string
	after  int64
	resync bool
	// said is the last state frame sent, so a state is said once.
	said string
	// has is set once this connection's browser holds the conversation: it
	// was sent one (from the runner or from disk), or it stands on the runner
	// being followed. A session that then goes down is not read from disk
	// again: the browser got every event its runner sent.
	has bool
	// every is how long a pass waits before the next look at a session that
	// is not being followed.
	every time.Duration
}

// streamRunnerSession: GET /v1/sessions/{id}/ws?boot=B&after=N.
func (s *Server) streamRunnerSession(w http.ResponseWriter, r *http.Request) {
	if !s.doorsReady(w) {
		return
	}
	id := r.PathValue("id")
	if sess, err := s.runnerRows.RunnerSession(id); sess == nil {
		writeRunnerError(w, orNoRunner(err))
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	st := &sessionStream{s: s, id: id, cancel: cancel, conn: conn, boot: r.URL.Query().Get("boot"), after: after, every: s.streamEvery()}
	go st.reads()
	go st.pings(ctx)
	st.run(ctx)
}

// reads owns the socket's read side: the page sends nothing here (its
// actions are routes), so this only notices the browser leaving, or going
// silent past the read deadline, and ends the connection.
func (st *sessionStream) reads() {
	defer st.cancel()
	st.s.ka.armReads(st.conn)
	for {
		if _, _, err := st.conn.ReadMessage(); err != nil {
			readEnded("session stream", st.id, err, st.s.ka.readWithin())
			return
		}
		st.s.ka.touchReads(st.conn)
	}
}

// pings keeps the browser answering (its pongs feed the read deadline), and
// ends the connection when one cannot be written.
func (st *sessionStream) pings(ctx context.Context) {
	t := time.NewTicker(st.s.ka.pingEvery())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := st.s.ka.writePing(st.conn); err != nil {
				st.cancel()
				return
			}
		}
	}
}

// streamEvery is the relay's pace while a session is not being followed.
func (s *Server) streamEvery() time.Duration {
	if s.streamPace > 0 {
		return s.streamPace
	}
	return time.Second
}

// write sends one frame. A failed write ends the connection: the relay
// never skips a frame and goes on.
func (st *sessionStream) write(f streamFrame) error {
	st.wmu.Lock()
	defer st.wmu.Unlock()
	_ = st.conn.SetWriteDeadline(time.Now().Add(streamWriteWithin))
	err := st.conn.WriteJSON(f)
	if err != nil {
		st.cancel()
	}
	return err
}

// say sends the session's state when it is not the one last said.
func (st *sessionStream) say(state, note string) {
	if st.said == state+"\x00"+note {
		return
	}
	if st.write(streamFrame{T: "state", State: state, Note: note}) == nil {
		st.said = state + "\x00" + note
	}
}

// run follows the session until the browser leaves: live while its runner
// answers, from disk otherwise.
func (st *sessionStream) run(ctx context.Context) {
	for ctx.Err() == nil {
		sess, state := st.s.sessionState(st.id)
		if sess == nil {
			sess, state = &store.RunnerSession{ID: st.id}, store.SessionDone
		}
		if state != store.SessionRunning {
			st.down(ctx, *sess, state)
		} else if err := st.follow(ctx); err != nil && ctx.Err() == nil {
			st.notFollowing(err)
		}
		if state == store.SessionDone {
			<-ctx.Done() // done is final: nothing more will be said
			return
		}
		st.pause(ctx)
	}
}

// sessionState is a session's row and where it is now (viewOf's rule). A
// stream asks it every second, so it reads the state alone, not the view.
func (s *Server) sessionState(id string) (*store.RunnerSession, string) {
	sess, err := s.runnerRows.RunnerSession(id)
	if err != nil {
		log.Printf("sessions: row of %s not read: %v", id, err)
		return &store.RunnerSession{ID: id}, stateUnknown
	}
	if sess == nil {
		return nil, store.SessionDone
	}
	return sess, s.stateOf(sess.ID)
}

func (st *sessionStream) pause(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(st.every):
	}
}

// down is a pass over a session whose runner is not up: its state, and,
// once per connection, its conversation from disk. A browser that was
// following the runner until it went down already has every event it sent.
func (st *sessionStream) down(ctx context.Context, sess store.RunnerSession, state string) {
	if !st.has {
		if err := st.sendFromDisk(ctx, sess); err != nil {
			note := "the conversation could not be read from disk: " + err.Error()
			if state == store.SessionDone {
				st.sayProblem(state, note) // final, so said with its state; there is no next pass
				return
			}
			// Said instead of the session's state, and read again next pass:
			// a plain "asleep" over an empty page would hide that it failed.
			st.sayProblem(stateUnknown, note)
			return
		}
		st.has = true
	}
	st.say(state, sess.Note)
}

// sendFromDisk sends the conversation as Claude Code's store holds it.
func (st *sessionStream) sendFromDisk(ctx context.Context, sess store.RunnerSession) error {
	data, err := st.s.diskSnapshot(ctx, sess)
	if err != nil {
		return err
	}
	if err := st.write(streamFrame{T: "snapshot", Data: data}); err != nil {
		return err
	}
	st.boot, st.after = "", 0
	return nil
}

// sayProblem says a state whose note is a failure, and logs it once per
// distinct failure rather than on every pass.
func (st *sessionStream) sayProblem(state, note string) {
	if st.said != state+"\x00"+note {
		log.Printf("sessions: %s: %s", st.id, note)
	}
	st.say(state, note)
}

// diskSnapshot is a snapshot body holding only the conversation from disk.
func (s *Server) diskSnapshot(ctx context.Context, sess store.RunnerSession) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	msgs, err := s.doors.History(ctx, sess)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]json.RawMessage{"messages": msgs})
}

// errStreamEnded is a runner closing its event stream: it is going down, or
// restarting.
var errStreamEnded = errors.New("the runner closed its event stream")

// errRunnerOld is a runner on another protocol: it has no snapshot to give,
// and the supervisor restarts it once its turn ends.
var errRunnerOld = errors.New("the session's runner is an older build")

// follow reads the running session's runner until its stream ends: a
// snapshot first unless the browser stands on this very process, then every
// event after its place.
func (st *sessionStream) follow(ctx context.Context) error {
	c := st.s.doors.Dial(st.id)
	h, err := c.Hello(ctx)
	if err != nil {
		return err
	}
	if h.Protocol != runner.Protocol {
		return errRunnerOld
	}
	if h.Boot != st.boot || st.resync {
		if err := st.sendSnapshot(ctx, c); err != nil {
			return err
		}
	}
	st.has = true
	st.say(store.SessionRunning, "")
	before := st.after
	err = c.Events(ctx, st.boot, st.after, st.forward)
	if err == nil {
		return errStreamEnded
	}
	// No way on from this place (a gap, another process, or the next event
	// cannot be read): the next pass starts from a snapshot instead of
	// asking for the same events again.
	if errors.Is(err, runner.ErrGap) || st.after == before {
		st.resync = true
	}
	return err
}

func (st *sessionStream) sendSnapshot(ctx context.Context, c SessionRunner) error {
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return err
	}
	if err := st.write(streamFrame{T: "snapshot", Boot: snap.Boot, Seq: snap.Seq, Data: snap.Body}); err != nil {
		return err
	}
	st.boot, st.after, st.resync = snap.Boot, snap.Seq, false
	return nil
}

func (st *sessionStream) forward(ev runner.Event) error {
	if err := st.write(streamFrame{T: "event", Seq: ev.Seq, Data: ev.Data}); err != nil {
		return err
	}
	st.after = ev.Seq
	return nil
}

// notFollowing says why a running session is not being read right now. The
// stream ending is said by the next pass (asleep, or running again).
func (st *sessionStream) notFollowing(err error) {
	switch {
	case errors.Is(err, errRunnerOld):
		st.say(stateUpdating, "this session runs an older build; it restarts on the new one when its turn ends")
	case errors.Is(err, errStreamEnded), errors.Is(err, runner.ErrGap):
	case runnerNotListening(err):
		st.say(stateStarting, "")
	default:
		// It answers but fails: said and logged, never a silent "starting".
		st.sayProblem(stateStarting, "its runner answered with an error: "+err.Error())
	}
}

// runnerNotListening is a runner whose socket is not there or not taking
// connections yet: a session starting up, not one that is broken.
func runnerNotListening(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

// runnerSessionMessages: GET /v1/sessions/{id}/messages is the session's
// conversation in one read, as a snapshot frame: from its runner when that
// answers, from disk otherwise.
func (s *Server) runnerSessionMessages(w http.ResponseWriter, r *http.Request) {
	if !s.doorsReady(w) {
		return
	}
	sess, state := s.sessionState(r.PathValue("id"))
	if sess == nil {
		writeRunnerError(w, store.ErrNoSession)
		return
	}
	if state == store.SessionRunning {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		snap, err := s.doors.Dial(sess.ID).Snapshot(ctx)
		if err == nil {
			writeJSON(w, http.StatusOK, streamFrame{T: "snapshot", State: state, Boot: snap.Boot, Seq: snap.Seq, Data: snap.Body})
			return
		}
		log.Printf("sessions: %s: its runner gave no snapshot, serving the conversation from disk: %v", sess.ID, err)
		state = stateStarting // its runner is not answering (yet): what disk holds is the best there is
	}
	data, err := s.diskSnapshot(r.Context(), *sess)
	if err != nil {
		writeError(w, http.StatusBadGateway, "the conversation could not be read from disk: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, streamFrame{T: "snapshot", State: state, Data: data})
}
