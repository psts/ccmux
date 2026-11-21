package peers

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

// threadBus is a window SHOP with a thread in it: its lead on runner pane
// "lead" (app-thread-1), the repo backend's own live session on tmux pane
// "tmuxb", and a session in no thread on tmux pane "tmuxa" (app). The
// thread's backend helper is runner pane "help" (backend-thread-1), which
// comes to be when StartHelper is called.
type threadBus struct {
	svc     *Service
	box     *inbox
	lead    string // the lead's peer id
	backend string // the repo's own session's peer id
	plain   string // the app session's peer id

	mu       sync.Mutex
	runners  map[string]string // runner pane → bus name
	started  []string          // "thread repo" per StartHelper call
	startErr error
}

func newThreadBus(t *testing.T) *threadBus {
	t.Helper()
	svc, hook, _ := newTestServiceWithStore(t)
	b := &threadBus{svc: svc, box: &inbox{}, runners: map[string]string{"lead": "app-thread-1"}}
	for _, pane := range []string{"lead", "help", "tmuxa", "tmuxb"} {
		hook.groups[pane] = "SHOP"
	}
	svc.RunnerPane = func(paneID string) (RunnerInfo, bool) {
		b.mu.Lock()
		defer b.mu.Unlock()
		name, ok := b.runners[paneID]
		return RunnerInfo{Name: name, Repo: "/r/" + strings.TrimSuffix(name, "-thread-1")}, ok
	}
	svc.RunnerInbox = b.box.take
	svc.ThreadRoute = func(senderPane, toName string) (HelperRoute, bool) {
		if (senderPane != "lead" && senderPane != "help") || toName != "backend" {
			return HelperRoute{}, false
		}
		return HelperRoute{ThreadID: 1, Repo: "/r/backend", Name: "backend-thread-1"}, true
	}
	svc.StartHelper = func(r HelperRoute) error {
		repo := r.Repo
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.startErr != nil {
			return b.startErr
		}
		b.started = append(b.started, repo)
		b.runners["help"] = "backend-thread-1"
		go svc.EnsureRunnerPeer("help", repo) // its session was made: the daemon puts its peer on the bus
		return nil
	}
	svc.EnsureRunnerPeer("lead", "/r/app")
	b.lead = derivedID("lead")
	b.plain = registerPane(svc, "tmuxa", "/r/app").PeerID
	b.backend = registerPane(svc, "tmuxb", "/r/backend").PeerID
	return b
}

func (b *threadBus) starts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.started)
}

// toHelper is the messages that reached the helper's session, as "sender|text".
func (b *threadBus) toHelper() []string {
	var out []string
	for _, q := range b.box.got() {
		if f := strings.SplitN(q, "|", 4); f[0] == "help" {
			out = append(out, f[2]+"|"+f[3])
		}
	}
	return out
}

