package manager

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/listeners"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// builderManager is runnerManager with a task's builder beside the plain
// session: a runner session with the role builder, in its worktree. The
// repo's tmux session is closed (no controller), as it is most nights.
func builderManager(t *testing.T) (*Manager, *store.SQLite, store.RunnerSession) {
	t.Helper()
	m, st, plain := runnerManager(t)
	m.SyncRunnerPane(plain)
	b := store.RunnerSession{ID: "0b0b0b0b-aaaa-bbbb-cccc-000000000002", Name: "app-task-12", Cwd: "/wt/app/task-12", Wanted: store.SessionRunning,
		PaneID: "build1", WorkspaceID: "ws", ThreadID: 4, Role: store.RoleBuilder, Repo: "/r/app", Worktree: "/wt/app/task-12", Branch: "task/12-x", CreatedAt: 6}
	if err := st.AddRunnerSession(b); err != nil {
		t.Fatal(err)
	}
	m.SyncRunnerPane(b)
	return m, st, b
}

// sig is the builder's signal as the queue's watchdog reads it.
func sig(m *Manager) (model.Attention, model.AttentionReason) {
	s := m.taskSigs.get("build1")
	return s.Attention, s.Reason
}

func TestABuildersPaneCarriesTheTaskWorkersStamp(t *testing.T) {
	m, _, _ := builderManager(t)
	m.mu.RLock()
	_, b := m.findPaneLocked("build1")
	_, plain := m.findPaneLocked("run1")
	m.mu.RUnlock()
	if b == nil || b.CreatedBy != taskRunner || b.Runtime != model.RuntimeRunner {
		t.Fatalf("a builder's pane is a task worker's and a runner's: %+v", b)
	}
	if plain == nil || plain.CreatedBy != "runner" {
		t.Fatalf("a person's session is no task worker: %+v", plain)
	}
	if info, ok := m.RunnerPane("build1"); !ok || info.Name != "app-task-12" {
		t.Fatalf("its bus name is the task's name for its repo: %+v %v", info, ok)
	}
}

// Gap 2: a tmux worker is alive only while its repo's tmux session is open.
// A builder has no tmux at all: it is there until its session has ended,
// and its Claude counts as gone once the session is asleep.
func TestABuilderIsAliveWithoutATmuxSession(t *testing.T) {
	m, _, b := builderManager(t)
	st, ok := m.TaskPane("build1")
	if !ok || st.AtShell {
		t.Fatalf("a running builder in a closed tmux session is alive: %+v %v", st, ok)
	}
	b.Wanted = store.SessionAsleep // the supervisor put it to sleep after too many stops
	m.SyncRunnerPane(b)
	if st, ok := m.TaskPane("build1"); !ok || !st.AtShell {
		t.Fatalf("an asleep builder is one whose Claude exited: %+v %v", st, ok)
	}
	b.Wanted = store.SessionDone
	m.SyncRunnerPane(b)
	if _, ok := m.TaskPane("build1"); ok {
		t.Fatal("an ended builder's session is closed")
	}
	if _, ok := m.TaskPane("tmux1"); ok {
		t.Fatal("a tmux pane in a closed session is still not alive: that rule is unchanged")
	}
}

