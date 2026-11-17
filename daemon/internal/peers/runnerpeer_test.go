package peers

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// inbox is a runner session's inbox as the bus sees it: what was queued, in
// order, and an error to refuse with.
type inbox struct {
	mu     sync.Mutex
	queued []string // "paneID|promptID|sender|text"
	refuse error
}

func (b *inbox) take(paneID, promptID, text, sender string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.refuse != nil {
		return b.refuse
	}
	b.queued = append(b.queued, paneID+"|"+promptID+"|"+sender+"|"+text)
	return nil
}

func (b *inbox) got() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.queued...)
}

func (b *inbox) setRefuse(err error) {
	b.mu.Lock()
	b.refuse = err
	b.mu.Unlock()
}

// runnerBus is a bus with one runner session's pane "run1" in window APP
// (bus name app-s-0a1b2c3d, repo /r/app) and a tmux pane "tmux1" beside it.
// The last value sets where the session is: "asleep", "running", or "done",
// which ends it (its pane stops being a runner's).
func runnerBus(t *testing.T) (*Service, *fakeHook, *inbox, func(state string)) {
	t.Helper()
	svc, hook, _ := newTestServiceWithStore(t)
	hook.groups["run1"], hook.groups["tmux1"] = "APP", "APP"
	var mu sync.Mutex
	state := "running"
	svc.RunnerPane = func(paneID string) (RunnerInfo, bool) {
		mu.Lock()
		defer mu.Unlock()
		return RunnerInfo{Name: "app-s-0a1b2c3d", Repo: "/r/app", Asleep: state == "asleep"}, state != "done" && paneID == "run1"
	}
	box := &inbox{}
	svc.RunnerInbox = box.take
	return svc, hook, box, func(to string) { mu.Lock(); state = to; mu.Unlock() }
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// With session runners off (no inbox wired) a runner session's pane is no
// runner peer: kept present, it would take messages nothing delivers.
func TestWithNoInboxARunnersPaneIsNoRunnerPeer(t *testing.T) {
	svc, _, _, _ := runnerBus(t)
	svc.RunnerInbox = nil
	svc.EnsureRunnerPeer("run1", "/r/app")
	if p := svc.peers[derivedID("run1")]; p != nil {
		t.Fatalf("a runner peer was made with nowhere to deliver to: %+v", p)
	}
}

func TestTheDaemonRegistersARunnersPeerAndItStaysPresent(t *testing.T) {
	svc, hook, _, _ := runnerBus(t)
	svc.EnsureRunnerPeer("run1", "/r/app")
	svc.EnsureRunnerPeer("run1", "/r/app") // idempotent
	lead := registerPane(svc, "tmux1", "/r/app")
	list := svc.List(lead.PeerID, "project", "")
	if len(list) != 1 || list[0].Name != "app-s-0a1b2c3d" || list[0].Group != "APP" || !list[0].PollOnly {
		t.Fatalf("listed in its window's group under the daemon's name, with no shim yet: %+v", list)
	}
	id := list[0].ID
	if id != derivedID("run1") {
		t.Fatalf("its id is its pane's, like any pane peer: %s", id)
	}
	// Its shim leaving (the unit stopped: asleep), its hooks saying the
	// session ended, and time passing do not make it absent: a message
	// wakes it.
	svc.Unregister(id)
	hook.setShell("run1", true)
	now := time.Now()
	svc.Now = func() time.Time { return now.Add(time.Hour) }
	svc.ReapOnce()
	if got := svc.List(lead.PeerID, "project", ""); len(got) != 1 {
		t.Fatalf("an asleep runner session stays messageable: %+v", got)
	}
	if svc.PeerForPane("run1") != id {
		t.Fatal("and a card it raises still finds its peer")
	}
	svc.EnsureRunnerPeer("tmux1", "/r/app")
	if p := svc.peers[derivedID("tmux1")]; p.Name != "app" {
		t.Fatalf("a tmux pane's peer is not the daemon's to make or name: %+v", p)
	}
}

func TestARunnersShimRegistersUnderTheDaemonsName(t *testing.T) {
	svc, _, _, _ := runnerBus(t)
	svc.EnsureRunnerPeer("run1", "/r/app")
	// The shim says what any shim says: its own folder's name.
	got := svc.Register(RegisterReq{PaneID: "run1", PID: 4242, CWD: "/w/app-task-9", GitRoot: "/w/app-task-9", Name: "app", PollOnly: true})
	if got.PeerID != derivedID("run1") || got.Name != "app-s-0a1b2c3d" || got.Group != "APP" {
		t.Fatalf("the same peer, named by the daemon: %+v", got)
	}
	p := svc.peers[got.PeerID]
	if p.GitRoot != "/r/app" || p.CWD != "/w/app-task-9" || p.PID != 4242 {
		t.Fatalf("its repo is its workspace's, the rest is what it sent: %+v", p)
	}
	if !svc.AuthorizePeer(got.PeerID, TokenForPane(svc.secret, "run1")) {
		t.Fatal("it acts with its pane's token")
	}
}

func TestAMessageToARunnerArrivesOnceAsAPrompt(t *testing.T) {
	svc, _, box, _ := runnerBus(t)
	svc.EnsureRunnerPeer("run1", "/r/app")
	lead := registerPane(svc, "tmux1", "/r/app")
	resp := svc.Send(SendReq{FromID: lead.PeerID, ToName: "app-s-0a1b2c3d", Text: "review the diff"})
	if !resp.OK || resp.Queued {
		t.Fatalf("sent to a present peer: %+v", resp)
	}
	eventually(t, "the prompt", func() bool { return len(box.got()) == 1 })
	id := derivedID("run1")
	want := fmt.Sprintf("run1|%s|app|[claude-peers message from app (id %s)]\nreview the diff\n\nReply with send_message to_id=%s.",
		InboxPromptID(id, 1), lead.PeerID, lead.PeerID)
	if got := box.got()[0]; got != want {
		t.Fatalf("the prompt names its sender and how to answer:\n got %q\nwant %q", got, want)
	}
	eventually(t, "the cursor", func() bool { c, _ := svc.st.PeerCursor(id); return c == 1 })
	// check_messages hands the session nothing the inbox already took, and
	// asking again queues nothing new.
	evs, err := svc.Poll(id)
	if err != nil || len(evs) != 0 {
		t.Fatalf("poll: %v %v", evs, err)
	}
	svc.ReapOnce()
	svc.EnsureRunnerPeer("run1", "/r/app")
	time.Sleep(50 * time.Millisecond)
	if n := len(box.got()); n != 1 {
		t.Fatalf("queued %d times, want once", n)
	}
	// A tmux pane's peer still polls its mailbox as before.
	svc.Send(SendReq{FromID: id, ToID: lead.PeerID, Text: "done"})
	if evs, _ := svc.Poll(lead.PeerID); len(evs) != 1 || evs[0].Text != "done" || evs[0].FromName != "app-s-0a1b2c3d" {
		t.Fatalf("the runner's reply reaches the tmux session's mailbox: %+v", evs)
	}
}

func TestARefusedInboxKeepsTheMessageAndItsPlace(t *testing.T) {
	svc, _, box, _ := runnerBus(t)
	svc.EnsureRunnerPeer("run1", "/r/app")
	lead := registerPane(svc, "tmux1", "/r/app")
	id := derivedID("run1")
	box.setRefuse(errors.New("database is locked"))
	svc.Send(SendReq{FromID: lead.PeerID, ToID: id, Text: "first"})
	svc.Send(SendReq{FromID: lead.PeerID, ToID: id, Text: "second"})
	time.Sleep(50 * time.Millisecond)
	if c, _ := svc.st.PeerCursor(id); c != 0 || len(box.got()) != 0 {
		t.Fatalf("nothing handed over, nothing skipped: cursor %d, queued %v", c, box.got())
	}
	if evs, _ := svc.Poll(id); len(evs) != 0 {
		t.Fatalf("and check_messages does not show them in the meantime: %+v", evs)
	}
	box.setRefuse(nil)
	svc.ReapOnce() // the sweep tries again
	eventually(t, "both prompts", func() bool { return len(box.got()) == 2 })
	got := box.got()
	if !strings.Contains(got[0], "\nfirst\n") || !strings.Contains(got[1], "\nsecond\n") {
		t.Fatalf("in the order they were sent: %v", got)
	}
	if !strings.Contains(got[0], InboxPromptID(id, 1)) || !strings.Contains(got[1], InboxPromptID(id, 2)) {
		t.Fatalf("each under the id its message gives it, so a repeat queues nothing: %v", got)
	}
	// A session that has ended reads nothing: the drain moves past.
	box.setRefuse(ErrInboxClosed)
	svc.Send(SendReq{FromID: lead.PeerID, ToID: id, Text: "too late"})
	eventually(t, "the cursor past the dropped message", func() bool { c, _ := svc.st.PeerCursor(id); return c == 3 })
}

func TestAVerdictForARunnersCardIsNotAPrompt(t *testing.T) {
	svc, _, box, _ := runnerBus(t)
	svc.EnsureRunnerPeer("run1", "/r/app")
	lead := registerPane(svc, "tmux1", "/r/app")
	id := derivedID("run1")
	replies := make(chan PaneReply, 1)
	svc.ReplyToPane = func(paneID string, r PaneReply) error {
		if paneID == "run1" {
			replies <- r
		}
		return nil
	}
	svc.Send(SendReq{FromID: lead.PeerID, ToID: id, Text: "please run the migration check"}) // lead delegated
	eventually(t, "the delegation prompt", func() bool { return len(box.got()) == 1 })
	if n, err := svc.PermissionRequest(id, "abcde", "Bash", "run it", "make check"); err != nil || n != 1 {
		t.Fatalf("the card is relayed to its delegator: %d %v", n, err)
	}
	if resp := svc.Send(SendReq{FromID: lead.PeerID, ToID: id, Text: "yes abcde"}); !resp.OK {
		t.Fatalf("verdict: %+v", resp)
	}
	select {
	case r := <-replies:
		if r.Kind != AskPermission || r.RequestID != "abcde" || r.Behavior != "allow" {
			t.Fatalf("reply %+v", r)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the verdict never reached the runner's card")
	}
	eventually(t, "the cursor past the verdict", func() bool { c, _ := svc.st.PeerCursor(id); return c >= 2 })
	if n := len(box.got()); n != 1 {
		t.Fatalf("the verdict was typed to the session as a prompt: %v", box.got())
	}
}

func TestAnEndedRunnerLeavesTheBus(t *testing.T) {
	svc, hook, _, set := runnerBus(t)
	svc.EnsureRunnerPeer("run1", "/r/app")
	lead := registerPane(svc, "tmux1", "/r/app")
	set("done") // its session ended
	hook.dropGroup("run1")
	if got := svc.List(lead.PeerID, "project", ""); len(got) != 0 {
		t.Fatalf("an ended session is not listed: %+v", got)
	}
	now := time.Now()
	svc.ReapOnce() // first miss starts the grace
	svc.Now = func() time.Time { return now.Add(substrateGrace + time.Second) }
	svc.ReapOnce()
	if svc.peers[derivedID("run1")] != nil {
		t.Fatal("its peer and mailbox are reaped once its pane has stayed gone")
	}
}

func TestTheDaemonTakesAnEndedRunnersPeerOffTheBusAtOnce(t *testing.T) {
	svc, _, _, set := runnerBus(t)
	svc.EnsureRunnerPeer("run1", "/r/app")
	lead := registerPane(svc, "tmux1", "/r/app")
	svc.Send(SendReq{FromID: lead.PeerID, ToID: derivedID("run1"), Text: "hello"})
	set("done")
	svc.ForgetRunnerPeer("run1")
	svc.ForgetRunnerPeer("run1")  // twice is nothing
	svc.ForgetRunnerPeer("tmux9") // nor is a pane with no peer
	if svc.peers[derivedID("run1")] != nil || svc.PeerForPane("run1") != "" {
		t.Fatal("gone with its session, not after the sweep's grace")
	}
	if resp := svc.Send(SendReq{FromID: lead.PeerID, ToName: "app-s-0a1b2c3d", Text: "anyone?"}); resp.OK {
		t.Fatalf("a message to an ended session is refused, not queued for nobody: %+v", resp)
	}
	if svc.peers[lead.PeerID] == nil {
		t.Fatal("other peers are untouched")
	}
}

// A delegation is a message with a task behind it: it reaches a runner the
// same way, once, and the task is open against the runner's peer.
func TestADelegationToARunnerArrivesOnceAsAPrompt(t *testing.T) {
	svc, _, box, _ := runnerBus(t)
	svc.EnsureRunnerPeer("run1", "/r/app")
	lead := registerPane(svc, "tmux1", "/r/app")
	resp := svc.Delegate(DelegateReq{FromID: lead.PeerID, ToName: "app-s-0a1b2c3d", Text: "run the migration check"})
	if !resp.OK || resp.TaskID == "" || resp.Queued {
		t.Fatalf("delegated to a present peer: %+v", resp)
	}
	eventually(t, "the delegation prompt", func() bool { return len(box.got()) == 1 })
	got := box.got()[0]
	if !strings.Contains(got, "[claude-peers delegation task "+resp.TaskID+"]") || !strings.Contains(got, "run the migration check") ||
		!strings.Contains(got, "[claude-peers message from app (id "+lead.PeerID+")]") {
		t.Fatalf("the prompt carries the task header, the work and who asked: %q", got)
	}
	if n := svc.OpenTaskCountForPane("run1"); n != 1 {
		t.Fatalf("the runner owes its delegator one task: %d", n)
	}
	svc.ReapOnce()
	time.Sleep(50 * time.Millisecond)
	if n := len(box.got()); n != 1 {
		t.Fatalf("queued %d times, want once", n)
	}
}

// A window notice goes to everyone on the bus now. An asleep runner session
// is on the bus, but a notice for nobody in particular must not start it.
func TestAWindowNoticeDoesNotWakeAnAsleepRunner(t *testing.T) {
	svc, _, box, set := runnerBus(t)
	svc.EnsureRunnerPeer("run1", "/r/app")
	lead := registerPane(svc, "tmux1", "/r/app")
	id := derivedID("run1")
	set("asleep")
	if n := svc.NotifyGroup("APP", "", "backend joined this window"); n != 1 {
		t.Fatalf("reached %d sessions, want only the tmux session: the runner sleeps", n)
	}
	if evs, _ := svc.Poll(lead.PeerID); len(evs) != 1 {
		t.Fatalf("the session that is up heard it: %+v", evs)
	}
	time.Sleep(50 * time.Millisecond)
	if c, _ := svc.st.PeerCursor(id); c != 0 || len(box.got()) != 0 {
		t.Fatalf("nothing was queued for the sleeping runner: cursor %d, %v", c, box.got())
	}
	// A message to it is another matter: that is what wakes it.
	if resp := svc.Send(SendReq{FromID: lead.PeerID, ToName: "app-s-0a1b2c3d", Text: "wake up"}); !resp.OK {
		t.Fatalf("send to an asleep runner: %+v", resp)
	}
	eventually(t, "the waking prompt", func() bool { return len(box.got()) == 1 })
	// And once it runs, it hears the window's news like anyone.
	set("running")
	if n := svc.NotifyGroup("APP", "", "backend left this window"); n != 2 {
		t.Fatalf("reached %d sessions, want both", n)
	}
	eventually(t, "the notice as a prompt", func() bool { return len(box.got()) == 2 })
}

func TestARunnersSummaryIsHandedOnAndAnotherPeersIsNot(t *testing.T) {
	svc, _, _, _ := runnerBus(t)
	svc.EnsureRunnerPeer("run1", "/r/app")
	var kept []string
	svc.RunnerSummary = func(paneID, summary string) {
		// Called outside the bus lock: asking the bus something here must not hang.
		_ = svc.PeerForPane(paneID)
		kept = append(kept, paneID+": "+summary)
	}
	runner := svc.PeerForPane("run1")
	if !svc.SetSummary(runner, "Export closed trades. Next: the admin table.") {
		t.Fatal("the session's peer takes a summary")
	}
	// A terminal session's peer in the same group keeps its summary on the
	// bus only: it has no session row to keep it on.
	other := svc.RegisterFrom(RegisterReq{PID: 4242, CWD: "/r/app", GitRoot: "/r/app", PaneID: "tmux1"}, "")
	if !svc.SetSummary(other.PeerID, "something else") {
		t.Fatal("a terminal peer takes a summary")
	}
	if svc.SetSummary("no-such-peer", "x") {
		t.Fatal("an unknown peer takes none")
	}
	if len(kept) != 1 || kept[0] != "run1: Export closed trades. Next: the admin table." {
		t.Fatalf("only the runner's summary is handed on: %v", kept)
	}
}
