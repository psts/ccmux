package manager

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// agentHomeManager is a manager over a real registry with the home of role
// agent "radar" (no tmux session, no panes) and the session that runs it.
// The base is in a real agent store: idle exit after two minutes.
func agentHomeManager(t *testing.T) (*Manager, *store.SQLite, store.RunnerSession) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	m.Agents = agent.NewStore(filepath.Join(t.TempDir(), "agents"))
	d := agent.Defaults()
	d.Name, d.Description, d.Harness, d.IdleExitMinutes = "radar", "Reads sources and files ideas.", "claude", 2
	if _, err := m.Agents.Save(d); err != nil {
		t.Fatal(err)
	}
	ws, err := m.CreateAgentHome("radar", "tester", "Chart Labs", "radar", "/w/agents/radar")
	if err != nil {
		t.Fatal(err)
	}
	sess := store.RunnerSession{ID: "0a1b2c3d-aaaa-bbbb-cccc-0000000000a1", Name: "radar", Cwd: "/w/agents/radar", Wanted: store.SessionRunning,
		PaneID: "agent1", WorkspaceID: ws.ID, Kind: store.SessionAgent, AgentVersion: "1.0.0", CreatedAt: 5}
	if err := st.AddRunnerSession(sess); err != nil {
		t.Fatal(err)
	}
	m.SyncRunnerPane(sess)
	return m, st, sess
}

func TestAnAgentsHomeNeedsNoTmuxSession(t *testing.T) {
	m, st, sess := agentHomeManager(t)
	home := m.Workspace(sess.WorkspaceID)
	if home == nil || home.Agent != "radar" || home.Status != model.StatusCold || len(home.Panes) != 0 {
		t.Fatalf("the home: a closed workspace that names its base, with no panes: %+v", home)
	}
	saved, err := st.Load()
	if err != nil || len(saved) != 1 || saved[0].Agent != "radar" || saved[0].Group != "Chart Labs" {
		t.Fatalf("it is in the registry, for the next start: %+v %v", saved, err)
	}
	// The session's pane is the agent's pane, wherever an agent's pane is asked for.
	p := m.AgentPane(home.ID, "radar")
	if p == nil || p.ID != "agent1" || p.Runtime != model.RuntimeRunner || p.AgentVersion != "1.0.0" || p.SessionID != sess.ID {
		t.Fatalf("the agent's pane is its session's: %+v", p)
	}
	if info, ok := m.RunnerPane("agent1"); !ok || info.Name != "radar" {
		t.Fatalf("on the bus it is the agent's name: %+v", info)
	}
	if m.RunningAgents() != 1 || m.AgentAsleep("agent1") {
		t.Fatalf("a running agent session counts against the cap: %d", m.RunningAgents())
	}
	sess.Wanted, sess.AgentVersion = store.SessionAsleep, "1.1.0"
	m.SyncRunnerPane(sess)
	if m.RunningAgents() != 0 || !m.AgentAsleep("agent1") {
		t.Fatalf("asleep, it costs nothing: %d", m.RunningAgents())
	}
	if p := m.AgentPane(home.ID, "radar"); p.AgentVersion != "1.1.0" {
		t.Fatalf("the pane follows the version its row keeps: %q", p.AgentVersion)
	}
	if got := storedPaneIDs(t, st); len(got) != 0 {
		t.Fatalf("a session's pane is never a row in panes: %v", got)
	}
}

// lifecycleHooks records what the lifecycle asked for.
type lifecycleHooks struct {
	slept, woken []string
	sleepErr     error
}

func (h *lifecycleHooks) wire(m *Manager) {
	m.SleepAgentSession = func(id string) error { h.slept = append(h.slept, id); return h.sleepErr }
	m.WakeAgent = func(wsID, name string) error { h.woken = append(h.woken, name); return nil }
}

