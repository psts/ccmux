package peers

import (
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

// twoPeers registers a delegator and a worker in the same window group and
// returns their ids.
func twoPeers(t *testing.T, svc *Service, hook *fakeHook) (delegator, worker string) {
	t.Helper()
	hook.groups["pane-d"] = "G"
	hook.groups["pane-w"] = "G"
	d := registerPane(svc, "pane-d", "/r/delegator")
	w := registerPane(svc, "pane-w", "/r/worker")
	return d.PeerID, w.PeerID
}

func pollTexts(t *testing.T, svc *Service, peerID string) []string {
	t.Helper()
	evs, err := svc.Poll(peerID)
	if err != nil {
		t.Fatalf("poll %s: %v", peerID, err)
	}
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Text)
	}
	return out
}

func assertTaskRow(t *testing.T, st *store.SQLite, taskID, from, to, text string) {
	t.Helper()
	row, err := st.PeerTask(taskID)
	if err != nil || row == nil {
		t.Fatalf("task row missing: %v %v", row, err)
	}
	if row.Status != "sent" || row.FromID != from || row.ToID != to || row.Text != text {
		t.Fatalf("task row = %+v", row)
	}
}

func TestDelegate_CreatesTaskAndDeliversHeaderedMessage(t *testing.T) {
	svc, hook, st := newTestServiceWithStore(t)
	delegator, worker := twoPeers(t, svc, hook)

	resp := svc.Delegate(DelegateReq{FromID: delegator, ToID: worker, Text: "rename the field"})
	if !resp.OK || !strings.HasPrefix(resp.TaskID, "tsk_") {
		t.Fatalf("delegate = %+v, want OK with tsk_ id", resp)
	}

	assertTaskRow(t, st, resp.TaskID, delegator, worker, "rename the field")

	texts := pollTexts(t, svc, worker)
	if len(texts) != 1 {
		t.Fatalf("worker got %d messages, want 1", len(texts))
	}
	if !strings.Contains(texts[0], "[claude-peers delegation task "+resp.TaskID+"]") ||
		!strings.Contains(texts[0], "rename the field") {
		t.Fatalf("delegation message = %q — header or payload missing", texts[0])
	}
}

func TestUpdateTask_ReportsFlowToDelegator(t *testing.T) {
	svc, hook, st := newTestServiceWithStore(t)
	delegator, worker := twoPeers(t, svc, hook)
	taskID := svc.Delegate(DelegateReq{FromID: delegator, ToID: worker, Text: "do it"}).TaskID
	_ = pollTexts(t, svc, worker) // drain the delegation itself

	if resp := svc.UpdateTask(TaskUpdateReq{PeerID: worker, TaskID: taskID, Status: "acked"}); !resp.OK {
		t.Fatalf("ack: %+v", resp)
	}
	if resp := svc.UpdateTask(TaskUpdateReq{PeerID: worker, TaskID: taskID, Status: "completed",
		Result: "renamed in 3 files, tests green"}); !resp.OK {
		t.Fatalf("complete: %+v", resp)
	}

	texts := pollTexts(t, svc, delegator)
	if len(texts) != 2 {
		t.Fatalf("delegator got %d updates, want 2: %q", len(texts), texts)
	}
	if !strings.Contains(texts[0], "[claude-peers task update] "+taskID+": acked") {
		t.Fatalf("first update = %q", texts[0])
	}
	if !strings.Contains(texts[1], taskID+": completed") || !strings.Contains(texts[1], "renamed in 3 files") {
		t.Fatalf("completion update = %q — result missing", texts[1])
	}
	if row, _ := st.PeerTask(taskID); row.Status != "completed" || row.Result == "" {
		t.Fatalf("closed row = %+v", row)
	}
}

// The reason the rows are durable: the delegator's session can be gone when
// the worker reports. The update must land in its queue and replay when the
// same pane re-registers (same derived id).
func TestUpdateTask_SurvivesDelegatorRestart(t *testing.T) {
	svc, hook, _ := newTestServiceWithStore(t)
	delegator, worker := twoPeers(t, svc, hook)
	taskID := svc.Delegate(DelegateReq{FromID: delegator, ToID: worker, Text: "long job"}).TaskID
	_ = pollTexts(t, svc, worker)

	svc.Unregister(delegator)
	if resp := svc.UpdateTask(TaskUpdateReq{PeerID: worker, TaskID: taskID, Status: "completed",
		Result: "done while you were away"}); !resp.OK {
		t.Fatalf("update with delegator away: %+v", resp)
	}

	back := registerPane(svc, "pane-d", "/r/delegator")
	if back.PeerID != delegator {
		t.Fatalf("pane re-register changed id: %s → %s", delegator, back.PeerID)
	}
	texts := pollTexts(t, svc, delegator)
	if len(texts) != 1 || !strings.Contains(texts[0], "done while you were away") {
		t.Fatalf("replayed updates = %q, want the completion", texts)
	}
}

