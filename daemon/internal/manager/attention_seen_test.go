package manager

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// seenManager builds a cold manager with one workspace of four panes, one per
// attention value, so a test can see exactly which ones a look retires.
func seenManager(t *testing.T) (*Manager, <-chan Event) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	m.byID["ws"] = &entry{ws: &model.Workspace{ID: "ws", Panes: []*model.Pane{
		{ID: "done", WorkspaceID: "ws", Attention: model.AttentionDone, Agent: "bot"},
		{ID: "needs", WorkspaceID: "ws", Attention: model.AttentionNeedsInput},
		{ID: "idle", WorkspaceID: "ws", Attention: model.AttentionIdle},
		{ID: "running", WorkspaceID: "ws", Attention: model.AttentionRunning},
	}}}
	_, ch := m.events.subscribe()
	return m, ch
}

func drain(ch <-chan Event) []Event {
	var out []Event
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestMarkSeen_RetiresFlashesAndNothingElse(t *testing.T) {
	m, ch := seenManager(t)
	m.activity.note("done", false, time.Now())

	m.MarkSeen("ws")

	want := map[string]model.Attention{
		"done": model.AttentionIdle, "needs": model.AttentionIdle,
		"idle": model.AttentionIdle, "running": model.AttentionRunning,
	}
	for _, p := range m.byID["ws"].ws.Panes {
		if p.Attention != want[p.ID] {
			t.Errorf("pane %s = %q, want %q", p.ID, p.Attention, want[p.ID])
		}
	}
	evs := drain(ch)
	if len(evs) != 2 {
		t.Fatalf("published %d events, want one per cleared pane: %+v", len(evs), evs)
	}
	for _, ev := range evs {
		if ev.Kind != "attention" || ev.WorkspaceID != "ws" || ev.Attention != model.AttentionIdle {
			t.Errorf("event %+v", ev)
		}
	}
	// A human looking is not the agent starting work.
	if act, ok := m.activity.get("done"); !ok || act.busy {
		t.Fatalf("lifecycle record changed by a look: %+v ok=%v", act, ok)
	}
}

func TestMarkSeen_UnknownWorkspaceIsQuiet(t *testing.T) {
	m, ch := seenManager(t)
	m.MarkSeen("nope")
	if evs := drain(ch); len(evs) != 0 {
		t.Fatalf("events for an unknown workspace: %+v", evs)
	}
}

// A pane that finishes while someone is looking ends up idle, but the hook's
// own value goes out FIRST: the notifier and the alert flag read that frame,
// so push routing is untouched by who happens to be watching. The lifecycle
// reads the real hook too, or an agent finishing under your eyes would look busy.
func TestApplyAttention_WatchedWorkspaceIsRetiredAfterTheHookGoesOut(t *testing.T) {
	m, ch := seenManager(t)
	watched := true
	m.Watched = func(wsID string) bool { return wsID == "ws" && watched }

	m.ApplyAttention("idle", model.AttentionDone)
	if got := m.byID["ws"].ws.Panes[2].Attention; got != model.AttentionIdle {
		t.Fatalf("watched done stored as %q, want idle", got)
	}
	evs := drain(ch)
	first, last := evs[0], evs[len(evs)-1]
	if first.PaneID != "idle" || first.Attention != model.AttentionDone {
		t.Fatalf("first frame %+v, want the hook as sent", first)
	}
	if last.PaneID != "idle" || last.Attention != model.AttentionIdle {
		t.Fatalf("last frame %+v, want the retire to idle", last)
	}

	m.ApplyAttention("done", model.AttentionDone) // an agent pane
	if act, ok := m.activity.get("done"); !ok || act.busy {
		t.Fatalf("lifecycle read the shown value, not the hook: %+v ok=%v", act, ok)
	}

	watched = false
	m.ApplyAttention("idle", model.AttentionNeedsInput)
	if got := m.byID["ws"].ws.Panes[2].Attention; got != model.AttentionNeedsInput {
		t.Fatalf("unwatched needs_input stored as %q", got)
	}
}

func TestApplyAttention_NoWatcherWiredShowsEverything(t *testing.T) {
	m, _ := seenManager(t)
	m.ApplyAttention("idle", model.AttentionDone)
	if got := m.byID["ws"].ws.Panes[2].Attention; got != model.AttentionDone {
		t.Fatalf("with Watched nil, done stored as %q", got)
	}
}
