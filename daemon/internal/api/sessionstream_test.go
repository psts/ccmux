package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
)

// fakeProc is a runner process as the relay sees it through its socket: a
// boot id, a numbered log it may have dropped the start of, and a
// conversation that holds every event up to the moment it is asked.
type fakeProc struct {
	mu        sync.Mutex
	boot      string
	protocol  int
	down      bool    // the socket does not answer
	log       []int64 // event numbers held, in order
	seq       int64
	snapshots int
	unread    int64 // Events fails instead of handing over this number
	grew      chan struct{}
	answers   []runner.Answer
	aborts    int
	gone      map[string]bool // cards no longer pending
}

func newProc(boot string) *fakeProc {
	return &fakeProc{boot: boot, protocol: runner.Protocol, grew: make(chan struct{}), gone: map[string]bool{}}
}

// emit adds n events to the log.
func (p *fakeProc) emit(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; i < n; i++ {
		p.seq++
		p.log = append(p.log, p.seq)
	}
	close(p.grew)
	p.grew = make(chan struct{})
}

// restart is the process dying and another coming up: a new boot, an empty
// log, every open stream closed.
func (p *fakeProc) restart(boot string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.boot, p.seq, p.log = boot, 0, nil
	close(p.grew)
	p.grew = make(chan struct{})
}

func (p *fakeProc) Hello(context.Context) (runner.Hello, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		return runner.Hello{}, errors.New("connection refused")
	}
	return runner.Hello{Protocol: p.protocol, Boot: p.boot, Seq: p.seq}, nil
}

func (p *fakeProc) Snapshot(context.Context) (runner.Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshots++
	body := fmt.Sprintf(`{"boot":%q,"seq":%d,"messages":["upto-%d"]}`, p.boot, p.seq, p.seq)
	return runner.Snapshot{Boot: p.boot, Seq: p.seq, Body: json.RawMessage(body)}, nil
}

// next is the events above after that the process holds, or why it cannot
// say, and what to wait on for more.
func (p *fakeProc) next(boot string, after int64) (out []int64, wait chan struct{}, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if boot != p.boot {
		return nil, nil, errStreamOver
	}
	for _, n := range p.log {
		if n > after {
			out = append(out, n)
		}
	}
	return out, p.grew, nil
}

var errStreamOver = errors.New("over")

func (p *fakeProc) Events(ctx context.Context, boot string, after int64, fn func(runner.Event) error) error {
	p.mu.Lock()
	oldest := p.seq + 1
	if len(p.log) > 0 {
		oldest = p.log[0]
	}
	bad := boot != p.boot || after > p.seq || (after > 0 && after < oldest-1) || (after == 0 && oldest > 1)
	p.mu.Unlock()
	if bad {
		return runner.ErrGap
	}
	for {
		batch, wait, err := p.next(boot, after)
		if err != nil {
			return nil // the process went away: its stream just closes
		}
		for _, n := range batch {
			if n == p.unread {
				return errors.New("bufio.Scanner: token too long")
			}
			if err := fn(runner.Event{Seq: n, Data: json.RawMessage(fmt.Sprintf(`{"type":"e","properties":{"n":%d}}`, n))}); err != nil {
				return err
			}
			after = n
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
		}
	}
}

func (p *fakeProc) Abort(context.Context, string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		return errors.New("connection refused")
	}
	p.aborts++
	return nil
}

func (p *fakeProc) AnswerCard(_ context.Context, a runner.Answer) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		return false, errors.New("connection refused")
	}
	if a.Kind != "permission" && a.Kind != "question" {
		return false, runner.ErrBadAnswer
	}
	p.answers = append(p.answers, a)
	return !p.gone[a.ID], nil
}

// streamRows is the registry behind the stream routes: one session, s1.
type streamRows struct {
	fakeRunnerRows
	mu     sync.Mutex
	closed []string // "<card> <state>"
}

func (r *streamRows) SetSessionCardState(_, id, state string, _ int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = append(r.closed, id+" "+state)
	return nil
}

// streamSup says where s1 is; the test moves it.
type streamSup struct {
	*fakeRunners
	mu    sync.Mutex
	state string
}

