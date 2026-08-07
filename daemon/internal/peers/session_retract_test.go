package peers

import (
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

// The shell backstop is asserted on every command signal, restarts included, so
// a pane caught at its shell for one moment is recorded as holding no session.
// Only a hook could ever say otherwise — and on a host with no hooks installed
// there are none, so a healthy Claude with a connected bus socket stayed hidden
// for the life of the pane. Seeing Claude in the foreground has to withdraw it.
func TestNoteSession_UnknownRetractsAStaleNone(t *testing.T) {
	svc, hook := newTestService(t)
	hook.setGroup("pane-A", "grp")
	hook.setGroup("pane-B", "grp")
	me := registerPane(svc, "pane-A", "/x/a")
	hidden := registerPane(svc, "pane-B", "/x/b")
	svc.mu.Lock()
	svc.conns[hidden.PeerID] = &peerConn{peerID: hidden.PeerID}
	svc.mu.Unlock()

	hook.setShell("pane-B", true)
	svc.NoteSession("pane-B", "", model.SessionNone)
	if contains(listIDs(svc, me.PeerID), hidden.PeerID) {
		t.Fatal("a pane at a bare shell must not be listed")
	}

	// Claude is in the foreground again. No hook has named a session id — that
	// is exactly the hookless host — so the honest state is not-knowing.
	hook.setShell("pane-B", false)
	svc.NoteSession("pane-B", "", model.SessionUnknown)

	if !contains(listIDs(svc, me.PeerID), hidden.PeerID) {
		t.Fatal("a pane running Claude is still hidden by the retired observation")
	}
}

// Not-knowing means no record at all: an empty record is precisely the state
// that reads as known-dead, so writing one would retract nothing.
func TestNoteSession_UnknownForgetsThePane(t *testing.T) {
	svc, _ := newTestService(t)
	svc.NoteSession("pane-X", "s1", model.SessionStarted)
	svc.NoteSession("pane-X", "", model.SessionUnknown)

	svc.mu.Lock()
	defer svc.mu.Unlock()
	if ps, ok := svc.sessions["pane-X"]; ok {
		t.Fatalf("pane still has a record (%d live ids), want none", len(ps.live))
	}
	if svc.paneSessionDeadLocked("pane-X") {
		t.Fatal("a forgotten pane must read as not-known-dead")
	}
}

// The retraction has to survive a daemon restart, or the first restart after it
// resurrects the stale verdict from disk and the pane vanishes again.
func TestNoteSession_UnknownIsPersisted(t *testing.T) {
	svc, _, st := newTestServiceWithStore(t)
	svc.NoteSession("pane-X", "", model.SessionNone)
	if rows, err := st.LoadPaneSessions(); err != nil || len(rows) != 1 {
		t.Fatalf("expected the none verdict on disk, got %v (%v)", rows, err)
	}

	svc.NoteSession("pane-X", "", model.SessionUnknown)

	rows, err := st.LoadPaneSessions()
	if err != nil {
		t.Fatal(err)
	}
	if _, still := rows["pane-X"]; still {
		t.Error("the retracted verdict is still on disk and will come back on restart")
	}
}

// A shell observation must still work. The retraction is a counterpart to the
// backstop, not a replacement for it.
func TestNoteSession_NoneStillHidesAPaneAtAShell(t *testing.T) {
	svc, hook := newTestService(t)
	hook.setShell("pane-X", true)
	svc.NoteSession("pane-X", "", model.SessionUnknown)
	svc.NoteSession("pane-X", "", model.SessionNone)

	svc.mu.Lock()
	defer svc.mu.Unlock()
	if !svc.paneSessionDeadLocked("pane-X") {
		t.Fatal("a pane at a bare shell must still read as dead")
	}
}
