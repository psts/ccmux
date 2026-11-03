package manager

import (
	"context"
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// Only a task worker's pane is recorded, and a later signal replaces the
// earlier one: the runner reads the last thing the worker said.
func TestTaskSignals_KeepsWorkersLastSignal(t *testing.T) {
	var s taskSignals
	s.note(model.Pane{ID: "human", CreatedBy: "web"}, model.AttentionNeedsInput, model.ReasonQuestion)
	if got := s.get("human"); got.At != 0 {
		t.Fatalf("a human's pane was recorded: %+v", got)
	}
	worker := model.Pane{ID: "w", CreatedBy: taskRunner}
	s.note(worker, model.AttentionNeedsInput, model.ReasonPermission)
	s.note(worker, model.AttentionDone, model.ReasonFinished)
	got := s.get("w")
	if got.Attention != model.AttentionDone || got.Reason != model.ReasonFinished || got.At == 0 {
		t.Fatalf("worker signal = %+v", got)
	}
}

// The bug this exists for: a daemon restart forgot every worker's last
// signal, so a worker deep in one long turn (its prompt-submit hook long
// past, no hook until the turn ends) read as never having started and was
// flagged as needing a person. After a restart the signal is the one the
// last daemon heard.
func TestTaskSignalSurvivesARestart(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	worker := model.Pane{ID: "w", WorkspaceID: "ws", CreatedBy: taskRunner}
	if err := st.SavePane(&worker); err != nil {
		t.Fatal(err)
	}
	before := New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	before.taskSigs.note(worker, model.AttentionIdle, model.ReasonNone)
	heard := before.taskSigs.get("w")

	after := New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	after.loadTaskSignals()
	if got := after.taskSigs.get("w"); got != heard || got.At == 0 {
		t.Fatalf("after restart: %+v, want %+v", got, heard)
	}
}

// A prompt sent from outside the hooks (the runner's message after a failed
// merge) replaces a worker's old Stop with "prompted now", so the runner
// does not read the worker as finished without reporting. A person's pane,
// or a pane the manager does not know, records nothing.
func TestNoteTaskPrompt(t *testing.T) {
	m, st := devhostManager(t)
	givePanes(t, m, st, "w1", "pane-human", "pane-worker")
	m.mu.Lock()
	m.byID["w1"].ws.Panes[1].CreatedBy = taskRunner
	m.mu.Unlock()
	m.taskSigs.note(model.Pane{ID: "pane-worker", CreatedBy: taskRunner}, model.AttentionDone, model.ReasonFinished)
	m.NoteTaskPrompt("pane-worker")
	m.NoteTaskPrompt("pane-human")
	m.NoteTaskPrompt("pane-unknown")
	if got := m.taskSigs.get("pane-worker"); got.Attention != model.AttentionIdle || got.Reason != model.ReasonNone || got.At == 0 {
		t.Fatalf("worker signal = %+v, want prompted", got)
	}
	for _, id := range []string{"pane-human", "pane-unknown"} {
		if got := m.taskSigs.get(id); got.At != 0 {
			t.Fatalf("%s recorded %+v", id, got)
		}
	}
}
