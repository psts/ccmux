package manager

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// activityManager is a cold manager over a real registry holding one agent
// pane, so a second manager on the same file is a daemon restart.
func activityManager(t *testing.T, st store.Store) *Manager {
	t.Helper()
	m := New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	m.byID["ws"] = &entry{ws: &model.Workspace{ID: "ws", Panes: []*model.Pane{
		{ID: "a1", WorkspaceID: "ws", Agent: "radar", Harness: "claude", RawCommand: "claude"},
	}}}
	return m
}

func activityStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SavePane(&model.Pane{ID: "a1", WorkspaceID: "ws", Agent: "radar"}); err != nil {
		t.Fatal(err)
	}
	return st
}

// The bug this exists for: a restart forgot the idle clock, the fallback read
// a looked-at pane's "idle" attention as busy, and the agent never exited.
// After a restart the clock is the one the last daemon recorded.
func TestActivitySurvivesARestart(t *testing.T) {
	st := activityStore(t)
	t0 := time.Now().Add(-13 * time.Hour).Truncate(time.Millisecond)
	activityManager(t, st).activity.note("a1", stateIdle, t0)

	m := activityManager(t, st)
	m.loadActivity()
	act, ok := m.activity.get("a1")
	if !ok || act.state != stateIdle || !act.since.Equal(t0) {
		t.Fatalf("after restart: %+v ok=%v, want idle since %v", act, ok, t0)
	}
	d := agent.Definition{IdleExitMinutes: 720}
	if got := decideAgent(m.agentViewFor(*m.byID["ws"].ws.Panes[0], d, time.Now()), time.Now()); got != actExit {
		t.Fatalf("decision after restart = %v, want exit: 13h idle against a 12h cap", got)
	}
}

// No reading at all (a pane from before the columns, or a write that failed)
// is idle from the first look, whatever the attention says. Reading "idle"
// attention as busy is what kept radar's session alive for a day.
func TestNoReadingIsIdleFromFirstLook(t *testing.T) {
	m := activityManager(t, activityStore(t))
	p := *m.byID["ws"].ws.Panes[0]
	p.Attention = model.AttentionIdle // what a human's look leaves behind
	now := time.Now()
	v := m.agentViewFor(p, agent.Definition{IdleExitMinutes: 10}, now)
	if !v.idle || !v.idleSince.Equal(now) {
		t.Fatalf("view = %+v, want idle since now", v)
	}
	if _, ok := m.activity.get("a1"); !ok {
		t.Fatal("first look was not recorded, so the clock restarts every tick")
	}
}

// At a shell the session is over, and so is its clock: the next start must
// not inherit an idle stretch that began before it, or it ends at once.
func TestShellClearsTheClock(t *testing.T) {
	st := activityStore(t)
	m := activityManager(t, st)
	m.activity.note("a1", stateIdle, time.Now().Add(-time.Hour))
	p := *m.byID["ws"].ws.Panes[0]
	p.RawCommand = "bash"
	m.agentViewFor(p, agent.Definition{IdleExitMinutes: 10}, time.Now())
	if _, ok := m.activity.get("a1"); ok {
		t.Fatal("clock kept at a shell")
	}
	if stored, _ := st.PaneActivities(); len(stored) != 0 {
		t.Fatalf("stored clock kept at a shell: %+v", stored)
	}
}

// A busy or waiting reading from before a restart may have lost the signal
// that ended it (the sidecar does not retry). It is believed for an hour,
// then reads idle from that point: before this, nothing short of a new prompt
// ever ended it, and the agent's schedules waited on it forever.
func TestRestoredBusyIsTrustedForAnHourOnly(t *testing.T) {
	st := activityStore(t)
	activityManager(t, st).activity.note("a1", stateBusy, time.Now().Add(-3*time.Hour))

	m := activityManager(t, st)
	m.loadActivity()
	if !m.AgentBusy("a1") {
		t.Fatal("a restored busy reading was dropped at once: a turn running across a restart would be cut")
	}
	later := time.Now().Add(restoredTrust + time.Minute)
	act, _ := m.activity.current("a1", later)
	if act.state != stateIdle || act.since.After(later) || later.Sub(act.since) > 2*time.Minute {
		t.Fatalf("after the trust window: %+v, want idle since the window closed", act)
	}
	if stored, _ := st.PaneActivities(); stored["a1"].State != "idle" {
		t.Fatalf("the expiry was not saved: %+v", stored)
	}
	// A fresh signal is trusted as always.
	m.activity.note("a1", stateBusy, later)
	if act, _ := m.activity.current("a1", later.Add(5*time.Hour)); act.state != stateBusy {
		t.Fatalf("a live busy reading expired: %+v", act)
	}
}

