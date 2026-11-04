package api

import (
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// trunkGit answers the one git question a send-back asks.
type trunkGit struct{}

func (trunkGit) Trunk(string) (string, error)                     { return "main", nil }
func (trunkGit) AddWorktree(string, string, string, string) error { return nil }

// "Send back to worker" end to end on a real pane and the real bus: the
// button is offered while the worker's pane is open, pressing it puts the
// worker's Claude on notice over the bus with the failure, marks it
// prompted (so its old Stop is not read as "finished without reporting"),
// and runs the part and the task again. A worker pane with no Claude on
// the bus is refused and its signal left as it was.
func TestSendBack_ThroughTheRealBus(t *testing.T) {
	f := newWindowAgentFixture(t, "sleep 1;:")
	f.srv.EnablePeers(peers.NewService(f.st, f.srv.mgr, testSecret))
	f.srv.SetTasks(f.st)
	mgr := f.srv.mgr
	paneID, err := mgr.StartTaskPane(f.ws.ID, "/tmp", "sleep 600")
	if err != nil {
		t.Fatal(err)
	}
	mgr.ApplyAgentSignal(paneID, model.AttentionDone, model.ReasonFinished)
	worker, _ := registerOnPane(t, f, paneID, "/tmp/app-task-1")
	if _, err := f.st.AddTaskWithParts(store.Task{Title: "x", Status: taskq.MergeFailed, Note: "app: checks failed:\nFAIL: TestX"},
		[]store.Part{{RepoPath: "/tmp", Plan: "p", Status: taskq.Ready}}); err != nil {
		t.Fatal(err)
	}
	parts, _ := f.st.Parts()
	parts[0].PaneID = paneID
	f.st.SavePart(parts[0])

	if v := tasksOf(t, f.srv).Tasks[0].Parts[0]; !v.SendBackable {
		t.Fatalf("a built part of a failed merge with an open worker pane is not offered: %+v", v)
	}
	f.srv.taskRunner = &taskq.Runner{Store: f.st, Lock: &f.srv.tasksMu, Now: time.Now, Git: trunkGit{},
		Host: taskHost{mgr: mgr, peers: f.srv.peersSvc}}
	if rec := do(t, f.srv, "POST", "/v1/tasks/1/parts/1/send-back", ""); rec.Code != 200 {
		t.Fatalf("send back = %d %s", rec.Code, rec.Body)
	}
	evs, _ := f.srv.peersSvc.Poll(worker)
	if len(evs) != 1 || !strings.Contains(evs[0].Text, "FAIL: TestX") {
		t.Fatalf("the worker heard %+v", evs)
	}
	if st, _ := mgr.TaskPane(paneID); st.Signal.Attention != model.AttentionIdle {
		t.Fatalf("worker signal = %+v, want prompted", st.Signal)
	}
	got := tasksOf(t, f.srv).Tasks[0]
	if got.Status != taskq.Running || got.Parts[0].Status != taskq.Running || got.Parts[0].SendBackable {
		t.Fatalf("after send back: task %s, part %s, still offered %v", got.Status, got.Parts[0].Status, got.Parts[0].SendBackable)
	}

	deaf, err := mgr.StartTaskPane(f.ws.ID, "/tmp/other", "sleep 600")
	if err != nil {
		t.Fatal(err)
	}
	mgr.ApplyAgentSignal(deaf, model.AttentionDone, model.ReasonFinished)
	if err := (taskHost{mgr: mgr, peers: f.srv.peersSvc}).TellWorker(deaf, "hi"); err == nil {
		t.Fatal("a pane with no Claude on the bus was reported told")
	}
	if st, _ := mgr.TaskPane(deaf); st.Signal.Attention != model.AttentionDone {
		t.Fatalf("an unheard message changed the signal: %+v", st.Signal)
	}
}
