package peers

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// quietSetup is a sender and an agent that has run here and is now away.
func quietSetup(t *testing.T) (svc *Service, a, x string, pushes *[]string) {
	t.Helper()
	svc, hook := newTestService(t)
	hook.groups["pane-a"], hook.groups["pane-x"] = "PROJ", "PROJ"
	a = registerPane(svc, "pane-a", "/w/ccmux").PeerID
	x = registerPane(svc, "pane-x", "/repo/.ccmux/agents/x-poster").PeerID
	svc.Unregister(x) // asleep: away, mailbox kept
	var mu sync.Mutex
	pushes = &[]string{}
	svc.PushToPane = func(paneID, text string) error {
		mu.Lock()
		defer mu.Unlock()
		*pushes = append(*pushes, paneID+"|"+text)
		return nil
	}
	svc.WakePane = func(paneID, text string) error { t.Errorf("woke %s for a quiet notice", paneID); return nil }
	return svc, a, x, pushes
}

func TestQuiet_ReachesAnAwayAgentByNameWithoutPushOrWake(t *testing.T) {
	svc, a, x, pushes := quietSetup(t)
	resp := svc.Send(SendReq{FromID: a, ToName: "x-poster", Text: "new finding in shared/radar", Quiet: true, SpawnIfMissing: true})
	if !resp.OK || !resp.Queued || resp.Spawning {
		t.Fatalf("quiet send = %+v, want ok and queued, never a spawn", resp)
	}
	time.Sleep(50 * time.Millisecond) // a push would run on its own goroutine
	if len(*pushes) != 0 {
		t.Fatalf("quiet notice was pushed: %v", *pushes)
	}
	evs, err := svc.st.PeerEventsAfter(x, 0)
	if err != nil || len(evs) != 0 {
		t.Fatalf("quiet notice is in the replayed mailbox: %v %v", evs, err)
	}
}

func TestQuiet_RidesAlongWithTheNextMessageOnce(t *testing.T) {
	svc, a, x, _ := quietSetup(t)
	svc.Send(SendReq{FromID: a, ToID: x, Text: "notice one", Quiet: true})
	svc.Send(SendReq{FromID: a, ToID: x, Text: "notice two", Quiet: true})
	svc.WakePane = nil // the real message may wake; not under test here
	svc.Send(SendReq{FromID: a, ToID: x, Text: "real request"})
	svc.Send(SendReq{FromID: a, ToID: x, Text: "second request"})

	evs, err := svc.st.PeerEventsAfter(x, 0)
	if err != nil || len(evs) != 2 {
		t.Fatalf("mailbox = %v %v, want the two real messages", evs, err)
	}
	first, second := evs[0].Text, evs[1].Text
	if !strings.HasPrefix(first, "real request") || !strings.Contains(first, quietNoticesHeader) ||
		!strings.Contains(first, "notice one") || !strings.Contains(first, "notice two") {
		t.Fatalf("first message did not carry the notices:\n%s", first)
	}
	if strings.Contains(second, quietNoticesHeader) {
		t.Fatalf("notices delivered twice:\n%s", second)
	}
}

func TestQuiet_CheckMessagesHandsThemOverOnce(t *testing.T) {
	svc, a, x, _ := quietSetup(t)
	svc.Send(SendReq{FromID: a, ToID: x, Text: "fyi", Quiet: true})
	registerPane(svc, "pane-x", "/repo/.ccmux/agents/x-poster") // back, polling
	got, err := svc.Poll(x)
	if err != nil || len(got) != 1 || got[0].Text != "fyi" || !got[0].Quiet || got[0].Seq != 0 {
		t.Fatalf("poll = %+v %v, want the notice without its seq (the shim's dedupe would drop it)", got, err)
	}
	if again, _ := svc.Poll(x); len(again) != 0 {
		t.Fatalf("second poll repeated the notice: %+v", again)
	}
}

func TestQuiet_PaneNoticesAreMarkedOnlyWhenTaken(t *testing.T) {
	svc, a, x, _ := quietSetup(t)
	svc.Send(SendReq{FromID: a, ToID: x, Text: "fyi", Quiet: true})
	text, upto := svc.NoticesForPane("pane-x")
	if !strings.Contains(text, "fyi") || upto == 0 {
		t.Fatalf("NoticesForPane = %q %d", text, upto)
	}
	if again, _ := svc.NoticesForPane("pane-x"); again == "" {
		t.Fatal("an unmarked look (a start that failed) must leave the notices waiting")
	}
	svc.MarkNoticesForPane("pane-x", upto)
	if got, _ := svc.NoticesForPane("pane-x"); got != "" {
		t.Fatalf("still waiting after the mark: %q", got)
	}
}

func TestQuiet_UnknownNameIsAnErrorNotASpawn(t *testing.T) {
	svc, a, _, _ := quietSetup(t)
	svc.StartAgent = func(string, string, string) error { t.Fatal("a quiet notice must never start anyone"); return nil }
	resp := svc.Send(SendReq{FromID: a, ToName: "nobody", Text: "x", Quiet: true, SpawnIfMissing: true})
	if resp.OK || !strings.Contains(resp.Error, "quiet notice") {
		t.Fatalf("resp = %+v", resp)
	}
}
