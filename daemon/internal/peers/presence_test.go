package peers

import (
	"os"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

// listNames returns the names the caller sees at "all" scope.
func listIDs(svc *Service, callerID string) []string {
	var out []string
	for _, e := range svc.List(callerID, "all", "") {
		out = append(out, e.ID)
	}
	return out
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// The bug this whole change exists for: a Claude session exits, its pane falls
// back to a shell prompt, and the peer went on being listed as online forever
// because the PANE was alive. Presence is the session, not the pane.
func TestList_ExcludesSessionThatLeftEvenThoughPaneLives(t *testing.T) {
	svc, hook := newTestService(t)
	hook.setGroup("pane-A", "grp")
	hook.setGroup("pane-B", "grp")
	me := registerPane(svc, "pane-A", "/x/a")
	gone := registerPane(svc, "pane-B", "/x/b")

	if !contains(listIDs(svc, me.PeerID), gone.PeerID) {
		t.Fatal("a registered peer should be listed")
	}

	svc.Unregister(gone.PeerID) // Claude exits; pane-B survives as a shell

	if contains(listIDs(svc, me.PeerID), gone.PeerID) {
		t.Fatal("a peer whose session left must not be listed as online")
	}
}

// An unclean drop (SIGKILL, daemon restart, network blip) has no goodbye, so it
// is covered by the grace window — and only by the grace window.
func TestList_SurvivesSocketFlapThenExpires(t *testing.T) {
	svc, hook := newTestService(t)
	hook.setGroup("pane-A", "grp")
	hook.setGroup("pane-B", "grp")
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }

	me := registerPane(svc, "pane-A", "/x/a")
	flapping := registerPane(svc, "pane-B", "/x/b")

	now = now.Add(presenceGrace - time.Second)
	if !contains(listIDs(svc, me.PeerID), flapping.PeerID) {
		t.Fatal("a peer inside the grace window is reconnecting, not gone")
	}

	now = now.Add(2 * time.Second) // past the grace window
	if contains(listIDs(svc, me.PeerID), flapping.PeerID) {
		t.Fatal("a peer that stopped proving it is there must drop out of listings")
	}
}

// Presence is not eviction: the away peer keeps its mailbox, and a reply
// addressed to it by id is accepted — but the sender is TOLD it only queued.
func TestSend_ToAwayPeerQueuesAndSaysSo(t *testing.T) {
	svc, hook := newTestService(t)
	hook.setGroup("pane-A", "grp")
	hook.setGroup("pane-B", "grp")
	sender := registerPane(svc, "pane-A", "/x/a")
	target := registerPane(svc, "pane-B", "/x/b")

	if resp := svc.Send(SendReq{FromID: sender.PeerID, ToID: target.PeerID, Text: "hi"}); resp.Queued {
		t.Fatalf("send to a present peer must not report queued: %+v", resp)
	}

	svc.Unregister(target.PeerID)

	resp := svc.Send(SendReq{FromID: sender.PeerID, ToID: target.PeerID, Text: "while away"})
	if !resp.OK || !resp.Queued {
		t.Fatalf("send to an away peer should succeed AND report queued, got %+v", resp)
	}
}

// Addressing a teammate BY NAME means "the one that's running". An away peer
// must not absorb it — the request has to fall through to the spawn path.
func TestSend_ByNameIgnoresAwayPeer(t *testing.T) {
	svc, hook := newTestService(t)
	hook.setGroup("pane-A", "grp")
	hook.setGroup("pane-B", "grp")
	sender := registerPane(svc, "pane-A", "/x/a")
	target := registerPane(svc, "pane-B", "/x/backend")
	svc.Unregister(target.PeerID)

	resp := svc.Send(SendReq{FromID: sender.PeerID, ToName: "backend", Text: "you there?"})
	if resp.OK {
		t.Fatalf("by-name send should not resolve to a departed session, got %+v", resp)
	}
	if resp.Error == "" {
		t.Fatal("expected a not-found error the caller can act on")
	}
}

// Reaping is the registry's own timer, not a side effect of somebody calling
// List — and it takes the database with it.
func TestReapOnce_DropsDeadPaneAndErasesMailbox(t *testing.T) {
	svc, hook, st := newTestServiceWithStore(t)
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	hook.setGroup("pane-A", "grp")
	hook.setGroup("pane-B", "grp")
	sender := registerPane(svc, "pane-A", "/x/a")
	doomed := registerPane(svc, "pane-B", "/x/b")
	svc.Send(SendReq{FromID: sender.PeerID, ToID: doomed.PeerID, Text: "unread"})

	hook.dropGroup("pane-B") // the pane is deleted
	svc.ReapOnce()           // first sweep only records the absence
	if _, err := svc.Poll(doomed.PeerID); err != nil {
		t.Fatal("a single sweep must not be enough to erase a mailbox")
	}
	now = now.Add(substrateGrace + time.Second)
	svc.ReapOnce()

	if _, err := svc.Poll(doomed.PeerID); err == nil {
		t.Fatal("a peer whose pane is gone must be reaped from the registry")
	}
	boxes, err := st.PeerMailboxes()
	if err != nil {
		t.Fatalf("mailboxes: %v", err)
	}
	for _, b := range boxes {
		if b.PeerID == doomed.PeerID {
			t.Fatal("reaping must erase the mailbox, not leave a cursor behind")
		}
	}
	evs, err := st.PeerEventsAfter(doomed.PeerID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("undeliverable events should be erased with the mailbox, got %d", len(evs))
	}
}

// The collector runs against a registry that has forgotten everything (a daemon
// restart). It must keep the mailbox of a pane that still exists — that pane can
// still host a returning session — and erase one whose pane is gone.
func TestCollectMailboxes_KeepsLivePaneAcrossDaemonRestart(t *testing.T) {
	svc, hook, st := newTestServiceWithStore(t)
	hook.setGroup("pane-A", "grp")
	kept := registerPane(svc, "pane-A", "/x/a")

	// A fresh Service over the same store models ccmuxd restarting: no peers in
	// memory, every mailbox unclaimed.
	restarted := newBareService(st)
	restarted.mgr = hook
	now := time.Unix(1_700_000_000, 0)
	restarted.Now = func() time.Time { return now }
	restarted.CollectMailboxes()
	if !hasMailbox(t, st, kept.PeerID) {
		t.Fatal("a mailbox whose pane still exists must survive a daemon restart")
	}

	hook.dropGroup("pane-A")
	restarted.CollectMailboxes()
	if !hasMailbox(t, st, kept.PeerID) {
		t.Fatal("one failed pane lookup must not collect a mailbox")
	}

	now = now.Add(substrateGrace + time.Second)
	restarted.CollectMailboxes()
	if hasMailbox(t, st, kept.PeerID) {
		t.Fatal("a mailbox whose pane is confirmed gone must be collected")
	}
}

// A pane-less peer's mailbox dies with its process: nothing durable stands
// behind it, so an unclaimed one is garbage.
func TestCollectMailboxes_ErasesUnclaimedPanelessMailbox(t *testing.T) {
	svc, _, st := newTestServiceWithStore(t)
	r := svc.Register(RegisterReq{PID: os.Getpid(), CWD: "/x/a", GitRoot: "/x/a"})
	if !hasMailbox(t, st, r.PeerID) {
		t.Fatal("registering should record a mailbox")
	}

	newBareService(st).CollectMailboxes() // restarted daemon, nobody claims it
	if hasMailbox(t, st, r.PeerID) {
		t.Fatal("an unclaimed pane-less mailbox must be collected")
	}
}

// Cursors written before mailboxes recorded a substrate carry no pane id — but
// that means "never written down", NOT "pane-less". Reading it as pane-less
// erased three live panes' queues once already, so ambiguity must never be
// grounds for deletion. The set is fixed and shrinks as peers re-register.
func TestCollectMailboxes_KeepsCursorOfUnknownProvenance(t *testing.T) {
	_, _, st := newTestServiceWithStore(t)
	if err := st.AdvancePeerCursor("legacy01", 42); err != nil {
		t.Fatalf("seed legacy cursor: %v", err)
	}

	newBareService(st).CollectMailboxes()
	if !hasMailbox(t, st, "legacy01") {
		t.Fatal("a cursor of unknown provenance must never be collected")
	}
}

func hasMailbox(t *testing.T, st *store.SQLite, peerID string) bool {
	t.Helper()
	boxes, err := st.PeerMailboxes()
	if err != nil {
		t.Fatalf("mailboxes: %v", err)
	}
	for _, b := range boxes {
		if b.PeerID == peerID {
			return true
		}
	}
	return false
}

// newBareService models a restarted daemon: same store, empty registry.
func newBareService(st *store.SQLite) *Service {
	return NewService(st, &fakeHook{groups: map[string]string{}, repos: map[string]string{}, shells: map[string]bool{}},
		[]byte("test-secret-test-secret-test-sec"))
}

// A pane-less client that re-registers under a new id (its old one forgotten
// after a daemon bounce) leaves a record nothing can address: the id was random,
// so no session can ever re-derive it. Evicting it must take the mailbox too —
// this is how orphan cursors accumulated.
func TestRegister_EvictingStalePanelessRecordErasesItsMailbox(t *testing.T) {
	svc, _, st := newTestServiceWithStore(t)
	first := svc.Register(RegisterReq{PID: os.Getpid(), CWD: "/x/a", GitRoot: "/x/a"})
	if !hasMailbox(t, st, first.PeerID) {
		t.Fatal("first registration should record a mailbox")
	}

	// Same process, no requested_id (its id was lost) → a fresh random identity.
	second := svc.Register(RegisterReq{PID: os.Getpid(), CWD: "/x/a", GitRoot: "/x/a"})
	if second.PeerID == first.PeerID {
		t.Skip("ids collided; nothing was evicted")
	}
	if hasMailbox(t, st, first.PeerID) {
		t.Fatal("the superseded record's mailbox must be erased, not orphaned")
	}
	if !hasMailbox(t, st, second.PeerID) {
		t.Fatal("the live registration must keep its mailbox")
	}
}

// A client whose goodbye never lands (the POST is fire-and-forget, and it
// forgets its id either way) used to stay registered forever: its process is
// still alive, so no substrate check ever fired. With no pane behind it its id
// is unaddressable, so going absent is the end of it.
func TestReapOnce_DropsSilentPanelessPeer(t *testing.T) {
	svc, _, st := newTestServiceWithStore(t)
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	r := svc.Register(RegisterReq{PID: os.Getpid(), CWD: "/x/a", GitRoot: "/x/a"})

	svc.ReapOnce() // still present — its process AND its registration are fresh
	if _, err := svc.Poll(r.PeerID); err != nil {
		t.Fatal("a present pane-less peer must not be reaped")
	}

	now = now.Add(presenceGrace + time.Second) // stopped proving it is there
	svc.ReapOnce()
	if _, err := svc.Poll(r.PeerID); err == nil {
		t.Fatal("a pane-less peer that went silent must be reaped")
	}
	if hasMailbox(t, st, r.PeerID) {
		t.Fatal("its mailbox is unaddressable and must go with it")
	}
}

// A Mac driver-mode pane's peer id IS derived (from the local pane uuid), so it
// earns the same standing mailbox as a hosted pane: going silent must not erase
// the queue a returning session in that pane will collect.
func TestReapOnce_KeepsSilentLocalPanePeer(t *testing.T) {
	svc, _, st := newTestServiceWithStore(t)
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	const uuid = "AAAA1111-0000-0000-0000-000000000000"
	svc.SetLocalPaneGroups(map[string]string{uuid: "CHARTLABS"})
	r := svc.Register(RegisterReq{LocalPaneID: uuid, PID: os.Getpid(),
		CWD: "/x/admin", GitRoot: "/x/admin"})

	boxes, err := st.PeerMailboxes()
	if err != nil || len(boxes) != 1 || boxes[0].PaneID != "local:"+strings.ToLower(uuid) {
		t.Fatalf("mailbox = %+v (err %v), want the local pane recorded as substrate", boxes, err)
	}

	now = now.Add(presenceGrace + time.Second)
	svc.ReapOnce()
	if _, err := svc.Poll(r.PeerID); err != nil {
		t.Fatal("a local-pane peer keeps its mailbox while the pane exists")
	}

	// The pane itself goes away (the app pushes a map without it).
	svc.SetLocalPaneGroups(map[string]string{})
	svc.ReapOnce() // first miss only records the absence
	if _, err := svc.Poll(r.PeerID); err != nil {
		t.Fatal("one empty local-pane map must not erase a mailbox")
	}
	now = now.Add(substrateGrace + time.Second)
	svc.ReapOnce()
	if _, err := svc.Poll(r.PeerID); err == nil {
		t.Fatal("a local-pane peer must be reaped once its pane is confirmed gone")
	}
}

// The case Patric caught: Claude Code sitting on the session picker. The
// process is up and its MCP socket is CONNECTED, but no session will ever read
// a message. A live socket must not be able to override that.
func TestPresence_SessionEndHidesPeerWithLiveSocket(t *testing.T) {
	svc, hook := newTestService(t)
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	hook.setGroup("pane-A", "grp")
	hook.setGroup("pane-B", "grp")
	me := registerPane(svc, "pane-A", "/x/a")
	picker := registerPane(svc, "pane-B", "/x/b")

	// Give it a live push socket — the strongest "online" evidence the bus has.
	svc.mu.Lock()
	svc.conns[picker.PeerID] = &peerConn{peerID: picker.PeerID}
	svc.mu.Unlock()

	svc.NoteSession("pane-B", "s1", model.SessionStarted)
	if !contains(listIDs(svc, me.PeerID), picker.PeerID) {
		t.Fatal("a running session must be listed")
	}

	svc.NoteSession("pane-B", "s1", model.SessionEnded)
	now = now.Add(sessionIdleGrace + time.Second)
	if contains(listIDs(svc, me.PeerID), picker.PeerID) {
		t.Fatal("a connected socket with no session behind it must not be listed")
	}
}

// Hooks may only ever REMOVE presence. A pane the bus has never heard a session
// hook for behaves exactly as before, so a broken or missing hook install can
// never hide a session that is genuinely there.
func TestPresence_UnknownSessionsLeavePeerVisible(t *testing.T) {
	svc, hook := newTestService(t)
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	hook.setGroup("pane-A", "grp")
	hook.setGroup("pane-B", "grp")
	me := registerPane(svc, "pane-A", "/x/a")
	quiet := registerPane(svc, "pane-B", "/x/b")

	now = now.Add(10 * sessionIdleGrace) // no hook has ever mentioned pane-B
	svc.mu.Lock()
	svc.conns[quiet.PeerID] = &peerConn{peerID: quiet.PeerID}
	svc.mu.Unlock()
	if !contains(listIDs(svc, me.PeerID), quiet.PeerID) {
		t.Fatal("never having seen a session hook must not hide a connected peer")
	}
}

// A session whose start was missed — hooks installed mid-session, or the daemon
// restarted under it — proves itself with its next prompt or stop.
func TestPresence_ActivityRevivesAfterMissedStart(t *testing.T) {
	svc, hook := newTestService(t)
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	hook.setGroup("pane-A", "grp")
	hook.setGroup("pane-B", "grp")
	me := registerPane(svc, "pane-A", "/x/a")
	revived := registerPane(svc, "pane-B", "/x/b")
	svc.mu.Lock()
	svc.conns[revived.PeerID] = &peerConn{peerID: revived.PeerID}
	svc.mu.Unlock()

	svc.NoteSession("pane-B", "s1", model.SessionEnded) // only the end was seen
	now = now.Add(sessionIdleGrace + time.Second)
	if contains(listIDs(svc, me.PeerID), revived.PeerID) {
		t.Fatal("an ended session should be hidden")
	}

	// A new session starts; its start is missed, but it submits a prompt.
	svc.NoteSession("pane-B", "s2", model.SessionActive)
	if !contains(listIDs(svc, me.PeerID), revived.PeerID) {
		t.Fatal("activity must revive a pane whose session start was never seen")
	}
}

// Session truth is per pane and must not leak: ending a session in one pane
// cannot silence another pane's peer.
func TestPresence_SessionEndIsScopedToItsPane(t *testing.T) {
	svc, hook := newTestService(t)
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	hook.setGroup("pane-A", "grp")
	hook.setGroup("pane-B", "grp")
	hook.setGroup("pane-C", "grp")
	me := registerPane(svc, "pane-A", "/x/a")
	other := registerPane(svc, "pane-C", "/x/c")

	svc.NoteSession("pane-B", "s1", model.SessionStarted)
	svc.NoteSession("pane-B", "s1", model.SessionEnded)
	now = now.Add(sessionIdleGrace + time.Second)

	if !contains(listIDs(svc, me.PeerID), other.PeerID) {
		t.Fatal("pane-C's peer must be unaffected by pane-B's session ending")
	}
}

// Session truth has to outlive a daemon restart. A pane whose session ended will
// never fire another hook, so if the knowledge is lost on restart it is lost
// forever — the pane silently reappears as a peer that cannot answer.
func TestPresence_SessionTruthSurvivesDaemonRestart(t *testing.T) {
	svc, hook, st := newTestServiceWithStore(t)
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	hook.setGroup("pane-A", "grp")
	hook.setGroup("pane-B", "grp")
	registerPane(svc, "pane-A", "/x/a")
	registerPane(svc, "pane-B", "/x/b")
	svc.NoteSession("pane-B", "s1", model.SessionStarted)
	svc.NoteSession("pane-B", "s1", model.SessionEnded)

	// Restart: fresh service, same store, peers re-register as they reconnect.
	restarted := newBareService(st)
	restarted.mgr = hook
	restarted.Now = func() time.Time { return now }
	restarted.Start(t.Context())
	me := registerPane(restarted, "pane-A", "/x/a")
	back := registerPane(restarted, "pane-B", "/x/b")

	now = now.Add(sessionIdleGrace + time.Second)
	if contains(listIDs(restarted, me.PeerID), back.PeerID) {
		t.Fatal("a pane whose session ended must stay hidden across a restart")
	}

	// And a genuinely new session there is visible again.
	restarted.NoteSession("pane-B", "s2", model.SessionStarted)
	if !contains(listIDs(restarted, me.PeerID), back.PeerID) {
		t.Fatal("a new session must bring the peer back")
	}
}

// The delivery mode a session registers with has to reach the listing, or a
// poll-only peer is indistinguishable from one with a broken socket.
func TestList_CarriesPollOnlyMode(t *testing.T) {
	svc, hook := newTestService(t)
	hook.setGroup("pane-A", "grp")
	hook.setGroup("pane-B", "grp")
	hook.setGroup("pane-C", "grp")
	me := registerPane(svc, "pane-A", "/x/a")
	svc.Register(RegisterReq{PaneID: "pane-B", PID: os.Getpid(), CWD: "/x/b", GitRoot: "/x/b", PollOnly: true})
	svc.Register(RegisterReq{PaneID: "pane-C", PID: os.Getpid(), CWD: "/x/c", GitRoot: "/x/c"})

	byName := map[string]ListEntry{}
	for _, e := range svc.List(me.PeerID, "all", "") {
		byName[e.CWD] = e
	}
	if !byName["/x/b"].PollOnly {
		t.Error("a poll-only session must be reported as such")
	}
	// An older client sends nothing, and push is what almost every session runs.
	if byName["/x/c"].PollOnly {
		t.Error("the default must be push, so old clients are not mislabelled")
	}
}

// The backstop. Hooks get lost — the daemon was restarting, the session predates
// the hook install, a SIGKILL fired no SessionEnd — so a pane can be left
// believing in a session that ended. tmux streaming a bare shell settles it, and
// settles it AT ONCE: the idle grace exists to cover a start we may have missed,
// and there is nothing to miss when we can see the shell.
func TestPresence_BareShellRetiresSessionsImmediately(t *testing.T) {
	svc, hook := newTestService(t)
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	hook.setGroup("pane-A", "grp")
	hook.setGroup("pane-B", "grp")
	me := registerPane(svc, "pane-A", "/x/a")
	gone := registerPane(svc, "pane-B", "/x/b")
	svc.mu.Lock()
	svc.conns[gone.PeerID] = &peerConn{peerID: gone.PeerID}
	svc.mu.Unlock()

	// A session is running and its SessionEnd will never arrive.
	svc.NoteSession("pane-B", "s1", model.SessionStarted)
	if !contains(listIDs(svc, me.PeerID), gone.PeerID) {
		t.Fatal("a running session must be listed")
	}

	hook.setShell("pane-B", true) // tmux: the pane is at a shell
	svc.NoteSession("pane-B", "", model.SessionNone)
	if contains(listIDs(svc, me.PeerID), gone.PeerID) {
		t.Fatal("a pane at a bare shell has no session, no grace period needed")
	}

	// And a real session starting there brings it straight back.
	hook.setShell("pane-B", false)
	svc.NoteSession("pane-B", "s2", model.SessionStarted)
	if !contains(listIDs(svc, me.PeerID), gone.PeerID) {
		t.Fatal("a new session must clear the shell observation")
	}
}

// SessionNone is an observation of the PANE, so it retires every session id at
// once — unlike SessionEnded, which retires the one that reported itself.
func TestNoteSession_NoneClearsEveryLiveID(t *testing.T) {
	svc, hook := newTestService(t)
	hook.setShell("pane-X", true)
	svc.NoteSession("pane-X", "s1", model.SessionStarted)
	svc.NoteSession("pane-X", "s2", model.SessionStarted)

	svc.NoteSession("pane-X", "", model.SessionNone)

	svc.mu.Lock()
	defer svc.mu.Unlock()
	if n := len(svc.sessions["pane-X"].live); n != 0 {
		t.Fatalf("SessionNone left %d live ids, want 0", n)
	}
	if !svc.paneSessionDeadLocked("pane-X") {
		t.Fatal("the pane must read as dead straight away")
	}
}
