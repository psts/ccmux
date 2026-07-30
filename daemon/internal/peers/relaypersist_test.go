package peers

import (
	"path/filepath"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// restartableService opens a store that outlives the Service built over it, so a
// second Service can be constructed against the same database — a daemon restart.
func restartableService(t *testing.T) (*Service, *fakeHook, func() (*Service, *fakeHook)) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Each call builds a Service over the SAME store with a fresh in-memory
	// state — which is exactly what a daemon restart is.
	build := func() (*Service, *fakeHook) {
		hook := &fakeHook{groups: map[string]string{}, repos: map[string]string{}, shells: map[string]bool{}}
		svc := NewService(st, hook, []byte("test-secret-test-secret-test-sec"))
		svc.OpenCmd = ""
		return svc, hook
	}
	svc, hook := build()
	return svc, hook, build
}

// The bug: a tool-approval dialog can sit open for hours, but the request lived
// only in memory. A daemon restart in between orphaned it — the delegator's
// "yes <id>" no longer matched anything and was delivered to the worker as
// ordinary chat, while the worker went on waiting.
func TestRelayPersist_PermissionRequestSurvivesRestart(t *testing.T) {
	svc, _, rebuild := restartableService(t)

	worker := registerPane(svc, "pane-worker", "/repo")
	boss := registerPane(svc, "pane-boss", "/repo")
	// The boss has to have messaged the worker for the relay to reach them.
	if resp := svc.Send(SendReq{FromID: boss.PeerID, ToID: worker.PeerID, Text: "do the thing"}); !resp.OK {
		t.Fatalf("seeding message failed: %+v", resp)
	}
	if n, err := svc.PermissionRequest(worker.PeerID, "abcde", "Bash", "rm -rf", "rm -rf /tmp/x"); err != nil || n != 1 {
		t.Fatalf("PermissionRequest = (%d, %v), want (1, nil)", n, err)
	}

	// Daemon restart.
	fresh, _ := rebuild()
	fresh.loadRelayState()
	registerPane(fresh, "pane-worker", "/repo")
	registerPane(fresh, "pane-boss", "/repo")

	fresh.mu.Lock()
	pr := fresh.perms["abcde"]
	fresh.mu.Unlock()
	if pr == nil {
		t.Fatal("the outstanding permission request did not survive the restart")
	}
	if pr.workerID != worker.PeerID {
		t.Errorf("request points at %q, want the worker %q", pr.workerID, worker.PeerID)
	}
	if pr.resolved {
		t.Error("an unanswered request came back marked resolved")
	}
}

// End to end across the restart: the verdict still resolves the dialog, which is
// the behavior the persistence exists for.
func TestRelayPersist_VerdictStillResolvesAfterRestart(t *testing.T) {
	svc, _, rebuild := restartableService(t)
	worker := registerPane(svc, "pane-worker", "/repo")
	boss := registerPane(svc, "pane-boss", "/repo")
	svc.Send(SendReq{FromID: boss.PeerID, ToID: worker.PeerID, Text: "do the thing"})
	if _, err := svc.PermissionRequest(worker.PeerID, "abcde", "Bash", "rm -rf", "x"); err != nil {
		t.Fatal(err)
	}

	fresh, _ := rebuild()
	fresh.loadRelayState()
	w2 := registerPane(fresh, "pane-worker", "/repo")
	b2 := registerPane(fresh, "pane-boss", "/repo")

	resp := fresh.Send(SendReq{FromID: b2.PeerID, ToID: w2.PeerID, Text: "yes abcde"})
	if !resp.OK {
		t.Fatalf("verdict send failed: %+v", resp)
	}

	evs, err := fresh.Poll(w2.PeerID)
	if err != nil {
		t.Fatal(err)
	}
	var verdicts int
	for _, ev := range evs {
		if ev.Kind == "permission_verdict" {
			verdicts++
			if ev.RequestID != "abcde" || ev.Behavior != "allow" {
				t.Errorf("verdict = %s/%s, want abcde/allow", ev.RequestID, ev.Behavior)
			}
		}
	}
	if verdicts != 1 {
		t.Errorf("worker received %d verdicts, want 1 — the reply arrived as plain chat instead", verdicts)
	}
}

// A request already past its 12h TTL when the daemon comes back must not be
// resurrected, or the table would grow forever and a stale id could match.
func TestRelayPersist_ExpiredRequestIsNotReloaded(t *testing.T) {
	svc, _, rebuild := restartableService(t)
	worker := registerPane(svc, "pane-worker", "/repo")
	boss := registerPane(svc, "pane-boss", "/repo")
	svc.Send(SendReq{FromID: boss.PeerID, ToID: worker.PeerID, Text: "hi"})
	if _, err := svc.PermissionRequest(worker.PeerID, "abcde", "Bash", "x", "x"); err != nil {
		t.Fatal(err)
	}

	fresh, _ := rebuild()
	fresh.Now = func() time.Time { return time.Now().Add(permRequestTTL + time.Hour) }
	fresh.loadRelayState()

	fresh.mu.Lock()
	_, present := fresh.perms["abcde"]
	fresh.mu.Unlock()
	if present {
		t.Error("a request past its TTL was reloaded")
	}
}

// The bug: a cross-group message opens a two-hour return path, but the licence
// lived only in memory. A restart inside that window told the teammate it could
// not send messages across projects when it tried to answer.
func TestRelayPersist_ReplyGrantSurvivesRestart(t *testing.T) {
	svc, hook, rebuild := restartableService(t)
	// Two peers in different groups.
	hook.groups["pane-a"] = "alpha"
	hook.groups["pane-b"] = "beta"
	a := registerPane(svc, "pane-a", "/a")
	b := registerPane(svc, "pane-b", "/b")

	// A reaches into beta explicitly, which licenses B to answer.
	if resp := svc.Send(SendReq{FromID: a.PeerID, ToID: b.PeerID, ToGroup: "beta", Text: "check in"}); !resp.OK {
		t.Fatalf("cross-group send failed: %+v", resp)
	}

	fresh, freshHook := rebuild()
	freshHook.groups["pane-a"] = "alpha"
	freshHook.groups["pane-b"] = "beta"
	fresh.loadRelayState()
	a2 := registerPane(fresh, "pane-a", "/a")
	b2 := registerPane(fresh, "pane-b", "/b")

	// B answers A with no to_group — only the grant can permit this.
	resp := fresh.Send(SendReq{FromID: b2.PeerID, ToID: a2.PeerID, Text: "done"})
	if !resp.OK {
		t.Errorf("reply across the boundary was refused after a restart: %q", resp.Error)
	}
}

func TestRelayPersist_ExpiredGrantIsNotReloaded(t *testing.T) {
	svc, hook, rebuild := restartableService(t)
	hook.groups["pane-a"] = "alpha"
	hook.groups["pane-b"] = "beta"
	a := registerPane(svc, "pane-a", "/a")
	b := registerPane(svc, "pane-b", "/b")
	svc.Send(SendReq{FromID: a.PeerID, ToID: b.PeerID, ToGroup: "beta", Text: "check in"})

	fresh, freshHook := rebuild()
	fresh.Now = func() time.Time { return time.Now().Add(replyGrantTTL + time.Minute) }
	freshHook.groups["pane-a"] = "alpha"
	freshHook.groups["pane-b"] = "beta"
	fresh.loadRelayState()

	fresh.mu.Lock()
	n := len(fresh.replyGrants)
	fresh.mu.Unlock()
	if n != 0 {
		t.Errorf("%d expired grant(s) reloaded, want 0", n)
	}
}