// mail is what waits in a tmux pane's mailbox.
func (b *threadBus) mail(t *testing.T, peerID string) []string {
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

func TestAThreadMembersMessageToARepoStartsAndReachesItsOwnHelper(t *testing.T) {
	b := newThreadBus(t)
	resp := b.svc.Send(SendReq{FromID: b.lead, ToName: "backend", Text: "what does GET /trades return?"})
	if !resp.OK || !resp.Spawning {
		t.Fatalf("the helper is not there yet, so it is started and the message waits: %+v", resp)
	}
	eventually(t, "the helper to get the message that started it", func() bool { return len(b.toHelper()) == 1 })
	if got := b.toHelper()[0]; !strings.HasPrefix(got, "app-thread-1|") || !strings.Contains(got, "GET /trades") {
		t.Fatalf("it arrives as the lead's message: %q", got)
	}
	if got := b.mail(t, b.backend); len(got) != 0 {
		t.Fatalf("the repo's own session gets none of the thread's mail: %v", got)
	}
	// The helper is on the bus now: the next message goes straight to it.
	if resp := b.svc.Send(SendReq{FromID: b.lead, ToName: "backend", Text: "and POST?"}); !resp.OK || resp.Spawning {
		t.Fatalf("a helper that is there is just sent to: %+v", resp)
	}
	eventually(t, "the second message", func() bool { return len(b.toHelper()) == 2 })
	if b.starts() != 1 {
		t.Fatalf("one helper was started, once: %d", b.starts())
	}
	// The helper can name its own repo's session only by id; by name it would be itself.
	helper := derivedID("help")
	if resp := b.svc.Send(SendReq{FromID: helper, ToID: b.backend, Text: "are you mid-change in routes/?"}); !resp.OK {
		t.Fatalf("a helper still reaches the repo's own session by id: %+v", resp)
	}
	if got := b.mail(t, b.backend); len(got) != 1 || !strings.HasPrefix(got[0], "backend-thread-1|") {
		t.Fatalf("and that message is from the helper, under its own name: %v", got)
	}
}

func TestTwoMessagesToAHelperThatIsStartingWaitForTheOneStart(t *testing.T) {
	b := newThreadBus(t)
	hold := make(chan struct{})
	start := b.svc.StartHelper
	b.svc.StartHelper = func(r HelperRoute) error { <-hold; return start(r) }
	first := b.svc.Send(SendReq{FromID: b.lead, ToName: "backend", Text: "one"})
	second := b.svc.Delegate(DelegateReq{FromID: b.lead, ToName: "backend", Text: "two"})
	if !first.Spawning || !second.Spawning || second.TaskID == "" {
		t.Fatalf("both wait for the helper, the delegation with its task: %+v %+v", first, second)
	}
	close(hold)
	eventually(t, "both messages", func() bool { return len(b.toHelper()) == 2 })
	if got := b.toHelper(); !strings.Contains(got[0], "\none\n") || !strings.Contains(got[1], "[claude-peers delegation task "+second.TaskID+"]") {
		t.Fatalf("in the order they were sent, the delegation with its header: %v", got)
	}
	if b.starts() != 1 {
		t.Fatalf("one start for both: %d", b.starts())
	}
}

func TestSendersOutsideAThreadAndOtherNamesKeepTheirRouting(t *testing.T) {
	b := newThreadBus(t)
	// A session in no thread naming the repo reaches the repo's own session.
	if resp := b.svc.Send(SendReq{FromID: b.plain, ToName: "backend", Text: "hi"}); !resp.OK || resp.Spawning {
		t.Fatalf("today's routing: %+v", resp)
	}
	if got := b.mail(t, b.backend); len(got) != 1 || got[0] != "app|hi" {
		t.Fatalf("the repo's own session got it: %v", got)
	}
	// A thread member naming something that is no repo of the window: the usual lookup, and its usual miss.
	if resp := b.svc.Send(SendReq{FromID: b.lead, ToName: "payments", Text: "hi"}); resp.OK || !strings.Contains(resp.Error, `Peer "payments" not found in project`) {
		t.Fatalf("an unknown name misses as it always did: %+v", resp)
	}
	// A thread member naming the repo in another project: the thread has no helpers there.
	if resp := b.svc.Send(SendReq{FromID: b.lead, ToName: "backend", ToGroup: "OTHER", Text: "hi"}); resp.OK || !strings.Contains(resp.Error, `Peer "backend" not found in project`) {
		t.Fatalf("another group is looked up as always: %+v", resp)
	}
	// Naming its own group changes nothing.
	if resp := b.svc.Send(SendReq{FromID: b.lead, ToName: "backend", ToGroup: "shop", Text: "hi"}); !resp.Spawning {
		t.Fatalf("its own group named is still the thread's helper: %+v", resp)
	}
	eventually(t, "the helper's start", func() bool { return b.starts() == 1 })
	eventually(t, "the message that started it", func() bool { return len(b.toHelper()) == 1 })
	if b.starts() != 1 {
		t.Fatalf("only the thread member's message started a helper: %d", b.starts())
	}
}

func TestAHelperThatCannotStartIsSaidToWhoeverAskedForIt(t *testing.T) {
	b := newThreadBus(t)
	b.startErr = errors.New("backend is not a repo session in thread #1's window")
	tracked := b.svc.Delegate(DelegateReq{FromID: b.lead, ToName: "backend", Text: "add the endpoint"})
	if !tracked.Spawning {
		t.Fatalf("the start is tried: %+v", tracked)
	}
	lead := func() []string {
		var out []string
		for _, q := range b.box.got() {
			if strings.HasPrefix(q, "lead|") {
				out = append(out, q)
			}
		}
		return out
	}
	eventually(t, "the lead to hear", func() bool { return len(lead()) == 1 })
	if got := lead()[0]; !strings.Contains(got, `Helper "backend-thread-1" could not be started`) || !strings.Contains(got, "not a repo session") {
		t.Fatalf("the lead is told why: %q", got)
	}
	if task, _ := b.svc.st.PeerTask(tracked.TaskID); task == nil || task.Status != "failed" {
		t.Fatalf("the delegation is closed, not left open for a helper that will never be: %+v", task)
	}
	// With no way to start one at all, the send is refused outright.
	b.svc.StartHelper = nil
	if resp := b.svc.Send(SendReq{FromID: b.lead, ToName: "backend", Text: "hi"}); resp.OK || !strings.Contains(resp.Error, "cannot start one") {
		t.Fatalf("refused with the reason: %+v", resp)
	}
}

func TestASideHelperNamingItsMainSessionsRepoReachesThatSession(t *testing.T) {
	b := newThreadBus(t)
	b.mu.Lock()
	b.runners["help"] = "backend-for-app"
	b.mu.Unlock()
	b.svc.EnsureRunnerPeer("help", "/r/backend")
	b.svc.ThreadRoute = func(senderPane, toName string) (HelperRoute, bool) {
		if senderPane == "help" && toName == "app" {
			return HelperRoute{Repo: "/r/app", Pane: "lead"}, true
		}
		return HelperRoute{}, false
	}
	if resp := b.svc.Send(SendReq{FromID: derivedID("help"), ToName: "app", Text: "the endpoint is live"}); !resp.OK || resp.Spawning {
		t.Fatalf("the main session is there, so the message goes straight to it: %+v", resp)
	}
	eventually(t, "the main session to get it", func() bool {
		for _, q := range b.box.got() {
			if strings.HasPrefix(q, "lead|") && strings.Contains(q, "the endpoint is live") {
				return true
			}
		}
		return false
	})
	if got := b.mail(t, b.plain); len(got) != 0 {
		t.Fatalf("app's old terminal session gets none of it: %v", got)
	}
	if b.starts() != 0 {
		t.Fatalf("no helper is started for the main session's own repo: %d", b.starts())
	}
}

func TestAMessageToASessionThatEndedOnItsWayIsHandedBackToItsSender(t *testing.T) {
	b := newThreadBus(t)
	b.mu.Lock()
	b.runners["help"] = "backend-for-app"
	b.mu.Unlock()
	b.svc.EnsureRunnerPeer("help", "/r/backend")
	b.box.setRefuse(ErrInboxClosed)
	if resp := b.svc.Send(SendReq{FromID: b.plain, ToID: derivedID("help"), Text: "is the migration live?"}); !resp.OK {
		t.Fatalf("the send itself is taken: %+v", resp)
	}
	eventually(t, "the sender to hear", func() bool {
		for _, m := range b.mail(t, b.plain) {
			if strings.Contains(m, "backend-for-app was not delivered") {
				return true
			}
		}
		return false
	})
}

func TestASideHelperWhoseMainSessionIsGoneIsToldSo(t *testing.T) {
	b := newThreadBus(t)
	b.mu.Lock()
	b.runners["help"] = "backend-for-app"
	b.mu.Unlock()
	b.svc.EnsureRunnerPeer("help", "/r/backend")
	b.svc.ThreadRoute = func(senderPane, toName string) (HelperRoute, bool) {
		return HelperRoute{Repo: "/r/app", Pane: "ended-main"}, true
	}
	resp := b.svc.Send(SendReq{FromID: derivedID("help"), ToName: "app", Text: "done"})
	if resp.OK || !strings.Contains(resp.Error, "has ended") {
		t.Fatalf("refused with the reason: %+v", resp)
	}
	if b.starts() != 0 {
		t.Fatalf("no helper is started in its place: %d", b.starts())
	}
	if got := b.mail(t, b.plain); len(got) != 0 {
		t.Fatalf("app's old terminal session gets nothing: %v", got)
	}
}
