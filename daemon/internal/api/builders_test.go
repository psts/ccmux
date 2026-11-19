package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/loadout"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// builderServer is sessionsServer with a task's builder in the repo's
// workspace: session s1, pane run1 (the pair the fake rows answer with).
func builderServer(t *testing.T) (*Server, *store.SQLite, string) {
	t.Helper()
	repo := t.TempDir()
	s, _, st := sessionsServer(t, repo)
	b := store.RunnerSession{ID: "s1", Name: "app-task-3", Cwd: "/wt/app/task-3", Wanted: store.SessionRunning, PaneID: "run1", WorkspaceID: "ws-app",
		ThreadID: 1, Role: store.RoleBuilder, Repo: repo, Worktree: "/wt/app/task-3", Branch: "task/3-x"}
	if err := st.AddRunnerSession(b); err != nil {
		t.Fatal(err)
	}
	s.mgr.SyncRunnerPane(b)
	return s, st, repo
}

// Gap 3: a builder's own prompt hook is not heard, so the route a person's
// prompt comes in on is what tells the watchdog "a person typed".
func TestAPersonsPromptToABuilderIsMarkedForTheWatchdog(t *testing.T) {
	s, _, _ := builderServer(t)
	s.mgr.ApplyAgentSignal("run1", model.AttentionDone, model.ReasonFinished)
	if w := callSessions(t, s, "POST", "/v1/sessions/s1/prompt", `{"text":"go on, use the second option","sender":"Pat"}`); w.Code != http.StatusAccepted {
		t.Fatalf("prompt = %d %s", w.Code, w.Body)
	}
	st, ok := s.mgr.TaskPane("run1")
	if !ok || st.Signal.Attention != model.AttentionIdle || st.Signal.At == 0 {
		t.Fatalf("the builder's signal after a person's prompt = %+v %v, want prompted", st.Signal, ok)
	}
	// Its runner goes busy a second later: the mark stands for the turn.
	s.mgr.ApplyAgentSignal("run1", model.AttentionRunning, model.ReasonNone)
	if st, _ := s.mgr.TaskPane("run1"); st.Signal.Attention != model.AttentionIdle {
		t.Fatalf("busy replaced the person's prompt: %+v", st.Signal)
	}
	// A prompt that was refused marks nothing.
	if w := callSessions(t, s, "POST", "/v1/sessions/nope/prompt", `{"text":"x"}`); w.Code == http.StatusAccepted {
		t.Fatalf("a prompt for no session was accepted: %d", w.Code)
	}
}

// Gap 9: a part built by a session says which, so a lens opens its
// conversation; a worker in a tmux pane says none and opens as a terminal.
// Send-back is offered for both while the worker is there.
func TestAPartSaysWhichSessionBuildsIt(t *testing.T) {
	s, _, repo := builderServer(t)
	failed := store.Task{ID: 3, Status: taskq.MergeFailed}
	v := s.partViewOf(failed, store.Part{ID: 1, TaskID: 3, RepoPath: repo, Status: taskq.Ready, PaneID: "run1"})
	if v.SessionID != "s1" || v.WorkspaceID != "ws-app" || !v.SendBackable {
		t.Fatalf("a builder's part = %+v", v)
	}
	old := s.partViewOf(failed, store.Part{ID: 2, TaskID: 3, RepoPath: repo, Status: taskq.Ready, PaneID: "tmux1"})
	if old.SessionID != "" || old.WorkspaceID != "ws-app" || !old.SendBackable {
		t.Fatalf("a tmux worker's part = %+v", old)
	}
	gone := s.partViewOf(failed, store.Part{ID: 3, TaskID: 3, RepoPath: repo, Status: taskq.Ready, PaneID: "ended-long-ago"})
	if gone.SessionID != "" || gone.WorkspaceID != "" || gone.SendBackable {
		t.Fatalf("a part whose worker is gone = %+v", gone)
	}
}

// The queue starts builders through the threads service, and where there
// is none (no systemd) it starts nothing and says why.
func TestTheQueueStartsBuildersThroughTheThreadsService(t *testing.T) {
	s, st, _ := builderServer(t)
	s.SetTasks(st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartTaskRunner(ctx, time.Hour, 1, t.TempDir(), nil, "no jobs here")
	if s.taskRunner == nil || !strings.Contains(s.taskRunner.BuildersOff, "no Claude sessions") {
		t.Fatalf("with no threads service the runner starts no builders: %+v", s.taskRunner)
	}
	if _, err := s.taskRunner.Host.StartBuilder(taskq.Builder{Name: "app-task-3"}); err == nil {
		t.Fatal("a builder was started with nothing to start it")
	}
	f := &fakeThreads{}
	s.SetThreads(f)
	s.StartTaskRunner(ctx, time.Hour, 1, t.TempDir(), nil, "no jobs here")
	if s.taskRunner.BuildersOff != "" {
		t.Fatalf("with a threads service builders start: %q", s.taskRunner.BuildersOff)
	}
	pane, err := s.taskRunner.Host.StartBuilder(taskq.Builder{Name: "app-task-3", TaskID: 3})
	if err != nil || pane != "pane-app-task-3" || len(f.built) != 1 || f.built[0].TaskID != 3 {
		t.Fatalf("the builder went to the threads service: %q %v %+v", pane, err, f.built)
	}
}

// Gap 10: a builder has no loadout on purpose, so "no loadout" must not
// read as "started before loadouts, restart it": the page would offer a
// restart to every builder.
func TestABuilderWithNoLoadoutIsNotOutOfDate(t *testing.T) {
	snap := &loadout.Snapshot{}
	old := store.RunnerSession{ID: "old", Wanted: store.SessionRunning}
	if _, stale := loadoutOf(snap, old); !stale {
		t.Fatal("a person's running session with no loadout is still out of date: that rule is unchanged")
	}
	b := store.RunnerSession{ID: "b", Wanted: store.SessionRunning, Role: store.RoleBuilder}
	if rec, stale := loadoutOf(snap, b); rec != nil || stale {
		t.Fatalf("a builder's loadout = %+v stale=%v, want none and never out of date", rec, stale)
	}
}
