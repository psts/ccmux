package peers

import "testing"

// A window notice reaches every session in that window from the bus
// itself, never the session it is about, and nobody in another window.
func TestNotifyGroup_ReachesTheWindowButNotItsSubject(t *testing.T) {
	svc, hook := newTestService(t)
	for pane, group := range map[string]string{"p-fe": "CL", "p-be": "CL", "p-admin": "CL", "p-x": "OTHER"} {
		hook.setGroup(pane, group)
	}
	fe := registerPane(svc, "p-fe", "/r/frontend")
	be := registerPane(svc, "p-be", "/r/backend")
	admin := registerPane(svc, "p-admin", "/r/admin")
	other := registerPane(svc, "p-x", "/r/elsewhere")

	if n := svc.NotifyGroup("CL", "admin", "admin joined this window"); n != 2 {
		t.Fatalf("reached %d sessions, want frontend and backend", n)
	}
	for _, id := range []string{fe.PeerID, be.PeerID} {
		evs, err := svc.Poll(id)
		if err != nil || len(evs) != 1 || evs[0].Text != "admin joined this window" || evs[0].FromName != "claude-peers" {
			t.Fatalf("%s got %v %v", id, evs, err)
		}
	}
	for _, id := range []string{admin.PeerID, other.PeerID} {
		if evs, _ := svc.Poll(id); len(evs) != 0 {
			t.Fatalf("%s must not hear it: %v", id, evs)
		}
	}
}
