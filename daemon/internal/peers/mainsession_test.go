package peers

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// mainBus is a window SHOP with two repos, app and backend, on a host that
// runs sessions. A sender is on tmux pane "tmuxa" (app: any session that is
// routed to no helper of its own, as a role agent or a task builder is).
// backend's main session, when it has one, is runner pane "mainb"
// (backend-s-0a1b2c3d); StartMain makes it, as the daemon would.
type mainBus struct {
	svc  *Service
	hook *fakeHook
	box  *inbox
	app  string // the sender's peer id

	mu       sync.Mutex
	runners  map[string]RunnerInfo // runner pane → what the daemon says of it
	started  []string              // the workspace of each StartMain call
	startErr error
	offBus   bool // StartMain makes a session whose peer never comes
}

func newMainBus(t *testing.T) *mainBus {
	t.Helper()
	svc, hook, _ := newTestServiceWithStore(t)
	b := &mainBus{svc: svc, hook: hook, box: &inbox{}, runners: map[string]RunnerInfo{}}
	for _, pane := range []string{"tmuxa", "tmuxb", "mainb", "maina", "helpb"} {
		hook.groups[pane] = "SHOP"
	}
	hook.closed = map[string]string{"SHOP\x00backend": "ws-backend:/r/backend", "SHOP\x00app": "ws-app:/r/app"}
	svc.RunnerPane = func(paneID string) (RunnerInfo, bool) {
		b.mu.Lock()
		defer b.mu.Unlock()
		info, ok := b.runners[paneID]
		return info, ok
	}
	svc.RunnerInbox = b.box.take
	svc.StartMain = b.startMain
	b.app = registerPane(svc, "tmuxa", "/r/app").PeerID
	return b
}

// startMain is the daemon's part: the session is made, and its peer put on
// the bus before the call returns.
func (b *mainBus) startMain(wsID string) (string, string, error) {
	b.mu.Lock()
	b.started = append(b.started, wsID)
	err, offBus := b.startErr, b.offBus
	b.mu.Unlock()
	if err != nil {
		return "", "", err
	}
	if offBus {
		return "nopane", "/r/backend", nil
	}
	b.haveMain("mainb", "backend", false)
	return "mainb", "/r/backend", nil
}

// haveMain puts repo's main session on the bus, on runner pane.
func (b *mainBus) haveMain(pane, repo string, asleep bool) string {
	b.setRunner(pane, RunnerInfo{Name: repo + "-s-0a1b2c3d", Repo: "/r/" + repo, Asleep: asleep, Main: true})
	b.svc.EnsureRunnerPeer(pane, "/r/"+repo)
	return derivedID(pane)
}

func (b *mainBus) setRunner(pane string, info RunnerInfo) {
	b.mu.Lock()
	b.runners[pane] = info
	b.mu.Unlock()
}

func (b *mainBus) starts() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.started...)
}

// to is what reached a runner pane's session, as "sender|text".
func (b *mainBus) to(pane string) []string {
	var out []string
	for _, q := range b.box.got() {
		if f := strings.SplitN(q, "|", 4); f[0] == pane {
			out = append(out, f[2]+"|"+f[3])
		}
	}
	return out
}

func (b *mainBus) mail(t *testing.T, peerID string) []string {
	t.Helper()
	evs, err := b.svc.Poll(peerID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		out = append(out, ev.FromName+"|"+ev.Text)
	}
	return out
}

// settle gives goroutines that should do nothing the time to do it.
func settle() { time.Sleep(60 * time.Millisecond) }

func TestARepoPlainNameReachesItsAsleepMainSessionAndStartsNothing(t *testing.T) {
	for _, spawn := range []bool{false, true} {
		b := newMainBus(t)
		b.haveMain("mainb", "backend", true)
		resp := b.svc.Send(SendReq{FromID: b.app, ToName: "backend", Text: "what does GET /trades return?", SpawnIfMissing: spawn})
		if !resp.OK || resp.Spawning || resp.Queued {
			t.Fatalf("spawn_if_missing=%v: send = %+v, want delivered to the main session", spawn, resp)
		}
		eventually(t, "the message in the main session's inbox", func() bool { return len(b.to("mainb")) == 1 })
		if got := b.to("mainb")[0]; !strings.HasPrefix(got, "app|") || !strings.Contains(got, "GET /trades") || !strings.Contains(got, "to_id="+b.app) {
			t.Fatalf("the main session got %q, want app's message with its id to reply to", got)
		}
		settle()
		if n := b.hook.spawnCount(); n != 0 || len(b.starts()) != 0 || len(b.to("mainb")) != 1 {
			t.Fatalf("spawn_if_missing=%v: %d terminal spawns, starts %v, %d prompts; want none, none, one", spawn, n, b.starts(), len(b.to("mainb")))
		}
	}
}