func (s *streamSup) Liveness(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != "s1" {
		return store.SessionDone, nil
	}
	return s.state, nil
}

func (s *streamSup) set(state string) {
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
}

type streamRig struct {
	t    *testing.T
	proc *fakeProc
	sup  *streamSup
	rows *streamRows
	hs   *httptest.Server
	srv  *Server
	disk string // the conversation "on disk"; "!" fails the read
}

func newStreamRig(t *testing.T) *streamRig {
	t.Helper()
	r := &streamRig{t: t, proc: newProc("b1"), sup: &streamSup{fakeRunners: &fakeRunners{}, state: store.SessionRunning}, rows: &streamRows{}, disk: `["from-disk"]`}
	r.srv = &Server{ka: defaultKeepalive(), streamPace: 5 * time.Millisecond}
	r.srv.SetRunners(r.sup, r.rows)
	r.srv.SetRunnerDoors(RunnerDoors{
		Dial: func(string) SessionRunner { return r.proc },
		History: func(context.Context, store.RunnerSession) (json.RawMessage, error) {
			if r.disk == "!" {
				return nil, errors.New("store unreadable")
			}
			return json.RawMessage(r.disk), nil
		},
	})
	r.hs = httptest.NewServer(r.srv.Handler())
	t.Cleanup(r.hs.Close)
	return r
}

// dial opens the stream as a browser standing at boot/after.
func (r *streamRig) dial(boot string, after int64) *websocket.Conn {
	r.t.Helper()
	url := "ws" + strings.TrimPrefix(r.hs.URL, "http") + fmt.Sprintf("/v1/sessions/s1/ws?boot=%s&after=%d", boot, after)
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { conn.Close() })
	return conn
}

// streamWords reads n frames and writes each as one word: "snap:<boot>@<seq>",
// "ev:<seq>" or "state:<state>".
func streamWords(t *testing.T, conn *websocket.Conn, n int) string {
	t.Helper()
	var out []string
	for len(out) < n {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var f streamFrame
		if err := conn.ReadJSON(&f); err != nil {
			t.Fatalf("after %v: %v", out, err)
		}
		switch f.T {
		case "snapshot":
			out = append(out, fmt.Sprintf("snap:%s@%d", f.Boot, f.Seq))
		case "event":
			out = append(out, fmt.Sprintf("ev:%d", f.Seq))
		default:
			out = append(out, "state:"+f.State)
		}
	}
	return strings.Join(out, " ")
}

func TestAFreshBrowserGetsTheConversationThenEveryEventAfterIt(t *testing.T) {
	r := newStreamRig(t)
	r.proc.emit(3)
	conn := r.dial("", 0)
	if got := streamWords(t, conn, 2); got != "snap:b1@3 state:running" {
		t.Fatal(got)
	}
	r.proc.emit(2)
	if got := streamWords(t, conn, 2); got != "ev:4 ev:5" {
		t.Fatalf("events after the snapshot, once and in order: %s", got)
	}
}

func TestAReconnectGoesOnFromItsNumbers(t *testing.T) {
	// A daemon restart: the browser dials again and says where it stands.
	r := newStreamRig(t)
	r.proc.emit(5)
	conn := r.dial("b1", 3)
	if got := streamWords(t, conn, 3); got != "state:running ev:4 ev:5" {
		t.Fatalf("no snapshot for the same runner process, only what was missed: %s", got)
	}
	if r.proc.snapshots != 0 {
		t.Fatalf("%d snapshots taken for a reader that could continue", r.proc.snapshots)
	}
}

func TestARestartedRunnerStartsOverFromASnapshot(t *testing.T) {
	r := newStreamRig(t)
	r.proc.emit(2)
	conn := r.dial("", 0)
	streamWords(t, conn, 2)
	r.proc.restart("b2")
	r.proc.emit(1)
	got := streamWords(t, conn, 1)
	if got != "snap:b2@1" && got != "snap:b2@0" {
		t.Fatalf("a new runner process numbers its events from 1, so the page gets its conversation whole: %s", got)
	}
	r.proc.emit(1)
	if rest := streamWords(t, conn, 1); !strings.HasPrefix(rest, "ev:") {
		t.Fatalf("then its events: %s", rest)
	}
}