// Gap 3, single signals: the table in builderSignal.
func TestBuilderSignalReadsARunnersWordsForTheWatchdog(t *testing.T) {
	prompted := TaskSignal{Attention: model.AttentionIdle, At: 5}
	done := TaskSignal{Attention: model.AttentionDone, Reason: model.ReasonFinished, At: 5}
	asking := TaskSignal{Attention: model.AttentionNeedsInput, Reason: model.ReasonQuestion, At: 5}
	for _, tc := range []struct {
		name   string
		last   TaskSignal
		att    model.Attention
		reason model.AttentionReason
		want   TaskSignal
		keep   bool
	}{
		{"busy after nothing is at work", TaskSignal{}, model.AttentionRunning, model.ReasonNone, TaskSignal{Attention: model.AttentionRunning}, true},
		{"busy after a turn ended is at work", done, model.AttentionRunning, model.ReasonNone, TaskSignal{Attention: model.AttentionRunning}, true},
		{"busy after a card was answered is at work", asking, model.AttentionRunning, model.ReasonNone, TaskSignal{Attention: model.AttentionRunning}, true},
		{"busy never replaces a prompt's mark", prompted, model.AttentionRunning, model.ReasonNone, prompted, false},
		{"stopped is a turn that ended, not a person typing", TaskSignal{Attention: model.AttentionRunning, At: 5}, model.AttentionIdle, model.ReasonNone, TaskSignal{Attention: model.AttentionDone, Reason: model.ReasonFinished}, true},
		{"idle is a turn that ended", prompted, model.AttentionDone, model.ReasonFinished, TaskSignal{Attention: model.AttentionDone, Reason: model.ReasonFinished}, true},
		{"a card is a question", done, model.AttentionNeedsInput, model.ReasonQuestion, TaskSignal{Attention: model.AttentionNeedsInput, Reason: model.ReasonQuestion}, true},
	} {
		got, keep := builderSignal(tc.last, tc.att, tc.reason)
		if got != tc.want || keep != tc.keep {
			t.Errorf("%s: got %+v keep=%v, want %+v keep=%v", tc.name, got, keep, tc.want, tc.keep)
		}
	}
}

// Gap 3, whole sequences, through the routes a runner and the daemon use.
func TestABuildersSignalsInOrder(t *testing.T) {
	m, _, _ := builderManager(t)
	busy := func() { m.ApplyAgentSignal("build1", model.AttentionRunning, model.ReasonNone) }
	idle := func() { m.ApplyAgentSignal("build1", model.AttentionDone, model.ReasonFinished) }

	// Its first prompt is the queue's own, unmarked: busy is its first sign.
	busy()
	if att, _ := sig(m); att != model.AttentionRunning {
		t.Fatalf("a builder at work says so: %q", att)
	}
	idle()
	if att, reason := sig(m); att != model.AttentionDone || reason != model.ReasonFinished {
		t.Fatalf("its turn ended: %q %q", att, reason)
	}
	// A person types into it (the prompt route marks it), and its runner
	// goes busy a second later: the mark must still stand when the
	// watchdog next looks.
	m.NoteTaskPrompt("build1")
	marked := m.taskSigs.get("build1")
	busy()
	if got := m.taskSigs.get("build1"); got != marked || got.Attention != model.AttentionIdle || got.At == 0 {
		t.Fatalf("busy replaced a person's prompt: %+v, want %+v", got, marked)
	}
	idle()
	// A teammate's bus message starts a turn with no mark: at work.
	busy()
	if att, _ := sig(m); att != model.AttentionRunning {
		t.Fatalf("a turn a bus message started reads as at work, not as finished long ago: %q", att)
	}
	// Its runner is stopped mid-turn (a restart): not "a person typed".
	m.ApplyAgentSignal("build1", model.AttentionIdle, model.ReasonNone)
	if att, reason := sig(m); att != model.AttentionDone || reason != model.ReasonFinished {
		t.Fatalf("stopped reads as a turn that ended: %q %q", att, reason)
	}
	// A card, then its answer.
	m.ApplyAgentSignal("build1", model.AttentionNeedsInput, model.ReasonQuestion)
	if att, reason := sig(m); att != model.AttentionNeedsInput || reason != model.ReasonQuestion {
		t.Fatalf("a card is a question for a person: %q %q", att, reason)
	}
	busy() // "replied"
	if att, _ := sig(m); att != model.AttentionRunning {
		t.Fatalf("an answered card puts it back to work: %q", att)
	}
	// A person's own session keeps no task signal, whatever it says.
	m.ApplyAgentSignal("run1", model.AttentionDone, model.ReasonFinished)
	m.NoteTaskPrompt("run1")
	if got := m.taskSigs.get("run1"); got.At != 0 {
		t.Fatalf("a person's session was recorded as a task worker: %+v", got)
	}
}