func TestAPeerCalledExactlyTheRepoWinsOverItsMainSession(t *testing.T) {
	b := newMainBus(t)
	main := b.haveMain("mainb", "backend", true)
	listed := func() string {
		for _, e := range b.svc.List(b.app, "project", "") {
			if e.ID == main {
				return e.Name + "/" + e.AnswersTo
			}
		}
		return "not listed"
	}
	if got := listed(); got != "backend-s-0a1b2c3d/backend" {
		t.Fatalf("the main session is listed as %q, want it to answer to backend", got)
	}
	term := registerPane(b.svc, "tmuxb", "/r/backend").PeerID // a terminal Claude a person started
	if resp := b.svc.Send(SendReq{FromID: b.app, ToName: "backend", Text: "hi"}); !resp.OK {
		t.Fatalf("send = %+v", resp)
	}
	if got := b.mail(t, term); len(got) != 1 || got[0] != "app|hi" {
		t.Fatalf("the terminal session's mail = %v, want the message", got)
	}
	settle()
	if got := b.to("mainb"); len(got) != 0 {
		t.Fatalf("the main session also got %v", got)
	}
	if got := listed(); got != "backend-s-0a1b2c3d/" {
		t.Fatalf("with a peer called backend there, the main session is listed as %q, want no plain name", got)
	}
}

// A main session naming another repo reaches its own side helper, and a
// thread member its thread's helper: never that repo's main session.
func TestHelperRoutingStillWinsOverTheMainSession(t *testing.T) {
	for sender, helper := range map[string]string{"maina": "backend-for-app", "lead": "backend-thread-1"} {
		b := newMainBus(t)
		b.hook.groups["lead"] = "SHOP"
		b.haveMain("mainb", "backend", true)
		b.setRunner("lead", RunnerInfo{Name: "app-thread-1", Repo: "/r/app"})
		b.svc.EnsureRunnerPeer("lead", "/r/app")
		b.haveMain("maina", "app", false)
		b.svc.ThreadRoute = func(senderPane, toName string) (HelperRoute, bool) {
			if senderPane != sender || toName != "backend" {
				return HelperRoute{}, false
			}
			return HelperRoute{ThreadID: 1, Repo: "/r/backend", Name: helper}, true
		}
		b.svc.StartHelper = func(r HelperRoute) error {
			b.setRunner("helpb", RunnerInfo{Name: r.Name, Repo: r.Repo})
			go b.svc.EnsureRunnerPeer("helpb", r.Repo)
			return nil
		}
		if resp := b.svc.Send(SendReq{FromID: derivedID(sender), ToName: "backend", Text: "a question"}); !resp.OK || !resp.Spawning {
			t.Fatalf("%s: send = %+v, want its helper started", sender, resp)
		}
		eventually(t, sender+"'s helper getting the question", func() bool { return len(b.to("helpb")) == 1 })
		settle()
		if got := b.to("mainb"); len(got) != 0 || len(b.starts()) != 0 {
			t.Fatalf("%s: backend's main session got %v (starts %v), want nothing", sender, got, b.starts())
		}
	}
}

func TestSpawnIfMissingStartsTheMainSessionAndItsFirstPromptIsTheMessage(t *testing.T) {
	b := newMainBus(t)
	resp := b.svc.Send(SendReq{FromID: b.app, ToName: "backend", Text: "add a users route", SpawnIfMissing: true})
	if !resp.OK || !resp.Spawning {
		t.Fatalf("send = %+v, want a start under way", resp)
	}
	eventually(t, "the message in the new main session's inbox", func() bool { return len(b.to("mainb")) == 1 })
	if got := b.to("mainb")[0]; !strings.HasPrefix(got, "app|") || !strings.Contains(got, "add a users route") {
		t.Fatalf("the new main session's first prompt = %q", got)
	}
	settle()
	if got := b.starts(); len(got) != 1 || got[0] != "ws-backend" || b.hook.spawnCount() != 0 {
		t.Fatalf("starts %v, %d terminal spawns; want the one main session and no terminal Claude", got, b.hook.spawnCount())
	}
	if got := b.mail(t, b.app); len(got) != 0 {
		t.Fatalf("the sender was told %v, want nothing: the start worked", got)
	}
	// The next message finds the session: nothing is started twice.
	if resp := b.svc.Send(SendReq{FromID: b.app, ToName: "backend", Text: "and tests", SpawnIfMissing: true}); !resp.OK || resp.Spawning {
		t.Fatalf("second send = %+v, want it delivered", resp)
	}
	eventually(t, "the second message", func() bool { return len(b.to("mainb")) == 2 })
	if len(b.starts()) != 1 {
		t.Fatalf("starts %v, want one", b.starts())
	}
}

