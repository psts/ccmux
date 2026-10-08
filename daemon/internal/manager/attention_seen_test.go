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

	m.ApplyAttention("idle", model.AttentionDone, model.ReasonFinished)
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

	m.ApplyAttention("done", model.AttentionDone, model.ReasonFinished) // an agent pane
	if act, ok := m.activity.get("done"); !ok || act.busy {
		t.Fatalf("lifecycle read the shown value, not the hook: %+v ok=%v", act, ok)
	}

	watched = false
	m.ApplyAttention("idle", model.AttentionNeedsInput, model.ReasonPermission)
	if got := m.byID["ws"].ws.Panes[2].Attention; got != model.AttentionNeedsInput {
		t.Fatalf("unwatched needs_input stored as %q", got)
	}
}

func TestApplyAttention_NoWatcherWiredShowsEverything(t *testing.T) {
	m, _ := seenManager(t)
	m.ApplyAttention("idle", model.AttentionDone, model.ReasonFinished)
	if got := m.byID["ws"].ws.Panes[2].Attention; got != model.AttentionDone {
		t.Fatalf("with Watched nil, done stored as %q", got)
	}
}

// A claim is stored with its reason and start, and the firehose carries both;
// the start survives the idle reminder that follows a Stop, and a value that
// claims nothing drops the reason whatever the caller passed.
func TestApplyAttention_StampsTheClaim(t *testing.T) {
	m, ch := seenManager(t)
	pane := func() *model.Pane { return m.byID["ws"].ws.Panes[2] }

	m.ApplyAttention("idle", model.AttentionDone, model.ReasonFinished)
	since := pane().AttentionSince
	if pane().AttentionReason != model.ReasonFinished || since == 0 {
		t.Fatalf("after Stop: %+v", *pane())
	}
	ev := drain(ch)[0]
	if ev.Reason != model.ReasonFinished || ev.Since != since {
		t.Fatalf("firehose event %+v, want the claim alongside", ev)
	}

	time.Sleep(2 * time.Millisecond)
	m.ApplyAttention("idle", model.AttentionNeedsInput, model.ReasonFinished)
	if pane().AttentionSince != since {
		t.Fatalf("idle reminder restarted the wait: %d, want %d", pane().AttentionSince, since)
	}

	m.ApplyAttention("idle", model.AttentionIdle, model.ReasonFinished)
	if p := pane(); p.AttentionReason != model.ReasonNone || p.AttentionSince != 0 {
		t.Fatalf("idle kept a claim: %+v", *p)
	}
}

// The board's "done with this tile" retires that one pane and leaves the
// workspace's other claims standing, unlike a look (MarkSeen).
func TestMarkActed_RetiresOnePane(t *testing.T) {
	m, ch := seenManager(t)
	m.ApplyAttention("needs", model.AttentionNeedsInput, model.ReasonPermission)
	drain(ch)

	m.MarkActed("ws", "needs")

	want := map[string]model.Attention{
		"done": model.AttentionDone, "needs": model.AttentionIdle,
		"idle": model.AttentionIdle, "running": model.AttentionRunning,
	}
	for _, p := range m.byID["ws"].ws.Panes {
		if p.Attention != want[p.ID] {
			t.Errorf("pane %s = %q, want %q", p.ID, p.Attention, want[p.ID])
		}
	}
	if p := m.byID["ws"].ws.Panes[1]; p.AttentionReason != model.ReasonNone || p.AttentionSince != 0 {
		t.Errorf("acted pane kept its claim: %+v", *p)
	}
	evs := drain(ch)
	if len(evs) != 1 || evs[0].PaneID != "needs" || evs[0].Attention != model.AttentionIdle {
		t.Fatalf("events %+v, want one idle for the acted pane", evs)
	}
}

// An empty pane, an unknown one, or one that claims nothing retires nothing.
func TestMarkActed_NothingToRetireIsQuiet(t *testing.T) {
	m, ch := seenManager(t)
	m.MarkActed("ws", "")
	m.MarkActed("ws", "nope")
	m.MarkActed("ws", "running")
	m.MarkActed("other", "done")
	if evs := drain(ch); len(evs) != 0 {
		t.Fatalf("events: %+v", evs)
	}
	if got := m.byID["ws"].ws.Panes[0].Attention; got != model.AttentionDone {
		t.Fatalf("done pane = %q, want untouched", got)
	}
}
