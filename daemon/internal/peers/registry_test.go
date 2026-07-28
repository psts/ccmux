package peers

import (
	"os"
	"testing"
	"time"
)

// A pane peer whose pane still exists must stay addressable after its Claude
// process exits (an in-pane restart): a message sent while it is away is queued
// and replays when the returning session polls. This is the "restart 10 minutes
// later and still get the message" guarantee.
func TestUnregister_KeepsPanePeerWhilePaneLives(t *testing.T) {
	svc, hook := newTestService(t)
	hook.setGroup("pane-A", "grp")
	hook.setGroup("pane-B", "grp")
	sender := registerPane(svc, "pane-A", "/x/a")
	target := registerPane(svc, "pane-B", "/x/b")

	// target's Claude exits (restart) — its client posts unregister.
	svc.Unregister(target.PeerID)

	// It must still be addressable, and the send must queue.
	resp := svc.Send(SendReq{FromID: sender.PeerID, ToID: target.PeerID, Text: "while you were away"})
	if !resp.OK {
		t.Fatalf("send to a restarting pane peer should queue, got %+v", resp)
	}

	// The returning session (same pane → same id) replays the queued message.
	registerPane(svc, "pane-B", "/x/b")
	evs, err := svc.Poll(target.PeerID)
	if err != nil {
		t.Fatalf("poll after return: %v", err)
	}
	if len(evs) != 1 || evs[0].Text != "while you were away" {
		t.Fatalf("expected the queued message to replay, got %+v", evs)
	}
}

// A pane-less peer (a plain terminal, where the process really is the client)
// is fully removed on unregister — there's no durable pane behind it.
func TestUnregister_DeletesPanelessPeer(t *testing.T) {
	svc, _ := newTestService(t)
	r := svc.Register(RegisterReq{PID: os.Getpid(), CWD: "/x/a", GitRoot: "/x/a"})
	svc.Unregister(r.PeerID)
	if _, err := svc.Poll(r.PeerID); err == nil {
		t.Fatal("pane-less peer should be gone after unregister")
	}
}

// Once a pane peer's pane is actually destroyed there is nothing left to queue
// for, so the peer is removed — but only once the pane's absence has been
// confirmed. A single failed lookup is a cache miss, not a demolition order.
func TestUnregister_DeletesPanePeerWhenPaneConfirmedGone(t *testing.T) {
	svc, hook := newTestService(t)
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	hook.setGroup("pane-C", "grp")
	c := registerPane(svc, "pane-C", "/x/c")

	hook.dropGroup("pane-C") // pane destroyed (or the pane map blinked)
	svc.Unregister(c.PeerID)
	if _, err := svc.Poll(c.PeerID); err != nil {
		t.Fatal("one failed pane lookup must not erase a mailbox")
	}

	now = now.Add(substrateGrace + time.Second)
	svc.ReapOnce()
	if _, err := svc.Poll(c.PeerID); err == nil {
		t.Fatal("pane peer should be removed once its pane is confirmed gone")
	}
}