// A live signal repeating the restored state confirms it: opencode sends busy
// on every message, so a confirmed turn must not flip idle at the hour mark.
func TestRepeatedSignalConfirmsARestoredReading(t *testing.T) {
	st := activityStore(t)
	since := time.Now().Add(-time.Hour)
	activityManager(t, st).activity.note("a1", stateBusy, since)
	m := activityManager(t, st)
	m.loadActivity()
	m.activity.note("a1", stateBusy, time.Now())
	act, _ := m.activity.current("a1", time.Now().Add(restoredTrust+time.Hour))
	if act.state != stateBusy || !act.since.Equal(since.Truncate(time.Millisecond)) {
		t.Fatalf("confirmed reading = %+v, want busy since %v", act, since)
	}
}

// A card open across a restart keeps waiting on its own clock: turning it
// idle would cut a 12-hour card wait to an hour, and under an opencode dialog
// would send ctrl-d into the dialog.
func TestRestoredWaitingKeepsItsCardClock(t *testing.T) {
	st := activityStore(t)
	since := time.Now().Add(-3 * time.Hour).Truncate(time.Millisecond)
	activityManager(t, st).activity.note("a1", stateWaiting, since)
	m := activityManager(t, st)
	m.loadActivity()
	act, _ := m.activity.current("a1", time.Now().Add(5*time.Hour))
	if act.state != stateWaiting || !act.since.Equal(since) {
		t.Fatalf("restored card = %+v, want waiting since %v", act, since)
	}
	d := agent.Definition{IdleExitMinutes: 10, CardWaitMinutes: 720}
	if got := decideAgent(m.agentViewFor(*m.byID["ws"].ws.Panes[0], d, time.Now()), time.Now()); got != actNone {
		t.Fatalf("a card 3h into a 12h wait was ended after a restart: %v", got)
	}
}

// A revive starts a new session: the old clock goes, from memory and from
// the registry, or the next daemon start would hand it back.
func TestResetClearsMemoryAndRegistry(t *testing.T) {
	st := activityStore(t)
	m := activityManager(t, st)
	m.activity.note("a1", stateIdle, time.Now().Add(-2*time.Hour))
	m.activity.reset("a1")
	if _, ok := m.activity.get("a1"); ok {
		t.Fatal("reading kept in memory")
	}
	if stored, _ := st.PaneActivities(); len(stored) != 0 {
		t.Fatalf("reading kept in the registry: %+v", stored)
	}
}

// A pane stopped on a human takes no pushed prompt: it would be typed into
// the card.
func TestWaitingIsBusyForPushes(t *testing.T) {
	m := activityManager(t, activityStore(t))
	m.ApplyAttention("a1", model.AttentionNeedsInput, model.ReasonQuestion)
	if !m.AgentBusy("a1") {
		t.Fatal("a pane showing a question card read as free to push into")
	}
	m.ApplyAttention("a1", model.AttentionDone, model.ReasonFinished)
	if m.AgentBusy("a1") {
		t.Fatal("a finished pane read as busy")
	}
}

// holdsDialog comes from the pane's harness, and it is the only thing that
// stops ctrl-d going into an opencode dialog. Driven through the real signal
// and agentViewFor, not a hand-set view: a Claude pane's card still ends on
// its limit, an opencode one never does.
func TestWaitingExitDependsOnTheHarness(t *testing.T) {
	for _, c := range []struct {
		harness string
		want    agentAction
	}{
		{"opencode", actNone},
		{"claude", actExit},
		{"", actExit}, // a pane from before the harness column: Claude's rule
	} {
		m := activityManager(t, activityStore(t))
		m.byID["ws"].ws.Panes[0].Harness = c.harness
		m.ApplyAgentSignal("a1", model.AttentionNeedsInput, model.ReasonNone)
		later := time.Now().Add(48 * time.Hour)
		v := m.agentViewFor(*m.byID["ws"].ws.Panes[0], agent.Definition{IdleExitMinutes: 10}, later)
		if got := decideAgent(v, later); got != c.want {
			t.Errorf("harness %q waiting 48h: %v, want %v", c.harness, got, c.want)
		}
	}
}

// pi, codex and aider never report their state, so no reading is all they
// will ever have. Read as idle, that would end them mid-task every cap; they
// are never ended blindly. A harness that reports still gets the idle default.
func TestSilentHarnessIsNeverEndedBlindly(t *testing.T) {
	for _, c := range []struct {
		harness string
		want    agentAction
	}{
		{"codex", actNone},
		{"pi", actNone},
		{"claude", actExit},
		{"opencode", actExit},
	} {
		m := activityManager(t, activityStore(t))
		p := *m.byID["ws"].ws.Panes[0]
		p.Harness = c.harness
		d := agent.Definition{IdleExitMinutes: 10}
		now := time.Now()
		m.agentViewFor(p, d, now) // the first tick
		later := now.Add(11 * time.Minute)
		if got := decideAgent(m.agentViewFor(p, d, later), later); got != c.want {
			t.Errorf("harness %q with no signal, 11 min on: %v, want %v", c.harness, got, c.want)
		}
	}
}
