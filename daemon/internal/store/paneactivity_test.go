package store

import (
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

// The activity has its own writer, and SavePane must not touch it: a stale
// snapshot saved later would put an old reading back.
func TestPaneActivityRoundTripAndSavePaneLeavesIt(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := &model.Pane{ID: "p1", WorkspaceID: "ws", Agent: "radar"}
	if err := st.SavePane(p); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdatePaneActivity("p1", "waiting", 1234); err != nil {
		t.Fatal(err)
	}
	p.Title = "renamed"
	if err := st.SavePane(p); err != nil {
		t.Fatal(err)
	}
	got, err := st.PaneActivities()
	if err != nil {
		t.Fatal(err)
	}
	if a := got["p1"]; a.State != "waiting" || a.Since != 1234 {
		t.Fatalf("activity = %+v, want waiting at 1234", a)
	}
	if err := st.UpdatePaneActivity("p1", "", 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.PaneActivities(); len(got) != 0 {
		t.Fatalf("cleared activity still listed: %+v", got)
	}
	if err := st.UpdatePaneActivity("gone", "idle", 1); err == nil {
		t.Fatal("a write to a pane with no row must say so")
	}
}

// A task signal has its own writer too, and SavePane leaves it alone.
func TestTaskSignalRoundTripAndSavePaneLeavesIt(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := &model.Pane{ID: "w", WorkspaceID: "ws", CreatedBy: "task-runner"}
	if err := st.SavePane(p); err != nil {
		t.Fatal(err)
	}
	want := TaskSignal{Attention: model.AttentionDone, Reason: model.ReasonFinished, At: 1234}
	if err := st.UpdateTaskSignal("w", want); err != nil {
		t.Fatal(err)
	}
	p.Title = "renamed"
	if err := st.SavePane(p); err != nil {
		t.Fatal(err)
	}
	got, err := st.TaskSignals()
	if err != nil || got["w"] != want || len(got) != 1 {
		t.Fatalf("signals = %+v, %v; want only w at %+v", got, err, want)
	}
	if err := st.UpdateTaskSignal("gone", want); err == nil {
		t.Fatal("a write to a pane with no row must say so")
	}
}