// What waits for a main session is taken by that session alone: not by a
// helper of the same repo, and not by a peer that registers under the
// repo's plain name while the start is under way.
func TestOnlyTheMainSessionTakesWhatWaitsForIt(t *testing.T) {
	b := newMainBus(t)
	release := make(chan struct{})
	b.svc.StartMain = func(wsID string) (string, string, error) {
		<-release
		return b.startMain(wsID)
	}
	for _, text := range []string{"one", "two"} {
		if resp := b.svc.Send(SendReq{FromID: b.app, ToName: "backend", Text: text, SpawnIfMissing: true}); !resp.Spawning {
			t.Fatalf("send %q = %+v, want it waiting for the start", text, resp)
		}
	}
	b.setRunner("helpb", RunnerInfo{Name: "backend-for-app", Repo: "/r/backend"})
	b.svc.EnsureRunnerPeer("helpb", "/r/backend")
	term := registerPane(b.svc, "tmuxb", "/r/backend").PeerID
	settle()
	if got, mail := b.to("helpb"), b.mail(t, term); len(got) != 0 || len(mail) != 0 {
		t.Fatalf("a helper got %v and a terminal session %v, want the messages still waiting", got, mail)
	}
	close(release)
	eventually(t, "both messages in the main session's inbox", func() bool { return len(b.to("mainb")) == 2 })
	if got := b.to("mainb"); !strings.Contains(got[0], "one") || !strings.Contains(got[1], "two") {
		t.Fatalf("the main session got %v, want one then two", got)
	}
	if got := b.starts(); len(got) != 1 {
		t.Fatalf("starts %v, want the one start both waited for", got)
	}
}

func TestAMainSessionThatCannotStartIsSaidToWhoeverAskedForIt(t *testing.T) {
	for name, set := range map[string]func(*mainBus){
		"could not be started": func(b *mainBus) { b.startErr = errors.New("no slot") },
		"is not on the bus":    func(b *mainBus) { b.offBus = true },
	} {
		b := newMainBus(t)
		set(b)
		if resp := b.svc.Send(SendReq{FromID: b.app, ToName: "backend", Text: "x", SpawnIfMissing: true}); !resp.Spawning {
			t.Fatalf("%s: send = %+v", name, resp)
		}
		var told []string
		eventually(t, "the sender being told: "+name, func() bool { told = append(told, b.mail(t, b.app)...); return len(told) == 1 })
		if !strings.Contains(told[0], name) || !strings.Contains(told[0], `"backend"`) {
			t.Fatalf("the sender was told %q, want that backend's session %s", told[0], name)
		}
		if b.hook.spawnCount() != 0 {
			t.Fatalf("%s: a terminal Claude was started", name)
		}
	}
}

// Where no sessions run, and for a name that is no repo of the window, the
// spawn is the one it always was.
func TestWithoutSessionRunnersSpawnIfMissingIsTheTmuxSpawn(t *testing.T) {
	b := newMainBus(t)
	b.svc.RunnerInbox, b.svc.StartMain = nil, nil
	if resp := b.svc.Send(SendReq{FromID: b.app, ToName: "backend", Text: "x", SpawnIfMissing: true}); !resp.Spawning {
		t.Fatalf("send = %+v", resp)
	}
	eventually(t, "the terminal spawn", func() bool { return b.hook.spawnCount() == 1 })
	if got := b.hook.spawn(0); !strings.HasPrefix(got, "ws-backend|"+claudeLaunchPrefix) || len(b.starts()) != 0 {
		t.Fatalf("spawn = %q, starts %v; want the frozen claude launch line in backend's session", got, b.starts())
	}
	resp := b.svc.Send(SendReq{FromID: b.app, ToName: "backend2", Text: "x"})
	if want := `Peer "backend2" not found in project: it is not running. Resend with spawn_if_missing=true to start it`; resp.Error != want {
		t.Fatalf("miss = %q, want the old sentence %q", resp.Error, want)
	}
}

