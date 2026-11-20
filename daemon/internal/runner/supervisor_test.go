package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/walls"
)

// fakeUnits is systemd as the supervisor sees it.
type fakeUnits struct {
	mu         sync.Mutex
	active     map[string]bool
	result     map[string]string // systemd's Result, by unit
	onActive   func(unit string) // runs at each Active call, before it answers
	stopErr    error
	calls      []string
	onStart    func(unit string)
	onRestart  func(unit string) // runs at each Restart call
	restartErr error             // what Restart fails with, when set
	activeErr  error             // what Active fails with, when set
	reloaded   int
}

func (f *fakeUnits) log(verb, unit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, verb+" "+unit)
}

func (f *fakeUnits) Start(u string) error {
	f.log("start", u)
	f.mu.Lock()
	f.active[u] = true
	f.mu.Unlock()
	if f.onStart != nil {
		f.onStart(u)
	}
	return nil
}
func (f *fakeUnits) Stop(u string) error {
	f.log("stop", u)
	if f.stopErr != nil {
		return f.stopErr
	}
	f.set(u, false)
	return nil
}
func (f *fakeUnits) Restart(u string) error {
	f.log("restart", u)
	if f.onRestart != nil {
		f.onRestart(u)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restartErr
}
func (f *fakeUnits) Reload() error { f.reloaded++; return nil }
func (f *fakeUnits) ResetFailed(u string) error {
	f.log("reset-failed", u)
	f.mu.Lock()
	delete(f.result, u)
	f.mu.Unlock()
	return nil
}
func (f *fakeUnits) Result(u string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.result[u], nil
}
func (f *fakeUnits) Active(u string) (bool, error) {
	if f.onActive != nil {
		f.onActive(u)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.activeErr != nil {
		return false, f.activeErr
	}
	return f.active[u], nil
}
func (f *fakeUnits) set(u string, v bool) { f.mu.Lock(); f.active[u] = v; f.mu.Unlock() }
func (f *fakeUnits) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// fakeRunner is a session runner's HTTP surface on a unix socket: what it
// was sent, and what its transcript holds.
type fakeRunner struct {
	mu         sync.Mutex
	boot       string
	protocol   int
	busy       bool
	sent       []string            // prompt ids, in order
	images     map[string][]string // prompt id → the image names sent with it
	transcript map[string]bool     // ids Claude has
	cards      []map[string]string
	questions  []map[string]any
	rewinds    []string // "<to> <dry>" of each rewind asked
	rewindTo   Rewind   // what a rewind answers; a 409 when rewindNo is set
	rewindNo   string
	replies    []string // "<path> <body>" of each card reply, in order
	srv        *http.Server
}

// rewind records a going back and answers what the test set.
func (r *fakeRunner) rewind(w http.ResponseWriter, req *http.Request) {
	var in struct {
		To  string
		Dry bool
	}
	json.NewDecoder(req.Body).Decode(&in)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rewinds = append(r.rewinds, fmt.Sprintf("%s %v", in.To, in.Dry))
	if r.rewindNo != "" {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": r.rewindNo})
		return
	}
	out := r.rewindTo
	if in.Dry {
		out.Conversation = ""
	}
	json.NewEncoder(w).Encode(out)
}

// reply records a card reply and answers 200.
func (r *fakeRunner) reply(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replies = append(r.replies, req.URL.Path+" "+string(body))
	w.Write([]byte("{}"))
}

func (r *fakeRunner) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello", func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		json.NewEncoder(w).Encode(Hello{Protocol: r.protocol, Boot: r.boot, Busy: r.busy})
	})
	mux.HandleFunc("POST /prompt", func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			ID, Text string
			Images   []string
		}
		json.NewDecoder(req.Body).Decode(&in)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.sent = append(r.sent, in.ID)
		if r.images == nil {
			r.images = map[string][]string{}
		}
		r.images[in.ID] = in.Images
		json.NewEncoder(w).Encode(map[string]bool{"sent": true})
	})
	mux.HandleFunc("POST /delivered", func(w http.ResponseWriter, req *http.Request) {
		var in struct{ IDs []string }
		json.NewDecoder(req.Body).Decode(&in)
		r.mu.Lock()
		defer r.mu.Unlock()
		have := []string{}
		for _, id := range in.IDs {
			if r.transcript[id] {
				have = append(have, id)
			}
		}
		json.NewEncoder(w).Encode(map[string][]string{"delivered": have})
	})
	mux.HandleFunc("GET /permission", func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		json.NewEncoder(w).Encode(append([]map[string]string{}, r.cards...))
	})
	mux.HandleFunc("GET /question", func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		json.NewEncoder(w).Encode(append([]map[string]any{}, r.questions...))
	})
	mux.HandleFunc("POST /rewind", r.rewind)
	mux.HandleFunc("POST /permission/{id}/reply", r.reply)
	mux.HandleFunc("POST /question/{id}/reply", r.reply)
	return mux
}