func TestABrowserOnAnEarlierProcessGetsASnapshot(t *testing.T) {
	r := newStreamRig(t)
	r.proc.emit(4)
	if got := streamWords(t, r.dial("b0", 900), 2); got != "snap:b1@4 state:running" {
		t.Fatal(got)
	}
}

func TestDroppedEventsAreASnapshotNotAHole(t *testing.T) {
	r := newStreamRig(t)
	r.proc.emit(10)
	r.proc.mu.Lock()
	r.proc.log = r.proc.log[7:] // the runner kept only 8, 9, 10
	r.proc.mu.Unlock()
	conn := r.dial("b1", 2)
	if got := streamWords(t, conn, 2); got != "state:running snap:b1@10" {
		t.Fatalf("events 3 to 7 are gone, so the page gets the conversation whole: %s", got)
	}
	r.proc.emit(1)
	if got := streamWords(t, conn, 1); got != "ev:11" {
		t.Fatalf("then the stream goes on: %s", got)
	}
}

func TestAnEventThatCannotBeReadIsNotAskedForAgain(t *testing.T) {
	r := newStreamRig(t)
	r.proc.emit(3)
	conn := r.dial("b1", 1)
	r.proc.mu.Lock()
	r.proc.unread = 2 // the very next event is one line too long
	r.proc.mu.Unlock()
	if got := streamWords(t, conn, 2); got != "state:running state:starting" {
		t.Fatalf("the failed read is said: %s", got)
	}
	if got := streamWords(t, conn, 2); got != "snap:b1@3 state:running" {
		t.Fatalf("and the next pass rebuilds from a snapshot, past the event: %s", got)
	}
}

func TestASessionWithNoRunnerIsReadFromDiskOnce(t *testing.T) {
	r := newStreamRig(t)
	r.sup.set(store.SessionAsleep)
	conn := r.dial("b1", 7) // whatever it had may be stale: the runner is gone
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var snap streamFrame
	if err := conn.ReadJSON(&snap); err != nil || snap.T != "snapshot" || snap.Boot != "" || string(snap.Data) != `{"messages":["from-disk"]}` {
		t.Fatalf("the conversation from disk, under no boot: %+v %s %v", snap, snap.Data, err)
	}
	if got := streamWords(t, conn, 1); got != "state:asleep" {
		t.Fatal(got)
	}
	// Woken: the runner's own snapshot replaces it, and events follow.
	r.proc.emit(2)
	r.sup.set(store.SessionRunning)
	if got := streamWords(t, conn, 2); got != "snap:b1@2 state:running" {
		t.Fatal(got)
	}
	// Put to sleep again: the page already has everything; disk is not read twice.
	r.sup.set(store.SessionAsleep)
	r.proc.restart("b9")
	if got := streamWords(t, conn, 1); got != "state:asleep" {
		t.Fatal(got)
	}
}

func TestAConversationDiskCannotGiveIsSaid(t *testing.T) {
	r := newStreamRig(t)
	r.disk = "!"
	r.sup.set(store.SessionAsleep)
	conn := r.dial("", 0)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var f streamFrame
	if err := conn.ReadJSON(&f); err != nil || f.T != "state" || !strings.Contains(f.Note, "store unreadable") {
		t.Fatalf("%+v %v", f, err)
	}
}

func TestAnOlderRunnerIsSaidAndLeftToFinish(t *testing.T) {
	r := newStreamRig(t)
	r.proc.protocol = runner.Protocol - 1
	conn := r.dial("", 0)
	if got := streamWords(t, conn, 1); got != "state:updating" {
		t.Fatal(got)
	}
	time.Sleep(40 * time.Millisecond) // several passes
	if r.proc.snapshots != 0 {
		t.Fatal("a runner with no snapshot route must not be asked for one")
	}
	r.proc.mu.Lock()
	r.proc.protocol = runner.Protocol // restarted on this build
	r.proc.mu.Unlock()
	if got := streamWords(t, conn, 2); got != "snap:b1@0 state:running" {
		t.Fatal(got)
	}
}

