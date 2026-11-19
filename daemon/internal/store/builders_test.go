package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

func TestABuilderRowKeepsWhatItStartsWith(t *testing.T) {
	st := openSessions(t)
	th, err := st.AddThread(Thread{Group: "w1", Title: "Export", State: ThreadOpen, TaskID: 12, CreatedAt: 1, UpdatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	b := RunnerSession{ID: "b1", Name: "app-task-12", Cwd: "/wt/app/task-12", Model: "opus", Wanted: SessionRunning, PaneID: "p1", WorkspaceID: "ws1",
		ThreadID: th.ID, Role: RoleBuilder, Repo: "/r/app", Worktree: "/wt/app/task-12", Branch: "task/12-export", Effort: "medium", PermissionMode: "auto"}
	if err := st.AddRunnerSession(b); err != nil {
		t.Fatal(err)
	}
	got, err := st.RunnerSession("b1")
	if err != nil || got == nil || got.Effort != "medium" || got.PermissionMode != "auto" || got.Worktree != "/wt/app/task-12" || got.Branch != "task/12-export" || got.Role != RoleBuilder {
		t.Fatalf("a builder's row keeps its effort, mode, worktree and branch: %+v %v", got, err)
	}
	found, err := st.TaskThread(12)
	if err != nil || found == nil || found.ID != th.ID || found.TaskID != 12 || found.LeadSession != "" {
		t.Fatalf("a task finds its thread: %+v %v", found, err)
	}
	if none, err := st.TaskThread(13); err != nil || none != nil {
		t.Fatalf("a task that never started has no thread: %+v %v", none, err)
	}
	// A thread with no lead is the queue's: nothing that walks Threads meets it.
	if list, err := st.Threads(); err != nil || len(list) != 0 {
		t.Fatalf("a task's thread is not listed with the led ones: %+v %v", list, err)
	}
}

func TestATaskBuiltAgainFindsItsNewestThread(t *testing.T) {
	st := openSessions(t)
	if _, err := st.AddThread(Thread{State: ThreadEnded, TaskID: 5, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	second, err := st.AddThread(Thread{State: ThreadOpen, TaskID: 5, CreatedAt: 2, UpdatedAt: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := st.TaskThread(5); err != nil || got == nil || got.ID != second.ID {
		t.Fatalf("the newest thread of the task: %+v %v", got, err)
	}
}

func TestBuildersInAWorktreeAreReadFromTheRows(t *testing.T) {
	st := openSessions(t)
	add := func(id, role, worktree, wanted string, thread int64) {
		t.Helper()
		if err := st.AddRunnerSession(RunnerSession{ID: id, Cwd: worktree, Wanted: wanted, ThreadID: thread, Role: role, Repo: "/r/" + id, Worktree: worktree, CreatedAt: thread}); err != nil {
			t.Fatal(err)
		}
	}
	add("here", RoleBuilder, "/wt/task-1", SessionRunning, 1)
	add("asleep", RoleBuilder, "/wt/task-1", SessionAsleep, 2)
	add("ended", RoleBuilder, "/wt/task-1", SessionDone, 3)
	add("elsewhere", RoleBuilder, "/wt/task-2", SessionRunning, 4)
	add("helper", RoleHelper, "/wt/task-1", SessionRunning, 5)
	got, err := st.BuildersIn("/wt/task-1")
	if err != nil || len(got) != 2 || got[0].ID != "here" || got[1].ID != "asleep" {
		t.Fatalf("only the builders that have not ended, in that worktree, with or without a pane: %+v %v", got, err)
	}
}

func TestABuildersSignalIsKeptOnItsSessionRow(t *testing.T) {
	st := openSessions(t)
	for _, s := range []RunnerSession{
		{ID: "b1", Cwd: "/wt/1", Wanted: SessionRunning, PaneID: "p1", Role: RoleBuilder},
		{ID: "b2", Cwd: "/wt/2", Wanted: SessionRunning, PaneID: "p2", Role: RoleBuilder},
		{ID: "b3", Cwd: "/wt/3", Wanted: SessionDone, PaneID: "p3", Role: RoleBuilder},
	} {
		if err := st.AddRunnerSession(s); err != nil {
			t.Fatal(err)
		}
	}
	sig := TaskSignal{Attention: model.AttentionDone, Reason: model.ReasonFinished, At: 77}
	if err := st.UpdateRunnerTaskSignal("p1", sig); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRunnerTaskSignal("p3", sig); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRunnerTaskSignal("nope", sig); err == nil {
		t.Fatal("a signal for a pane with no session row is an error, not a silent drop")
	}
	got, err := st.RunnerTaskSignals()
	if err != nil || len(got) != 1 || got["p1"] != sig {
		t.Fatalf("the signal of every builder that has not ended, and of no other: %+v %v", got, err)
	}
}

func TestBuilderColumnsAreAddedToAnOlderRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddRunnerSession(RunnerSession{ID: "old", Cwd: "/r/app", Wanted: SessionRunning, PaneID: "p-old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddThread(Thread{State: ThreadOpen, Title: "old thread"}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`ALTER TABLE sessions DROP COLUMN effort`, `ALTER TABLE sessions DROP COLUMN permission_mode`,
		`ALTER TABLE sessions DROP COLUMN task_sig`, `ALTER TABLE sessions DROP COLUMN task_sig_reason`,
		`ALTER TABLE sessions DROP COLUMN task_sig_at`, `ALTER TABLE threads DROP COLUMN task_id`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatalf("an older registry opens: %v", err)
	}
	defer st.Close()
	got, err := st.RunnerSession("old")
	if err != nil || got == nil || got.Effort != "" || got.PermissionMode != "" {
		t.Fatalf("an older session starts as it always did: %+v %v", got, err)
	}
	if th, err := st.Thread(1); err != nil || th == nil || th.TaskID != 0 || th.Title != "old thread" {
		t.Fatalf("an older thread is no task's: %+v %v", th, err)
	}
	if sigs, err := st.RunnerTaskSignals(); err != nil || len(sigs) != 0 {
		t.Fatalf("an older session has no signal: %+v %v", sigs, err)
	}
}