func TestANameThatIsNoRepoOfTheWindowKeepsTheOldSpawnAndItsWords(t *testing.T) {
	b := newMainBus(t)
	resp := b.svc.Send(SendReq{FromID: b.app, ToName: "ghost", Text: "x", SpawnIfMissing: true})
	if resp.OK || !strings.HasPrefix(resp.Error, `Cannot spawn "ghost": repo path /r/ghost does not exist`) || len(b.starts()) != 0 {
		t.Fatalf("send = %+v, starts %v; want the old repo-guess error", resp, b.starts())
	}
	resp = b.svc.Send(SendReq{FromID: b.app, ToName: "ghost", Text: "x"})
	if want := `Peer "ghost" not found in project: it is not running. Resend with spawn_if_missing=true to start it`; resp.Error != want {
		t.Fatalf("miss = %q, want the old sentence", resp.Error)
	}
}

func TestTheMissSaysWhatSpawnIfMissingWillStartAndWhereTheSessionIs(t *testing.T) {
	b := newMainBus(t)
	resp := b.svc.Send(SendReq{FromID: b.app, ToName: "backend", Text: "x"})
	if resp.OK || !strings.HasPrefix(resp.Error, `Peer "backend" not found in project: `) ||
		!strings.Contains(resp.Error, "spawn_if_missing=true") || !strings.Contains(resp.Error, "main session") || !strings.Contains(resp.Error, "no terminal Claude") {
		t.Fatalf("miss = %+v, want it to say a main session is started, not a terminal", resp)
	}
	// backend's main session is in another window: the miss names it.
	b.hook.setGroup("mainb", "OTHER")
	b.haveMain("mainb", "backend", true)
	resp = b.svc.Send(SendReq{FromID: b.app, ToName: "backend", Text: "x"})
	if want := `Peer "backend" not found in project: it is running in "OTHER". Pass to_group="OTHER" to reach it`; resp.Error != want {
		t.Fatalf("miss = %q, want %q", resp.Error, want)
	}
	if resp := b.svc.Send(SendReq{FromID: b.app, ToName: "backend", ToGroup: "other", Text: "over the wall"}); !resp.OK {
		t.Fatalf("send with to_group = %+v, want it delivered", resp)
	}
	eventually(t, "the message across groups", func() bool { return len(b.to("mainb")) == 1 })
}

func TestAMainSessionNamingItsOwnRepoIsToldSoAndNothingStarts(t *testing.T) {
	b := newMainBus(t)
	main := b.haveMain("mainb", "backend", false)
	for _, spawn := range []bool{false, true} {
		resp := b.svc.Send(SendReq{FromID: main, ToName: "backend", Text: "x", SpawnIfMissing: spawn})
		if resp.OK || resp.Spawning || !strings.Contains(resp.Error, "is you") {
			t.Fatalf("spawn_if_missing=%v: send = %+v, want it told that backend is itself", spawn, resp)
		}
	}
	settle()
	if len(b.starts()) != 0 || b.hook.spawnCount() != 0 || len(b.to("mainb")) != 0 {
		t.Fatalf("starts %v, %d terminal spawns, prompts %v; want nothing", b.starts(), b.hook.spawnCount(), b.to("mainb"))
	}
}

func TestAQuietNoticeToARepoWaitsForItsMainSessionAndWakesNobody(t *testing.T) {
	b := newMainBus(t)
	// An archived terminal session of backend's left its record behind: the
	// notice is for the session that will read it.
	old := registerPane(b.svc, "tmuxb", "/r/backend").PeerID
	b.svc.Unregister(old)
	main := b.haveMain("mainb", "backend", true)
	resp := b.svc.Send(SendReq{FromID: b.app, ToName: "backend", Text: "FYI: the schema moved", Quiet: true})
	if !resp.OK || !resp.Queued || resp.Spawning {
		t.Fatalf("quiet send = %+v, want ok and queued", resp)
	}
	b.svc.ReapOnce() // the sweep drains every runner's mailbox
	settle()
	if got := b.to("mainb"); len(got) != 0 || len(b.starts()) != 0 {
		t.Fatalf("a quiet notice reached the asleep session as a prompt: %v (starts %v)", got, b.starts())
	}
	if pending, err := b.svc.st.PendingNotices(main); err != nil || len(pending) != 1 {
		t.Fatalf("notices waiting for the main session = %v %v, want the one", pending, err)
	}
	if pending, _ := b.svc.st.PendingNotices(old); len(pending) != 0 {
		t.Fatalf("the archived terminal session holds the notice: %v", pending)
	}
	// It rides along with the next message, once.
	b.svc.Send(SendReq{FromID: b.app, ToName: "backend", Text: "now a question"})
	eventually(t, "the next message", func() bool { return len(b.to("mainb")) == 1 })
	if got := b.to("mainb")[0]; !strings.Contains(got, "now a question") || !strings.Contains(got, "FYI: the schema moved") {
		t.Fatalf("the next message = %q, want the notice folded in", got)
	}
}