// The idle reminder is the daemon's own clock for a runner (runnerremind.go)
// and is what a merge waits for (taskq.Settled).
func TestABuildersEndedTurnIsReminded(t *testing.T) {
	m, _, _ := builderManager(t)
	m.runnerReminders.after = 30 * time.Millisecond
	m.ApplyAgentSignal("build1", model.AttentionDone, model.ReasonFinished)
	var att model.Attention
	var reason model.AttentionReason
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if att, reason = sig(m); att == model.AttentionNeedsInput {
			break
		}
	}
	if att != model.AttentionNeedsInput || reason != model.ReasonFinished {
		t.Fatalf("a builder's ended turn gets its reminder: %q %q", att, reason)
	}
}

// A runner pane has no row in panes, so the signal is kept on the session.
func TestABuildersSignalSurvivesARestart(t *testing.T) {
	m, st, _ := builderManager(t)
	m.ApplyAgentSignal("build1", model.AttentionDone, model.ReasonFinished)
	heard := m.taskSigs.get("build1")
	if heard.At == 0 {
		t.Fatal("no signal kept")
	}
	after := New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	after.loadTaskSignals()
	if got := after.taskSigs.get("build1"); got != heard {
		t.Fatalf("after a restart: %+v, want %+v", got, heard)
	}
}

// Gap 1, clearing a worktree: every builder that works there is ended,
// from the rows, and a stop that fails is said.
func TestClearTaskFolderEndsTheBuildersThatWorkThere(t *testing.T) {
	m, st, b := builderManager(t)
	// A builder whose workspace is gone has no pane and still holds its worktree.
	lost := store.RunnerSession{ID: "lost", Cwd: "/wt/app/task-12", Wanted: store.SessionAsleep, PaneID: "p-lost", WorkspaceID: "gone",
		ThreadID: 9, Role: store.RoleBuilder, Repo: "/r/other", Worktree: "/wt/app/task-12", CreatedAt: 7}
	elsewhere := store.RunnerSession{ID: "elsewhere", Cwd: "/wt/app/task-13", Wanted: store.SessionRunning, PaneID: "p-else", WorkspaceID: "ws",
		ThreadID: 10, Role: store.RoleBuilder, Repo: "/r/app", Worktree: "/wt/app/task-13"}
	for _, s := range []store.RunnerSession{lost, elsewhere} {
		if err := st.AddRunnerSession(s); err != nil {
			t.Fatal(err)
		}
		m.SyncRunnerPane(s)
	}
	var ended []string
	m.EndRunnerSession = func(id string) error {
		ended = append(ended, id)
		return st.SetRunnerSessionWanted(id, store.SessionDone, 9)
	}
	if err := m.ClearTaskFolder("/wt/app/task-12"); err != nil {
		t.Fatal(err)
	}
	if len(ended) != 2 || ended[0] != b.ID || ended[1] != "lost" {
		t.Fatalf("ended %v, want the builder there and the one with no pane", ended)
	}
	if err := m.ClearTaskFolder("/wt/app/task-12"); err != nil || len(ended) != 2 {
		t.Fatalf("clearing an empty folder ends nothing: %v %v", err, ended)
	}
	m.EndRunnerSession = func(string) error { return errors.New("unit would not stop") }
	if err := m.ClearTaskFolder("/wt/app/task-13"); err == nil || !strings.Contains(err.Error(), "unit would not stop") {
		t.Fatalf("a builder that does not stop must stop the caller: %v", err)
	}
	m.EndRunnerSession = nil
	if err := m.ClearTaskFolder("/wt/app/task-13"); err == nil {
		t.Fatal("no supervisor to end it with is an error, not an empty folder")
	}
}

// Gap 1, dev servers: a port a builder's own server holds is a task
// worker's, never its repo session's.
func TestABuildersDevServerIsNotItsSessions(t *testing.T) {
	m, _, _ := builderManager(t)
	m.applyListeners([]listeners.Listener{{Port: 22070, Process: "next-server", PaneID: "build1"}, {Port: 4000, Process: "next-server", PaneID: "run1"}})
	ws := m.Workspace("ws")
	if len(ws.Listeners) != 1 || ws.Listeners[0].Port != 4000 {
		t.Fatalf("only the person's session's port is the workspace's: %+v", ws.Listeners)
	}
	m.mu.RLock()
	held := m.taskPorts[22070]
	m.mu.RUnlock()
	if !held {
		t.Fatal("the builder's port is held by a task worker")
	}
}