func TestTheLifecycleSleepsAnIdleAgentSession(t *testing.T) {
	m, _, sess := agentHomeManager(t)
	var h lifecycleHooks
	h.wire(m)
	start := time.Now()

	// Working: never ended, however long.
	m.ApplyAgentSignal("agent1", model.AttentionRunning, model.ReasonNone)
	m.agentTick(start.Add(10 * time.Minute))
	if len(h.slept) != 0 {
		t.Fatalf("a working agent was put to sleep: %v", h.slept)
	}
	// Its turn ended: idle, but not yet past its cap.
	m.ApplyAgentSignal("agent1", model.AttentionDone, model.ReasonFinished)
	m.agentTick(time.Now().Add(time.Minute))
	if len(h.slept) != 0 {
		t.Fatalf("put to sleep before its two minutes: %v", h.slept)
	}
	// An open delegation holds it awake past the cap.
	m.OpenTasksForPane = func(string) int { return 1 }
	m.agentTick(time.Now().Add(3 * time.Minute))
	if len(h.slept) != 0 {
		t.Fatalf("put to sleep with work it still owes: %v", h.slept)
	}
	m.OpenTasksForPane = nil
	m.agentTick(time.Now().Add(3 * time.Minute))
	if len(h.slept) != 1 || h.slept[0] != sess.ID {
		t.Fatalf("idle past its cap, it is put to sleep through the door: %v", h.slept)
	}
	// A sleep that was refused (a message it has not read) is tried again.
	h.sleepErr = errors.New("a message waits")
	m.ApplyAgentSignal("agent1", model.AttentionDone, model.ReasonFinished)
	m.agentTick(time.Now().Add(6 * time.Minute))
	m.agentTick(time.Now().Add(7 * time.Minute))
	if len(h.slept) != 3 {
		t.Fatalf("a refused sleep is asked again at the next tick: %v", h.slept)
	}
}

func TestTheLifecycleWakesAKeepAliveAgentSession(t *testing.T) {
	m, _, sess := agentHomeManager(t)
	var h lifecycleHooks
	h.wire(m)
	sess.Wanted = store.SessionAsleep
	m.SyncRunnerPane(sess)
	m.agentTick(time.Now())
	if len(h.woken) != 0 {
		t.Fatalf("an asleep agent that is not keep-alive stays asleep: %v", h.woken)
	}
	d, _ := m.Agents.Get("radar")
	d.KeepAlive = true
	if _, err := m.Agents.Save(d); err != nil {
		t.Fatal(err)
	}
	m.agentTick(time.Now())
	if len(h.woken) != 1 || h.woken[0] != "radar" {
		t.Fatalf("keep-alive wakes it: %v", h.woken)
	}
}

func TestSleepAgentOnASessionGoesThroughTheDoor(t *testing.T) {
	m, _, sess := agentHomeManager(t)
	if err := m.SleepAgent("agent1"); !errors.Is(err, ErrNoAgentSleep) {
		t.Fatalf("with no supervisor wired: %v", err)
	}
	var h lifecycleHooks
	h.wire(m)
	if err := m.SleepAgent("agent1"); err != nil || len(h.slept) != 1 || h.slept[0] != sess.ID {
		t.Fatalf("a person's Sleep: %v %v", err, h.slept)
	}
	sess.Wanted = store.SessionAsleep
	m.SyncRunnerPane(sess)
	if err := m.SleepAgent("agent1"); err != nil || len(h.slept) != 1 {
		t.Fatalf("already asleep, nothing is asked: %v %v", err, h.slept)
	}
}

// A daemon restart must not read a working agent as idle from that moment:
// its clock comes back from the attention its session row kept.
func TestAnAgentSessionsClockSurvivesARestart(t *testing.T) {
	m, st, sess := agentHomeManager(t)
	m.ApplyAgentSignal("agent1", model.AttentionRunning, model.ReasonNone)
	if !m.AgentBusy("agent1") {
		t.Fatal("a running agent is busy")
	}
	after := New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	saved, _ := st.Load()
	after.byID[sess.WorkspaceID] = &entry{ws: saved[0]}
	if err := after.LoadRunnerPanes(); err != nil {
		t.Fatal(err)
	}
	if !after.AgentBusy("agent1") {
		t.Fatal("after a restart it still reads as working")
	}
	// A turn that had ended keeps its own time: idle since then, not since
	// the restart.
	m.ApplyAgentSignal("agent1", model.AttentionDone, model.ReasonFinished)
	again := New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	again.byID[sess.WorkspaceID] = &entry{ws: saved[0]}
	again.LoadRunnerPanes()
	act, ok := again.activity.get("agent1")
	if !ok || act.state != stateIdle || time.Since(act.since) > time.Minute {
		t.Fatalf("idle since its turn ended: %+v %v", act, ok)
	}
}

func TestAnAgentsFinishedTurnAlertsNobody(t *testing.T) {
	m, _, _ := agentHomeManager(t)
	m.runnerReminders.after = 20 * time.Millisecond
	m.ApplyAgentSignal("agent1", model.AttentionDone, model.ReasonFinished)
	time.Sleep(120 * time.Millisecond)
	if p := m.PaneByID("agent1"); p.Attention != model.AttentionDone {
		t.Fatalf("no reminder turns an agent's finished run into a call for a person: %q", p.Attention)
	}
}

