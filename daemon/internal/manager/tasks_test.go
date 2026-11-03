package manager

import (
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
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