func TestAQuietNoticeNeverStartsAMainSession(t *testing.T) {
	b := newMainBus(t)
	resp := b.svc.Send(SendReq{FromID: b.app, ToName: "backend", Text: "FYI", Quiet: true, SpawnIfMissing: true})
	if resp.OK || !strings.Contains(resp.Error, "a quiet notice only waits") {
		t.Fatalf("quiet send = %+v, want the old refusal", resp)
	}
	settle()
	if len(b.starts()) != 0 || b.hook.spawnCount() != 0 {
		t.Fatalf("a quiet notice started something: %v, %d", b.starts(), b.hook.spawnCount())
	}
}

func TestADelegationToARepoReachesItsMainSessionOrStartsIt(t *testing.T) {
	b := newMainBus(t)
	resp := b.svc.Delegate(DelegateReq{FromID: b.app, ToName: "backend", Text: "run the migration check", SpawnIfMissing: true})
	if !resp.OK || !resp.Spawning || resp.TaskID == "" {
		t.Fatalf("delegate = %+v, want a task and a start under way", resp)
	}
	eventually(t, "the delegation in the new main session's inbox", func() bool { return len(b.to("mainb")) == 1 })
	if got := b.to("mainb")[0]; !strings.Contains(got, "[claude-peers delegation task "+resp.TaskID+"]") || !strings.Contains(got, "run the migration check") {
		t.Fatalf("the main session got %q, want the headered delegation", got)
	}
	// The session that was started reports on the delegation that started
	// it: the row had no worker yet, and its first report makes it the one.
	main := derivedID("mainb")
	if up := b.svc.UpdateTask(TaskUpdateReq{PeerID: main, TaskID: resp.TaskID, Status: "completed", Result: "no drift"}); !up.OK {
		t.Fatalf("the new main session's report = %+v, want it taken", up)
	}
	if task, err := b.svc.st.PeerTask(resp.TaskID); err != nil || task == nil || task.ToID != main || task.Status != "completed" {
		t.Fatalf("the first task's row = %+v %v, want it closed by the main session", task, err)
	}
	if got := b.mail(t, b.app); len(got) != 1 || !strings.Contains(got[0], resp.TaskID) || !strings.Contains(got[0], "no drift") {
		t.Fatalf("the delegator heard %v, want the one update with the result", got)
	}
	// With the session there, the next one is delivered to it, tracked.
	next := b.svc.Delegate(DelegateReq{FromID: b.app, ToName: "backend", Text: "and the indexes"})
	if !next.OK || next.Spawning || next.TaskID == "" {
		t.Fatalf("second delegate = %+v, want it delivered", next)
	}
	eventually(t, "the second delegation", func() bool { return len(b.to("mainb")) == 2 })
	if task, err := b.svc.st.PeerTask(next.TaskID); err != nil || task == nil || task.ToID != derivedID("mainb") {
		t.Fatalf("the task's worker = %+v %v, want the main session", task, err)
	}
	if len(b.starts()) != 1 || b.hook.spawnCount() != 0 {
		t.Fatalf("starts %v, %d terminal spawns; want one start", b.starts(), b.hook.spawnCount())
	}
}

func TestAnAgentNameKeepsItsOwnStart(t *testing.T) {
	b := newMainBus(t)
	b.hook.closed["SHOP\x00radar"] = "ws-radar:/r/radar" // even with a folder of that name in the window
	started := make(chan string, 1)
	b.svc.IsAgent = func(name string) (bool, error) { return name == "radar", nil }
	b.svc.StartAgent = func(group, name, prompt string) error { started <- group + "/" + name; return nil }
	if resp := b.svc.Send(SendReq{FromID: b.app, ToName: "radar", Text: "morning run", SpawnIfMissing: true}); !resp.Spawning {
		t.Fatalf("send = %+v", resp)
	}
	select {
	case got := <-started:
		if got != "SHOP/radar" {
			t.Fatalf("agent started as %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the agent was not started")
	}
	if len(b.starts()) != 0 || b.hook.spawnCount() != 0 {
		t.Fatalf("starts %v, %d terminal spawns; want the agent's own start only", b.starts(), b.hook.spawnCount())
	}
}