func TestRetireAgentTmuxTakesAHomeOffTmux(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(context.Background(), &tmux.Server{Socket: "ccmux-test-none"}, st)
	ws := &model.Workspace{ID: "home", Name: "radar", RepoPath: "/w/agents/radar", TmuxSession: "ccmux-radar-home", Agent: "radar", Status: model.StatusCold}
	old := &model.Pane{ID: "old-pane", WorkspaceID: "home", CWD: "/w/agents/radar", Agent: "radar", AgentVersion: "1.0.0",
		StartupCommand: "node serve.mjs serve --port 4100 --agent radar", Attention: model.AttentionIdle}
	extra := &model.Pane{ID: "extra", WorkspaceID: "home", CWD: "/w/agents/radar", Attention: model.AttentionIdle}
	st.SaveWorkspace(ws)
	st.SavePane(old)
	st.SavePane(extra)
	ws.Panes = []*model.Pane{old, extra}
	m.byID["home"] = &entry{ws: ws}

	// A start typed into its pane this moment: it is running there.
	m.starting.mark("old-pane", time.Now())
	if _, err := m.TmuxAgentPane("home"); !errors.Is(err, ErrAgentRunning) {
		t.Fatalf("an agent running in its pane is not moved: %v", err)
	}
	if !m.TmuxAgentUp("home") {
		t.Fatal("and reads as running there")
	}
	m.starting.forget("old-pane")

	// Asked first: the pane's id, and nothing changed by asking.
	got, err := m.TmuxAgentPane("home")
	if err != nil || got != "old-pane" || len(m.Workspace("home").Panes) != 2 || len(storedPaneIDs(t, st)) != 2 {
		t.Fatalf("the agent pane's id, with the home untouched: %q %v", got, err)
	}
	// The session is made under that id while the tmux pane is still there:
	// from then on the id is the session's, wherever a pane is looked up.
	sess := store.RunnerSession{ID: "s1", Name: "radar", Cwd: "/w/agents/radar", Wanted: store.SessionRunning, PaneID: "old-pane", WorkspaceID: "home", Kind: store.SessionAgent}
	st.AddRunnerSession(sess)
	m.SyncRunnerPane(sess)
	m.ApplyAgentSignal("old-pane", model.AttentionRunning, model.ReasonNone)
	if p := m.PaneByID("old-pane"); p == nil || p.Runtime != model.RuntimeRunner || p.StartupCommand != "" {
		t.Fatalf("the same pane id, now the session's: %+v", p)
	}
	if info, ok := m.RunnerPane("old-pane"); !ok || info.SessionID != "s1" {
		t.Fatalf("the bus sees a session's pane there: %+v %v", info, ok)
	}
	// Then the home is taken off tmux, the extra pane included.
	if err := m.RetireAgentTmux("home"); err != nil {
		t.Fatal(err)
	}
	if home := m.Workspace("home"); len(home.Panes) != 0 || home.Status != model.StatusCold {
		t.Fatalf("no tmux pane is left to revive with the old launch line: %+v", home)
	}
	if left := storedPaneIDs(t, st); len(left) != 0 {
		t.Fatalf("their rows are gone too: %v", left)
	}
	if _, _, err := m.reviveIfClosed("home", nil); err == nil {
		t.Fatal("a revive of the home starts nothing in tmux")
	}
	// The session's pane, and its clock, are untouched by the tidy-up.
	if p := m.AgentPane("home", "radar"); p == nil || p.ID != "old-pane" || p.Runtime != model.RuntimeRunner || !m.AgentBusy("old-pane") {
		t.Fatalf("the session's pane stays, still working: %+v", p)
	}
	if err := m.RetireAgentTmux("home"); err != nil {
		t.Fatalf("a home already off tmux: %v", err)
	}
	if again, err := m.TmuxAgentPane("home"); err != nil || again != "" {
		t.Fatalf("it has no tmux agent pane any more: %q %v", again, err)
	}
	if _, err := m.TmuxAgentPane("nobody"); !errors.Is(err, ErrUnknownWorkspace) {
		t.Fatalf("an unknown home: %v", err)
	}
	if err := m.RetireAgentTmux("nobody"); !errors.Is(err, ErrUnknownWorkspace) {
		t.Fatalf("an unknown home: %v", err)
	}
}