// stepClock is the rig's time: every read moves it a millisecond on, so two
// sessions made one after the other never share a created_at (the list
// order is then the order they were made), and a test can jump it ahead.
type stepClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(time.Millisecond)
	return c.at
}

func (c *stepClock) jump(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

type rig struct {
	clock *stepClock
	t     *testing.T
	st    *store.SQLite
	units *fakeUnits
	paths Paths
	sup   *Supervisor
	run   map[string]*fakeRunner // by session id
}

// newRig is a supervisor over a real registry, a fake systemd, and a fake
// runner that comes up on its socket whenever its unit starts.
func newRig(t *testing.T) *rig {
	t.Helper()
	dir, err := os.MkdirTemp("", "rn") // short: unix socket paths are capped near 108 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	st, err := store.Open(filepath.Join(dir, "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r := &rig{t: t, st: st, units: &fakeUnits{active: map[string]bool{}, result: map[string]string{}}, paths: Paths{State: filepath.Join(dir, "st"), Runtime: filepath.Join(dir, "s"), Prefix: "ccmux-session-test"}, run: map[string]*fakeRunner{}}
	cfg := Config{Sidecar: func() (string, string, error) { return "/sc/serve.mjs", "/bin/node", nil }, Claude: "/bin/claude", Env: func() []string { return []string{"HOME=/h", "CLAUDECODE=1", "TMUX=x"} }}
	r.sup = New(st, r.units, r.paths, cfg)
	r.clock = &stepClock{at: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	r.sup.now = r.clock.now
	r.sup.soon = func(string) {} // the tests reconcile by hand, in a known order
	r.units.onStart = r.bootRunner
	return r
}

// bootRunner plays a runner process starting: a new boot id, the same
// transcript as before (it lives on disk), nothing pending in memory.
func (r *rig) bootRunner(unit string) {
	id := strings.TrimSuffix(strings.TrimPrefix(unit, "ccmux-session-test@"), ".service")
	old := r.run[id]
	fr := &fakeRunner{boot: "boot-" + string(rune('a'+len(r.units.calls))), protocol: Protocol, transcript: map[string]bool{}}
	if old != nil {
		old.srv.Close()
		fr.transcript = old.transcript
	}
	sock := r.paths.Socket(id)
	os.MkdirAll(filepath.Dir(sock), 0o700)
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		r.t.Fatal(err)
	}
	fr.srv = &http.Server{Handler: fr.handler()}
	go fr.srv.Serve(ln)
	r.t.Cleanup(func() { fr.srv.Close() })
	r.run[id] = fr
}

func (r *rig) reconcile() { r.sup.Reconcile(context.Background()) }

func TestCreateWritesTheSpecAndStartsTheUnit(t *testing.T) {
	r := newRig(t)
	sess, err := r.sup.Create(NewSession{Cwd: "/r/app", Model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	if r.units.count("start "+r.paths.Unit(sess.ID)) != 1 {
		t.Fatalf("unit started once: %v", r.units.calls)
	}
	sp, err := ReadSpec(r.paths.SpecFile(sess.ID))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(sp.Args, " ")
	for _, want := range []string{"--socket " + r.paths.Socket(sess.ID), "--session-auto " + sess.ID, "--detached 1", "--setting-sources user,project,local", "--claude /bin/claude", "--model opus", "--agent app"} {
		if !strings.Contains(args, want) {
			t.Errorf("spec args miss %q: %s", want, args)
		}
	}
	if sp.Dir != "/r/app" || !slices.Equal(sp.Env, []string{"HOME=/h"}) {
		t.Errorf("spec runs in the repo with the pane and Claude markers dropped: %+v", sp)
	}
}

func TestAPromptIsSentUntilTheTranscriptHasIt(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	id, err := r.sup.Send(sess.ID, "fix the build", "you")
	if err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	fr := r.run[sess.ID]
	if !slices.Equal(fr.sent, []string{id}) {
		t.Fatalf("sent once: %v", fr.sent)
	}
	r.reconcile()
	if len(fr.sent) != 2 {
		t.Fatalf("not in the transcript yet, so sent again (the runner drops a resend it has): %v", fr.sent)
	}
	fr.transcript[id] = true
	r.reconcile()
	if open, _ := r.st.UndeliveredPrompts(sess.ID); len(open) != 0 {
		t.Fatalf("once Claude has it, it is delivered: %+v", open)
	}
	r.reconcile()
	if len(fr.sent) != 2 {
		t.Fatalf("a delivered prompt is never sent again: %v", fr.sent)
	}
}

func TestACrashedRunnerIsStartedAgainAndGetsWhatItLost(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	id, _ := r.sup.Send(sess.ID, "commit it", "you")
	r.reconcile() // sent, but the runner dies before Claude writes it down
	r.units.set(r.paths.Unit(sess.ID), false)
	r.reconcile() // the unit is down: started again
	if r.units.count("start") != 2 {
		t.Fatalf("restarted: %v", r.units.calls)
	}
	r.reconcile() // the new runner answers: the prompt goes again
	if got := r.run[sess.ID].sent; !slices.Equal(got, []string{id}) {
		t.Fatalf("the new process gets the prompt the old one lost: %v", got)
	}
}

func TestANewSupervisorPicksUpWhereTheLastStopped(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	id, _ := r.sup.Send(sess.ID, "go on", "you")
	// The daemon restarts: a new supervisor over the same rows, and the
	// runner kept running all along.
	r.sup = New(r.st, r.units, r.paths, r.sup.cfg)
	r.reconcile()
	if r.units.count("start") != 1 {
		t.Fatalf("a running unit is not started again: %v", r.units.calls)
	}
	if got := r.run[sess.ID].sent; !slices.Equal(got, []string{id}) {
		t.Fatalf("the inbox survives the daemon: %v", got)
	}
}

func TestCardsExpireWithTheirRunnerAndCloseWhenAnswered(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	fr := r.run[sess.ID]
	fr.cards = []map[string]string{{"id": "abcde", "permission": "Bash"}, {"id": "fghij", "permission": "Edit"}}
	r.reconcile()
	if open, _ := r.st.OpenSessionCards(sess.ID); len(open) != 2 {
		t.Fatalf("both cards recorded: %+v", open)
	}
	fr.cards = fr.cards[1:] // abcde answered
	r.reconcile()
	r.units.set(r.paths.Unit(sess.ID), false) // the runner dies with fghij pending
	r.reconcile()
	r.reconcile()
	open, _ := r.st.OpenSessionCards(sess.ID)
	if len(open) != 0 {
		t.Fatalf("no card is left open: %+v", open)
	}
}

func TestAnOldRunnerIsRestartedBetweenTurns(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	fr := r.run[sess.ID]
	fr.protocol, fr.busy = Protocol+1, true
	r.reconcile()
	if r.units.count("restart") != 0 {
		t.Fatal("a busy runner finishes its turn first")
	}
	fr.busy = false
	r.reconcile()
	if r.units.count("restart") != 1 {
		t.Fatalf("an idle runner on another protocol is restarted: %v", r.units.calls)
	}
}

// A restart asked for between turns waits for a busy runner's turn to end,
// happens once, and leaves an asleep session asleep.
func TestRestartBetweenTurnsWaitsForTheTurn(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	other, _ := r.sup.Create(NewSession{Cwd: "/r/web"})
	r.reconcile()
	fr := r.run[sess.ID]
	fr.busy = true
	if err := r.sup.RestartBetweenTurns(sess.ID); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if n := r.units.count("restart"); n != 0 {
		t.Fatalf("a busy runner was restarted mid-turn: %v", r.units.calls)
	}
	fr.busy = false
	r.reconcile()
	r.reconcile()
	if n := r.units.count("restart"); n != 1 {
		t.Fatalf("restarts = %d, want one once its turn ended: %v", n, r.units.calls)
	}
	if err := r.sup.SetWanted(other.ID, store.SessionAsleep); err != nil {
		t.Fatal(err)
	}
	if err := r.sup.RestartBetweenTurns(other.ID); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if want, _ := r.sup.Liveness(other.ID); want != store.SessionAsleep {
		t.Fatalf("an asleep session was woken: %s", want)
	}
}

// A restart between turns that fails is tried again at the next pass, and
// stops once one took.
func TestRestartBetweenTurnsTriesAFailedRestartAgain(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	r.units.restartErr = errors.New("systemd busy")
	if err := r.sup.RestartBetweenTurns(sess.ID); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if n := r.units.count("restart"); n != 1 {
		t.Fatalf("restarts tried = %d, want 1", n)
	}
	r.units.mu.Lock()
	r.units.restartErr = nil
	r.units.mu.Unlock()
	r.reconcile()
	r.reconcile()
	if n := r.units.count("restart"); n != 2 {
		t.Fatalf("restarts tried = %d, want the failed one and one more that took: %v", n, r.units.calls)
	}
}

// A mark left on a session that went to sleep is dropped: woken, it is
// not restarted again on its first idle pass.
func TestRestartBetweenTurnsIsDroppedBySleep(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	r.run[sess.ID].busy = true
	if err := r.sup.RestartBetweenTurns(sess.ID); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if err := r.sup.SetWanted(sess.ID, store.SessionAsleep); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if err := r.sup.SetWanted(sess.ID, store.SessionRunning); err != nil {
		t.Fatal(err)
	}
	r.run[sess.ID].busy = false
	r.reconcile()
	r.reconcile()
	r.reconcile()
	if n := r.units.count("restart"); n != 0 {
		t.Fatalf("a woken session was restarted for a move made before its sleep: %v", r.units.calls)
	}
}

func TestAsleepAndDoneSessionsAreStopped(t *testing.T) {
	r := newRig(t)
	a, _ := r.sup.Create(NewSession{Cwd: "/r/a"})
	b, _ := r.sup.Create(NewSession{Cwd: "/r/b"})
	if err := r.sup.SetWanted(a.ID, store.SessionAsleep); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if err := r.sup.SetWanted(b.ID, store.SessionDone); err != nil {
		t.Fatal(err)
	}
	if r.units.count("stop "+r.paths.Unit(a.ID)) != 1 || r.units.count("stop "+r.paths.Unit(b.ID)) != 1 {
		t.Fatalf("both stopped: %v", r.units.calls)
	}
	if _, err := r.sup.Send(b.ID, "hello?", "you"); !errors.Is(err, ErrSessionDone) {
		t.Fatalf("a done session refuses prompts, got %v", err)
	}
	if open, _ := r.st.UndeliveredPrompts(b.ID); len(open) != 0 {
		t.Fatalf("and queues nothing: %+v", open)
	}
	if _, err := r.sup.Send(a.ID, "wake up", "you"); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if r.units.count("start "+r.paths.Unit(a.ID)) != 2 {
		t.Fatalf("a prompt wakes an asleep session: %v", r.units.calls)
	}
}

func TestTemplateIsWrittenOnceAndReloaded(t *testing.T) {
	dir := t.TempDir()
	u := &fakeUnits{active: map[string]bool{}, result: map[string]string{}}
	paths := Paths{State: "/st", Prefix: "ccmux-session-test"}
	for i := 0; i < 2; i++ {
		if err := EnsureTemplate(dir, "/bin/ccmuxd", paths, walls.Defaults().Caps(), u); err != nil {
			t.Fatal(err)
		}
	}
	if u.reloaded != 1 {
		t.Fatalf("an unchanged template needs no reload: %d", u.reloaded)
	}
	if _, err := os.Stat(filepath.Join(dir, "ccmux-session@.service")); err == nil {
		t.Fatal("a test prefix never writes the daemon's own template")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "ccmux-session-test@.service"))
	for _, want := range []string{"ExecStart=/bin/ccmuxd runner-exec /st/%i.json", "Restart=on-failure", "KillMode=control-group", "StandardInput=null", "StartLimitBurst=5", "StartLimitIntervalSec=600"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("template misses %q", want)
		}
	}
}

func TestASessionPastTheStartLimitIsPutToSleepAndAPromptWakesIt(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	other, _ := r.sup.Create(NewSession{Cwd: "/r/api"})
	unit := r.paths.Unit(sess.ID)
	r.units.set(unit, false)
	r.units.result[unit] = "start-limit-hit"
	r.units.set(r.paths.Unit(other.ID), false) // crashed once: started again as usual
	r.reconcile()
	got, _ := r.st.RunnerSession(sess.ID)
	if got.Wanted != store.SessionAsleep || !strings.Contains(got.Note, "put to sleep") {
		t.Fatalf("parked with a note: %+v", got)
	}
	if r.units.count("start "+unit) != 1 {
		t.Fatalf("not started again past the limit: %v", r.units.calls)
	}
	if r.units.count("start "+r.paths.Unit(other.ID)) != 2 {
		t.Fatalf("a session under the limit is restarted: %v", r.units.calls)
	}
	if _, err := r.sup.Send(sess.ID, "try again", "you"); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	got, _ = r.st.RunnerSession(sess.ID)
	if got.Wanted != store.SessionRunning || got.Note != "" || r.units.count("reset-failed "+unit) != 1 || r.units.count("start "+unit) != 2 {
		t.Fatalf("a prompt clears the limit and starts it: %+v %v", got, r.units.calls)
	}
}

func TestAFailedFirstStartStillHandsBackTheSession(t *testing.T) {
	r := newRig(t)
	broken := true
	r.sup.cfg.Sidecar = func() (string, string, error) {
		if broken {
			return "", "", errors.New("npm ci: registry unreachable")
		}
		return "/sc/serve.mjs", "/bin/node", nil
	}
	sess, err := r.sup.Create(NewSession{Cwd: "/r/app"})
	if err != nil || sess.ID == "" || !strings.Contains(sess.Note, "registry unreachable") {
		t.Fatalf("the caller gets the id and why it is not up yet: %+v %v", sess, err)
	}
	broken = false
	r.reconcile()
	if list, _ := r.st.RunnerSessions(); len(list) != 1 || r.units.count("start") != 1 {
		t.Fatalf("one session, started once the sidecar installs: %+v %v", list, r.units.calls)
	}
	r.reconcile() // the runner answers
	if got, _ := r.st.RunnerSession(sess.ID); got.Note != "" {
		t.Fatalf("a runner that came up clears the failure note: %q", got.Note)
	}
}

// The pass reads its list, then reaches b: an end of b that lands between
// the two is seen, because each step reads its row fresh.
func TestAStopDuringAReconcilePassIsNotUndone(t *testing.T) {
	r := newRig(t)
	a, _ := r.sup.Create(NewSession{Cwd: "/r/a"})
	b, _ := r.sup.Create(NewSession{Cwd: "/r/b"})
	r.units.set(r.paths.Unit(b.ID), false) // b's runner died: this pass would start it
	ended := false
	r.units.onActive = func(unit string) {
		if unit == r.paths.Unit(a.ID) && !ended {
			ended = true
			if err := r.sup.SetWanted(b.ID, store.SessionDone); err != nil { // the user ends b mid-pass
				t.Error(err)
			}
		}
	}
	r.reconcile()
	if r.units.count("start "+r.paths.Unit(b.ID)) != 1 {
		t.Fatalf("an ended session is not started again by the pass that was under way: %v", r.units.calls)
	}
	if r.units.count("start "+r.paths.Unit(a.ID)) != 1 {
		t.Fatalf("the other session is left alone: %v", r.units.calls)
	}
}

func TestDoneIsFinalAndOnlyAfterTheStop(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.units.stopErr = errors.New("bus timeout")
	if err := r.sup.SetWanted(sess.ID, store.SessionDone); err == nil {
		t.Fatal("a failed stop is an error")
	}
	if got, _ := r.st.RunnerSession(sess.ID); got.Wanted != store.SessionRunning {
		t.Fatalf("a failed stop leaves the row running, where reconcile still sees it: %+v", got)
	}
	r.units.stopErr = nil
	if err := r.sup.SetWanted(sess.ID, store.SessionDone); err != nil {
		t.Fatal(err)
	}
	if err := r.sup.SetWanted(sess.ID, store.SessionDone); err != nil {
		t.Fatalf("ending an ended session is no change: %v", err)
	}
	if err := r.sup.SetWanted(sess.ID, store.SessionRunning); !errors.Is(err, ErrSessionDone) {
		t.Fatalf("a done session never runs again: %v", err)
	}
}

func TestStartsThatFailBeforeSystemdAreCountedAndCapped(t *testing.T) {
	r := newRig(t)
	ok, _ := r.sup.Create(NewSession{Cwd: "/r/ok"})
	r.sup.cfg.Sidecar = func() (string, string, error) { return "", "", errors.New("no node on this host") }
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	for i := 0; i < startLimitBurst; i++ {
		r.reconcile()
	}
	got, _ := r.st.RunnerSession(sess.ID)
	if got.Wanted != store.SessionAsleep || !strings.Contains(got.Note, "failed to start 5 times") || !strings.Contains(got.Note, "no node") {
		t.Fatalf("put to sleep after %d failed starts, saying why: %+v", startLimitBurst, got)
	}
	if other, _ := r.st.RunnerSession(ok.ID); other.Wanted != store.SessionRunning || other.Note != "" {
		t.Fatalf("a session that started is untouched: %+v", other)
	}
}

// note is a session's note, read from its row.
func (r *rig) note(id string) string {
	got, _ := r.st.RunnerSession(id)
	return got.Note
}

func (r *rig) failSidecar() {
	r.sup.cfg.Sidecar = func() (string, string, error) { return "", "", errors.New("no node on this host") }
}

func TestAWakeAfterTheFailureCapGivesAFreshCount(t *testing.T) {
	r := newRig(t)
	r.failSidecar()
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	for i := 0; i < startLimitBurst; i++ {
		r.reconcile()
	}
	if got, _ := r.st.RunnerSession(sess.ID); got.Wanted != store.SessionAsleep {
		t.Fatalf("parked first: %+v", got)
	}
	r.sup.Send(sess.ID, "try again", "you")
	r.reconcile() // still failing: one failure, not the sixth
	got, _ := r.st.RunnerSession(sess.ID)
	if got.Wanted != store.SessionRunning || !strings.Contains(got.Note, "(1 of 5)") {
		t.Fatalf("a wake starts the count over: %+v", got)
	}
}

func TestOldFailuresDropOutOfTheWindow(t *testing.T) {
	r := newRig(t)
	r.failSidecar()
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"}) // failure 1
	for i := 0; i < 3; i++ {
		r.reconcile() // failures 2 to 4
	}
	r.clock.jump(startLimitWindow*time.Second + time.Second)
	r.reconcile()
	got, _ := r.st.RunnerSession(sess.ID)
	if got.Wanted != store.SessionRunning || !strings.Contains(got.Note, "(1 of 5)") {
		t.Fatalf("failures older than the window no longer count: %+v", got)
	}
}

func TestARunnerThatCameUpStartsTheCountOver(t *testing.T) {
	r := newRig(t)
	broken := true
	r.sup.cfg.Sidecar = func() (string, string, error) {
		if broken {
			return "", "", errors.New("npm ci failed")
		}
		return "/sc/serve.mjs", "/bin/node", nil
	}
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	for i := 0; i < 3; i++ {
		r.reconcile() // four failures in all
	}
	broken = false
	r.reconcile() // started
	r.reconcile() // answers
	broken = true
	r.units.set(r.paths.Unit(sess.ID), false) // it dies, and the next start fails
	r.reconcile()
	if !strings.Contains(r.note(sess.ID), "(1 of 5)") {
		t.Fatalf("the failures before it came up are forgotten: %q", r.note(sess.ID))
	}
}

func TestAnEndedSessionTakesNoModel(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app", Model: "opus"})
	r.sup.SetWanted(sess.ID, store.SessionDone)
	if err := r.sup.SetModel(sess.ID, "haiku"); !errors.Is(err, ErrSessionDone) {
		t.Fatalf("409, like a prompt to it: %v", err)
	}
}

// Ending a session while a pass is in the middle of that same session waits
// for the pass's step, then stops what it started: the lock, not luck.
func TestEndingASessionMidStepWaitsForTheStep(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	unit := r.paths.Unit(sess.ID)
	r.units.set(unit, false) // the pass will start it again
	done := make(chan error, 1)
	fired := false
	r.units.onActive = func(u string) {
		if u == unit && !fired {
			fired = true
			go func() { done <- r.sup.SetWanted(sess.ID, store.SessionDone) }()
			time.Sleep(50 * time.Millisecond) // the end would land mid-step without the lock
		}
	}
	r.reconcile()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if active, _ := r.units.Active(unit); active {
		t.Fatalf("the end stopped what the step started: %v", r.units.calls)
	}
	if got, _ := r.st.RunnerSession(sess.ID); got.Wanted != store.SessionDone {
		t.Fatalf("done: %+v", got)
	}
}

// storeDown is a registry whose prompt-and-wake write fails, as a locked or
// full database would.
type storeDown struct{ *store.SQLite }

func (storeDown) AddInboxPromptAndWake(store.InboxPrompt, bool, int64) error {
	return errors.New("database is locked")
}

func TestSendIsAllOrNothing(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.sup.SetWanted(sess.ID, store.SessionAsleep)
	r.sup.st = storeDown{r.st}
	if id, err := r.sup.Send(sess.ID, "hello", "you"); err == nil || id != "" {
		t.Fatalf("nothing was queued, so the caller gets an error to send again on: %q %v", id, err)
	}
	r.sup.st = r.st
	got, _ := r.st.RunnerSession(sess.ID)
	open, _ := r.st.UndeliveredPrompts(sess.ID)
	if got.Wanted != store.SessionAsleep || len(open) != 0 {
		t.Fatalf("no prompt and no wake: %+v %+v", got, open)
	}
	if _, err := r.sup.Send(sess.ID, "hello", "you"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.st.RunnerSession(sess.ID); got.Wanted != store.SessionRunning {
		t.Fatalf("the send again wakes it: %+v", got)
	}
}

func TestASessionPutToSleepStaysAsleep(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.sup.Send(sess.ID, "work", "you")
	r.sup.SetWanted(sess.ID, store.SessionAsleep)
	for i := 0; i < 3; i++ {
		r.reconcile()
	}
	if got, _ := r.st.RunnerSession(sess.ID); got.Wanted != store.SessionAsleep {
		t.Fatalf("only a new prompt or a wake brings it back: %+v", got)
	}
	if active, _ := r.units.Active(r.paths.Unit(sess.ID)); active {
		t.Fatal("and its unit is stopped")
	}
}

func TestRestartNowAppliesTheModel(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app", Model: "opus"})
	r.sup.SetModel(sess.ID, "haiku")
	if err := r.sup.Restart(sess.ID); err != nil {
		t.Fatal(err)
	}
	sp, _ := ReadSpec(r.paths.SpecFile(sess.ID))
	if r.units.count("restart "+r.paths.Unit(sess.ID)) != 1 || !strings.Contains(strings.Join(sp.Args, " "), "--model haiku") {
		t.Fatalf("restarted on the new model: %v %v", r.units.calls, sp.Args)
	}
	other, _ := r.sup.Create(NewSession{Cwd: "/r/quiet"})
	r.sup.SetWanted(other.ID, store.SessionAsleep)
	if err := r.sup.Restart(other.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.st.RunnerSession(other.ID); got.Wanted != store.SessionRunning || r.units.count("restart "+r.paths.Unit(other.ID)) != 0 {
		t.Fatalf("an asleep session is woken, not restarted: %+v %v", got, r.units.calls)
	}
	r.sup.SetWanted(other.ID, store.SessionDone)
	if err := r.sup.Restart(other.ID); !errors.Is(err, ErrSessionDone) {
		t.Fatalf("an ended session does not restart: %v", err)
	}
}

func TestAModelIsSavedForTheNextStartAndNeverSentLive(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app", Model: "opus"})
	if err := r.sup.SetModel(sess.ID, "haiku"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.st.RunnerSession(sess.ID); got.Model != "haiku" {
		t.Fatalf("the row has it: %+v", got)
	}
	r.units.set(r.paths.Unit(sess.ID), false) // its next start
	r.reconcile()
	sp, _ := ReadSpec(r.paths.SpecFile(sess.ID))
	if !strings.Contains(strings.Join(sp.Args, " "), "--model haiku") {
		t.Fatalf("the next start runs on it: %v", sp.Args)
	}
}

func TestAPromptsImagesGoWithItEveryTimeItIsSent(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	pics := []string{"0123456789abcdef0123456789abcdef.png"}
	id, err := r.sup.SendPrompt(sess.ID, Prompt{Text: "what is wrong here?", Sender: "you", Images: pics})
	if err != nil || id == "" {
		t.Fatalf("a prompt with no id of its own gets one: %q %v", id, err)
	}
	plain, _ := r.sup.Send(sess.ID, "and then?", "you")
	r.reconcile()
	fr := r.run[sess.ID]
	if !slices.Equal(fr.images[id], pics) || fr.images[plain] != nil {
		t.Fatalf("the picture goes with its prompt, and only with it: %v", fr.images)
	}
	// The runner crashed before Claude had the prompt: it is sent again, and
	// a resend without its picture would be a different prompt.
	r.units.set(r.paths.Unit(sess.ID), false)
	r.reconcile()
	r.reconcile()
	if again := r.run[sess.ID]; again == fr || !slices.Equal(again.images[id], pics) {
		t.Fatalf("sent again to the new runner with its picture: %v", again.images)
	}
	// The same id sent twice queues once (SendAs's rule holds for SendPrompt).
	if _, err := r.sup.SendPrompt(sess.ID, Prompt{ID: id, Text: "dup"}); err != nil {
		t.Fatal(err)
	}
	if open, _ := r.st.UndeliveredPrompts(sess.ID); len(open) != 2 {
		t.Fatalf("still two waiting: %+v", open)
	}
}

func TestASecondLeadIsRefusedAndNothingIsStartedForIt(t *testing.T) {
	r := newRig(t)
	lead, err := r.sup.Create(NewSession{Cwd: "/r/app", WorkspaceID: "ws", Kind: store.SessionLead, Title: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if row, _ := r.st.RunnerSession(lead.ID); row.Kind != store.SessionLead || row.Title != "app" {
		t.Fatalf("the row keeps its kind and title: %+v", row)
	}
	starts := r.units.count("start ")
	if _, err := r.sup.Create(NewSession{Cwd: "/r/app", WorkspaceID: "ws", Kind: store.SessionLead}); !errors.Is(err, store.ErrLeadExists) {
		t.Fatalf("a second lead: %v", err)
	}
	if r.units.count("start ") != starts {
		t.Fatal("a refused lead must not start a unit")
	}
	// A thread in the same workspace is its own session, started as any.
	if _, err := r.sup.Create(NewSession{Cwd: "/r/app", WorkspaceID: "ws", Title: "a thread"}); err != nil || r.units.count("start ") != starts+1 {
		t.Fatalf("a thread beside the lead: %v", err)
	}
	if list, _ := r.st.RunnerSessions(); len(list) != 2 {
		t.Fatalf("one lead, one thread: %+v", list)
	}
}