func TestARunnerNotAnsweringYetIsStarting(t *testing.T) {
	r := newStreamRig(t)
	r.proc.down = true
	conn := r.dial("", 0)
	if got := streamWords(t, conn, 1); got != "state:starting" {
		t.Fatal(got)
	}
}

func TestMessagesIsTheConversationInOneRead(t *testing.T) {
	r := newStreamRig(t)
	r.proc.emit(2)
	w := callSessions(t, r.srv, "GET", "/v1/sessions/s1/messages", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"boot":"b1"`) || !strings.Contains(w.Body.String(), `"upto-2"`) {
		t.Fatalf("from the runner: %d %s", w.Code, w.Body)
	}
	r.sup.set(store.SessionAsleep)
	w = callSessions(t, r.srv, "GET", "/v1/sessions/s1/messages", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"from-disk"`) || !strings.Contains(w.Body.String(), `"state":"asleep"`) {
		t.Fatalf("from disk: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, r.srv, "GET", "/v1/sessions/nope/messages", ""); w.Code != http.StatusNotFound {
		t.Fatalf("no such session: %d", w.Code)
	}
}

func TestACardIsAnsweredThroughTheRunnerAndItsRowClosed(t *testing.T) {
	r := newStreamRig(t)
	r.proc.gone["late1"] = true
	post := func(card, body string) int {
		return callSessions(t, r.srv, "POST", "/v1/sessions/s1/cards/"+card, body).Code
	}
	if code := post("abcde", `{"kind":"permission","reply":"always"}`); code != http.StatusNoContent {
		t.Fatalf("taken: %d", code)
	}
	if code := post("qqqqq", `{"kind":"question","answers":[["Red"]]}`); code != http.StatusNoContent {
		t.Fatalf("a question: %d", code)
	}
	if code := post("late1", `{"kind":"permission","reply":"once"}`); code != http.StatusConflict {
		t.Fatalf("a card answered elsewhere first: %d", code)
	}
	if code := post("abcde", `{"kind":"mystery"}`); code != http.StatusBadRequest {
		t.Fatalf("not an answer: %d", code)
	}
	if got := strings.Join(r.rows.closed, ","); got != "abcde answered,qqqqq answered" {
		t.Fatalf("only a taken answer closes its row: %s", got)
	}
	if a := r.proc.answers[0]; a.ID != "abcde" || a.Reply != "always" {
		t.Fatalf("the card is named by the path: %+v", a)
	}
	r.proc.down = true
	if code := post("abcde", `{"kind":"permission","reply":"once"}`); code != http.StatusBadGateway {
		t.Fatalf("a runner that does not answer: %d", code)
	}
	if code := callSessions(t, r.srv, "POST", "/v1/sessions/nope/cards/abcde", `{}`).Code; code != http.StatusNotFound {
		t.Fatalf("no such session: %d", code)
	}
}

func TestStopReachesARunningSessionOnly(t *testing.T) {
	r := newStreamRig(t)
	if w := callSessions(t, r.srv, "POST", "/v1/sessions/s1/abort", ""); w.Code != http.StatusNoContent || r.proc.aborts != 1 {
		t.Fatalf("stop: %d, %d aborts", w.Code, r.proc.aborts)
	}
	r.sup.set(store.SessionAsleep)
	if w := callSessions(t, r.srv, "POST", "/v1/sessions/s1/abort", ""); w.Code != http.StatusConflict || r.proc.aborts != 1 {
		t.Fatalf("asleep, there is no turn to stop: %d", w.Code)
	}
}

func TestTheThreadRoutesAreOffWithoutDoors(t *testing.T) {
	s := &Server{}
	s.SetRunners(&fakeRunners{}, fakeRunnerRows{})
	for _, path := range []string{"GET /v1/sessions/s1/messages", "POST /v1/sessions/s1/abort", "POST /v1/sessions/s1/cards/abcde", "GET /v1/sessions/s1/ws"} {
		method, p, _ := strings.Cut(path, " ")
		if w := callSessions(t, s, method, p, "{}"); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
}