func TestUpdateTask_Validation(t *testing.T) {
	svc, hook, st := newTestServiceWithStore(t)
	delegator, worker := twoPeers(t, svc, hook)
	hook.groups["pane-i"] = "G"
	intruder := registerPane(svc, "pane-i", "/r/intruder").PeerID
	taskID := svc.Delegate(DelegateReq{FromID: delegator, ToID: worker, Text: "x"}).TaskID

	cases := map[string]TaskUpdateReq{
		"unknown task": {PeerID: worker, TaskID: "tsk_nope", Status: "acked"},
		"bad status":   {PeerID: worker, TaskID: taskID, Status: "paused"},
		"wrong worker": {PeerID: intruder, TaskID: taskID, Status: "acked"},
		"unregistered": {PeerID: "ghost", TaskID: taskID, Status: "acked"},
	}
	for name, req := range cases {
		if resp := svc.UpdateTask(req); resp.OK {
			t.Errorf("%s: accepted %+v", name, req)
		}
	}

	// Closed means closed.
	svc.UpdateTask(TaskUpdateReq{PeerID: worker, TaskID: taskID, Status: "failed", Result: "no"})
	if resp := svc.UpdateTask(TaskUpdateReq{PeerID: worker, TaskID: taskID, Status: "working"}); resp.OK {
		t.Fatal("reopened a closed task")
	}
	if row, _ := st.PeerTask(taskID); row.Status != "failed" {
		t.Fatalf("row after reopen attempt = %+v", row)
	}
}

// A spawn_if_missing delegation has no worker yet; the row is claimed by the
// first update_task from whichever peer received the queued message.
func TestUpdateTask_SpawnedWorkerClaimsTask(t *testing.T) {
	svc, hook, st := newTestServiceWithStore(t)
	delegator, worker := twoPeers(t, svc, hook)

	taskID := "tsk_claim1"
	if err := st.SavePeerTask(store.PeerTask{TaskID: taskID, FromID: delegator,
		Group: "G", Text: "spawned work", Status: "sent",
		CreatedAt: svc.Now().UnixMilli(), UpdatedAt: svc.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if resp := svc.UpdateTask(TaskUpdateReq{PeerID: worker, TaskID: taskID, Status: "acked"}); !resp.OK {
		t.Fatalf("claim: %+v", resp)
	}
	if row, _ := st.PeerTask(taskID); row.ToID != worker {
		t.Fatalf("claim did not bind worker: %+v", row)
	}
}

// The delegator may CLOSE its own task (cancel), but never report progress on
// it — progress is the worker's to claim.
func TestUpdateTask_DelegatorMayCloseNotReport(t *testing.T) {
	svc, hook, st := newTestServiceWithStore(t)
	delegator, worker := twoPeers(t, svc, hook)
	taskID := svc.Delegate(DelegateReq{FromID: delegator, ToID: worker, Text: "x"}).TaskID
	_ = pollTexts(t, svc, worker)

	if resp := svc.UpdateTask(TaskUpdateReq{PeerID: delegator, TaskID: taskID, Status: "working"}); resp.OK {
		t.Fatal("delegator reported progress on its own task")
	}
	if resp := svc.UpdateTask(TaskUpdateReq{PeerID: delegator, TaskID: taskID, Status: "failed",
		Message: "cancelled, plans changed"}); !resp.OK {
		t.Fatalf("delegator cancel: %+v", resp)
	}
	if row, _ := st.PeerTask(taskID); row.Status != "failed" {
		t.Fatalf("cancelled row = %+v", row)
	}
	// The worker hears about the cancellation.
	texts := pollTexts(t, svc, worker)
	if len(texts) != 1 || !strings.Contains(texts[0], "cancelled, plans changed") {
		t.Fatalf("worker notification = %q", texts)
	}
}

// A spawn that never registers must close the delegation it carried — an open
// task whose worker will never exist would sit in the delegator's open list
// forever, with nobody able to close it.
func TestSpawnFailure_ClosesCarriedTasks(t *testing.T) {
	svc, hook, st := newTestServiceWithStore(t)
	delegator, _ := twoPeers(t, svc, hook)

	taskID := "tsk_orphan"
	if err := st.SavePeerTask(store.PeerTask{TaskID: taskID, FromID: delegator,
		Group: "G", Text: "never picked up", Status: "sent",
		CreatedAt: svc.Now().UnixMilli(), UpdatedAt: svc.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	pending := &pendingSpawn{name: "ghostrepo", group: "G", requests: []queuedRequest{
		{fromID: delegator, text: taskHeader(taskID) + "\n\nnever picked up"},
		{fromID: delegator, text: "a plain spawn message with no task"},
	}}
	svc.mu.Lock()
	svc.failTasksInLocked(pending, "ccmux did not register it")
	svc.mu.Unlock()

	row, _ := st.PeerTask(taskID)
	if row.Status != "failed" || !strings.Contains(row.StatusMessage, "did not register") {
		t.Fatalf("orphaned task after spawn failure = %+v", row)
	}
}

func TestOpenTasks_BothDirectionsOpenOnly(t *testing.T) {
	svc, hook, _ := newTestServiceWithStore(t)
	delegator, worker := twoPeers(t, svc, hook)

	open := svc.Delegate(DelegateReq{FromID: delegator, ToID: worker, Text: "open one"}).TaskID
	closed := svc.Delegate(DelegateReq{FromID: delegator, ToID: worker, Text: "closed one"}).TaskID
	svc.UpdateTask(TaskUpdateReq{PeerID: worker, TaskID: closed, Status: "completed", Result: "r"})

	for _, peer := range []string{delegator, worker} {
		tasks, err := svc.OpenTasks(peer)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 || tasks[0].TaskID != open {
			t.Fatalf("open tasks for %s = %+v, want only %s", peer, tasks, open)
		}
	}
	if _, err := svc.OpenTasks("ghost"); err == nil {
		t.Fatal("unregistered peer listed tasks")
	}
}
