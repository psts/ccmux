package runner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

// The deferred stage-1 fix: a restart of a session whose unit is down used
// to start it and then restart it, booting it twice.
func TestRestartOfAStoppedSessionBootsItOnce(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app", Model: "opus"})
	unit := r.paths.Unit(sess.ID)
	r.units.set(unit, false) // it crashed, or the machine came back up
	if err := r.sup.Restart(sess.ID); err != nil {
		t.Fatal(err)
	}
	if r.units.count("start "+unit) != 1 || r.units.count("restart "+unit) != 1 {
		t.Fatalf("one start (the create) and one restart, nothing starts it first: %v", r.units.calls)
	}
}

func TestTheSpecIsWrittenBeforeTheRestart(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app", Model: "opus"})
	var atRestart string
	r.units.onRestart = func(string) {
		sp, _ := ReadSpec(r.paths.SpecFile(sess.ID))
		atRestart = strings.Join(sp.Args, " ")
	}
	r.sup.SetModel(sess.ID, "haiku")
	if err := r.sup.Restart(sess.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(atRestart, "--model haiku") {
		t.Fatalf("the process systemd restarts reads the new spec: %q", atRestart)
	}
	// A runner on another protocol goes the same way (restartStale).
	atRestart = ""
	r.sup.SetModel(sess.ID, "sonnet")
	r.run[sess.ID].protocol = Protocol + 1
	r.reconcile()
	if !strings.Contains(atRestart, "--model sonnet") {
		t.Fatalf("a stale runner is restarted on a fresh spec too: %q", atRestart)
	}
}

func TestAFailedSpecRebuildDoesNotRestart(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.failSidecar()
	if err := r.sup.Restart(sess.ID); err == nil || !strings.Contains(err.Error(), "no node") {
		t.Fatalf("the restart fails with the reason: %v", err)
	}
	if r.units.count("restart") != 0 || r.units.count("start") != 1 {
		t.Fatalf("the running session is left alone: %v", r.units.calls)
	}
}

func TestASessionInAWorkspaceGetsAPaneAndIsAnnouncedBeforeItStarts(t *testing.T) {
	r := newRig(t)
	var heard []string
	r.sup.Changed = func(s store.RunnerSession) {
		heard = append(heard, s.Wanted+" pane="+s.PaneID+" starts="+strings.Repeat("x", r.units.count("start")))
	}
	sess, err := r.sup.Create(NewSession{Cwd: "/r/app", WorkspaceID: "ws1"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.PaneID == "" || sess.PaneID == sess.ID || sess.WorkspaceID != "ws1" {
		t.Fatalf("a pane id of its own, in its workspace: %+v", sess)
	}
	if got, _ := r.st.RunnerSession(sess.ID); got.PaneID != sess.PaneID || got.WorkspaceID != "ws1" {
		t.Fatalf("the row keeps both: %+v", got)
	}
	if !slices.Equal(heard, []string{"running pane=" + sess.PaneID + " starts="}) {
		t.Fatalf("announced once, before the unit started: %v", heard)
	}
	alone, _ := r.sup.Create(NewSession{Cwd: "/r/alone"})
	if alone.PaneID != "" {
		t.Fatalf("no workspace, no pane: %+v", alone)
	}
}

func TestEveryChangeOfWhatASessionWantsIsAnnounced(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app", WorkspaceID: "ws1"})
	var heard []string
	r.sup.Changed = func(s store.RunnerSession) { heard = append(heard, s.Wanted) }
	r.sup.SetWanted(sess.ID, store.SessionAsleep)
	r.sup.Send(sess.ID, "wake up", "you") // a prompt wakes it
	r.sup.Send(sess.ID, "and again", "you")
	unit := r.paths.Unit(sess.ID)
	r.units.set(unit, false)
	r.units.result[unit] = startLimitHit
	r.reconcile() // parked
	r.sup.Restart(sess.ID)
	r.sup.SetWanted(sess.ID, store.SessionDone)
	want := []string{store.SessionAsleep, store.SessionRunning, store.SessionAsleep, store.SessionRunning, store.SessionDone}
	if !slices.Equal(heard, want) {
		t.Fatalf("heard %v, want %v", heard, want)
	}
}

func TestAPromptNamedTwiceIsQueuedOnce(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	const id = "3f0e1d2c-0000-5000-8000-000000000001"
	for i := 0; i < 2; i++ {
		got, err := r.sup.SendAs(sess.ID, id, "from the bus", "backend")
		if err != nil || got != id {
			t.Fatalf("send %d: %q %v", i, got, err)
		}
	}
	open, _ := r.st.UndeliveredPrompts(sess.ID)
	if len(open) != 1 || open[0].ID != id || open[0].Sender != "backend" {
		t.Fatalf("queued once under its own id: %+v", open)
	}
	r.reconcile()
	r.run[sess.ID].transcript[id] = true
	r.reconcile() // delivered
	if _, err := r.sup.SendAs(sess.ID, id, "from the bus", "backend"); err != nil {
		t.Fatal(err)
	}
	if open, _ := r.st.UndeliveredPrompts(sess.ID); len(open) != 0 {
		t.Fatalf("a delivered prompt handed over again is not queued again: %+v", open)
	}
	r.sup.SetWanted(sess.ID, store.SessionDone)
	if _, err := r.sup.SendAs(sess.ID, "another", "late", "backend"); !errors.Is(err, ErrSessionDone) {
		t.Fatalf("an ended session takes none: %v", err)
	}
}

func TestSendDeliversWithoutWaitingForThePass(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	var tended []string
	r.sup.soon = func(id string) { tended = append(tended, id) }
	r.sup.Send(sess.ID, "now", "you")
	if !slices.Equal(tended, []string{sess.ID}) {
		t.Fatalf("the session is tended as soon as the prompt is queued: %v", tended)
	}
	r.sup.reconcileID(context.Background(), sess.ID) // what soon runs
	if len(r.run[sess.ID].sent) != 1 {
		t.Fatalf("and that delivers it: %v", r.run[sess.ID].sent)
	}
}

func TestASessionsOwnEnvironmentReplacesWhatTheDaemonCarries(t *testing.T) {
	leaked := []string{
		"HOME=/h", "PATH=/bin", "CLAUDECODE=1", "TMUX=x", "CCMUX_PANE_ID=daemons-own", "CCMUX_PANE_TOKEN=t0",
		"CCMUX_DAEMON_URL=http://127.0.0.1:7900", "CCMUX_HOOKS_SOCK=/live/hooks.sock", "CCMUX_PEERS_URL=http://hub",
		"CCMUX_BUS_OWNER=1", "CLAUDE_PEERS_NAME=ccmux", "ANTHROPIC_BASE_URL=http://127.0.0.1:7900/llm/pane/daemons-own",
		"_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1", "CLAUDE_CODE_MAX_MCP_DESCRIPTION_LENGTH=9", "ZDOTDIR=/z", "KEEP=yes",
	}
	cfg := Config{
		Sidecar: func() (string, string, error) { return "/sc/serve.mjs", "/bin/node", nil },
		Env:     func() []string { return leaked },
		Peers:   "/opt/ccmux-peers",
		SessionEnv: func(s store.RunnerSession) map[string]string {
			return map[string]string{"CCMUX_PANE_ID": s.PaneID, "CCMUX_DAEMON_URL": "http://127.0.0.1:7911", "PATH": "/own/bin"}
		},
	}
	paths := Paths{State: "/st", Runtime: "/s"}
	sp, err := cfg.BuildSpec(store.RunnerSession{ID: "s1", Cwd: "/r/app", PaneID: "p1"}, paths)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"HOME=/h", "KEEP=yes", "CCMUX_DAEMON_URL=http://127.0.0.1:7911", "CCMUX_PANE_ID=p1", "PATH=/own/bin"}
	if !slices.Equal(sp.Env, want) {
		t.Fatalf("the daemon's pane, proxy and bus variables are gone, the session's own are set once:\n got %v\nwant %v", sp.Env, want)
	}
	if !strings.Contains(strings.Join(sp.Args, " "), "--peers-command /opt/ccmux-peers") {
		t.Fatalf("a session with a pane loads the daemon's shim: %v", sp.Args)
	}
	bare, _ := cfg.BuildSpec(store.RunnerSession{ID: "s2", Cwd: "/r/app"}, paths)
	if !slices.Equal(bare.Env, []string{"HOME=/h", "PATH=/bin", "KEEP=yes"}) || strings.Contains(strings.Join(bare.Args, " "), "--peers-command") {
		t.Fatalf("a session with no pane gets no identity and no shim, and still none of the daemon's: %v %v", bare.Env, bare.Args)
	}
}

func TestLivenessReadsTheRowAndTheUnit(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	unit := r.paths.Unit(sess.ID)
	live := func(id string) string {
		t.Helper()
		got, err := r.sup.Liveness(id)
		if err != nil {
			t.Fatalf("liveness of %s: %v", id, err)
		}
		return got
	}
	if got := live(sess.ID); got != store.SessionRunning {
		t.Fatalf("wanted and up: %s", got)
	}
	r.units.set(unit, false)
	if got := live(sess.ID); got != store.SessionAsleep {
		t.Fatalf("wanted but down until the next start: %s", got)
	}
	r.units.set(unit, true)
	r.sup.SetWanted(sess.ID, store.SessionAsleep)
	if got := live(sess.ID); got != store.SessionAsleep {
		t.Fatalf("put to sleep, whatever the unit still does: %s", got)
	}
	r.sup.SetWanted(sess.ID, store.SessionDone)
	if live(sess.ID) != store.SessionDone || live("no-such") != store.SessionDone {
		t.Fatal("ended and unknown sessions are done")
	}
}

// lockedRows is a registry whose session reads fail, as a locked database's do.
type lockedRows struct{ Store }

func (lockedRows) RunnerSession(string) (*store.RunnerSession, error) {
	return nil, errors.New("database is locked")
}

// A registry or a systemd that cannot be asked is an error, not a state: a
// running session must never be shown as ended, or as asleep, because of it.
func TestLivenessDoesNotGuessWhenItCannotAsk(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	asleep, _ := r.sup.Create(NewSession{Cwd: "/r/quiet"})
	r.sup.SetWanted(asleep.ID, store.SessionAsleep)

	r.units.activeErr = errors.New("failed to connect to the user bus")
	if got, err := r.sup.Liveness(sess.ID); err == nil || got != "" {
		t.Fatalf("systemd unreachable: got %q %v, want no state and the error", got, err)
	}
	// What the row alone settles needs no systemd, and still answers.
	if got, err := r.sup.Liveness(asleep.ID); err != nil || got != store.SessionAsleep {
		t.Fatalf("a session put to sleep is asleep whatever systemd says: %q %v", got, err)
	}
	if got, err := r.sup.Liveness("no-such"); err != nil || got != store.SessionDone {
		t.Fatalf("a session with no row is done: %q %v", got, err)
	}
	r.units.activeErr = nil

	real := r.sup.st
	r.sup.st = lockedRows{real}
	if got, err := r.sup.Liveness(sess.ID); err == nil || got != "" {
		t.Fatalf("registry unreadable: got %q %v, want no state and the error, never done", got, err)
	}
	r.sup.st = real
	if got, err := r.sup.Liveness(sess.ID); err != nil || got != store.SessionRunning {
		t.Fatalf("and once both answer again: %q %v", got, err)
	}
}

func TestACardIsAnsweredOnceThroughTheRunner(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	fr := r.run[sess.ID]
	fr.cards = []map[string]string{{"id": "abcde", "permission": "Bash"}}
	fr.questions = []map[string]any{{"id": "fghij", "questions": []any{map[string]string{"question": "red or blue?"}, map[string]string{"question": "why?"}}}}
	ctx := context.Background()
	if ok, err := r.sup.ReplyCard(ctx, sess.ID, CardReply{Kind: "permission", ID: "abcde", Allow: true}); !ok || err != nil {
		t.Fatalf("allowed: %v %v", ok, err)
	}
	if ok, err := r.sup.ReplyCard(ctx, sess.ID, CardReply{Kind: "question", ID: "fghij", Answer: "blue"}); !ok || err != nil {
		t.Fatalf("answered: %v %v", ok, err)
	}
	want := []string{`/permission/abcde/reply {"reply":"once"}`, `/question/fghij/reply {"answers":[["blue"],["blue"]]}`}
	if !slices.Equal(fr.replies, want) {
		t.Fatalf("got %v, want %v", fr.replies, want)
	}
	if ok, err := r.sup.ReplyCard(ctx, sess.ID, CardReply{Kind: "permission", ID: "zzzzz"}); ok || err != nil {
		t.Fatalf("a card no longer pending is not an error and not answered: %v %v", ok, err)
	}
	// The id of a question in the permission verb is no pending permission.
	if ok, _ := r.sup.ReplyCard(ctx, sess.ID, CardReply{Kind: "permission", ID: "fghij"}); ok {
		t.Fatal("a reply in the other verb does not claim the card")
	}
	// A denial rejects the card: it must never reach the runner as an allow.
	fr.replies = nil
	if ok, err := r.sup.ReplyCard(ctx, sess.ID, CardReply{Kind: "permission", ID: "abcde", Allow: false}); !ok || err != nil {
		t.Fatalf("denied: %v %v", ok, err)
	}
	if !slices.Equal(fr.replies, []string{`/permission/abcde/reply {"reply":"reject"}`}) {
		t.Fatalf("a denial is sent as reject, and nothing else is: %v", fr.replies)
	}
}
