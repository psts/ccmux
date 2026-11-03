package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// itestServer is a Server over a live tmux manager and a task store.
func itestServer(t *testing.T, socket string) (*Server, *manager.Manager, *store.SQLite) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	tsrv := &tmux.Server{Socket: socket, ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr := manager.New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	s := &Server{mgr: mgr}
	s.SetTasks(st)
	return s, mgr, st
}

// A worker pulls only repos from its own ccmux window into its feature: a
// repo session in another window is refused and nothing is added, while one
// in the same window joins.
func TestTasks_WorkerAddsPartsOnlyInItsWindow(t *testing.T) {
	s, mgr, st := itestServer(t, "ccmux-api-tasks-window-itest")
	repoIn := func(window, name string) string {
		dir := filepath.Join(t.TempDir(), name)
		os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
		ws, err := mgr.CreateWorkspace(name, dir, dir, "", "tester", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := mgr.SeedWindowMembership(ws.ID, window); err != nil {
			t.Fatal(err)
		}
		return ws.ID
	}
	appWS := repoIn("ChartLabs", "app")
	repoIn("ChartLabs", "backend")
	repoIn("Personal", "blog")
	app := mgr.Workspace(appWS)
	worker, err := mgr.StartTaskPane(appWS, app.RepoPath, "true")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddTaskWithParts(store.Task{Title: "t", Status: taskq.Running},
		[]store.Part{{RepoPath: app.RepoPath, Plan: "p", Status: taskq.Running}}); err != nil {
		t.Fatal(err)
	}
	parts, _ := st.Parts()
	parts[0].PaneID = worker
	st.SavePart(parts[0])

	rec := do(t, s, "POST", "/v1/panes/"+worker+"/tasks", `{"action":"add_part","repo":"blog","plan":"a setting"}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "own ccmux window") {
		t.Fatalf("a repo in another window = %d %s", rec.Code, rec.Body)
	}
	if got, _ := st.Parts(); len(got) != 1 {
		t.Fatalf("a refused part was added: %d parts", len(got))
	}
	rec = do(t, s, "POST", "/v1/panes/"+worker+"/tasks", `{"action":"add_part","repo":"backend","plan":"the endpoint"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "backend-task-1") {
		t.Fatalf("a repo in the same window = %d %s", rec.Code, rec.Body)
	}
	if got, _ := st.Parts(); len(got) != 2 {
		t.Fatalf("parts after a same-window add = %d, want 2", len(got))
	}
}

// Marking a built feature merged by hand closes its finished workers'
// Claudes, which stay open after their report: once the task is removed,
// nothing would ever find them again. Their worktrees stay. A worker of
// another task in the same session is left alone.
func TestTasks_MarkMergedClosesIdleWorkers(t *testing.T) {
	s, mgr, st := itestServer(t, "ccmux-api-tasks-itest")
	repo, wt, other := t.TempDir(), t.TempDir(), t.TempDir()
	ws, err := mgr.CreateWorkspace("app", repo, repo, "", "tester", "")
	if err != nil {
		t.Fatal(err)
	}
	idle, _ := mgr.StartTaskPane(ws.ID, wt, "true")
	elsewhere, _ := mgr.StartTaskPane(ws.ID, other, "true")
	_, err = st.AddTaskWithParts(store.Task{Title: "t", Status: taskq.ReadyToMerge},
		[]store.Part{{RepoPath: repo, Plan: "p", Status: taskq.Ready}})
	if err != nil {
		t.Fatal(err)
	}
	parts, _ := st.Parts()
	parts[0].PaneID, parts[0].Worktree = idle, wt
	st.SavePart(parts[0])

	if rec := do(t, s, "PATCH", "/v1/tasks/1", `{"status":"done"}`); rec.Code != 200 {
		t.Fatalf("mark merged = %d %s", rec.Code, rec.Body)
	}
	if mgr.WorkspaceForPane(idle) != "" {
		t.Fatal("the idle worker of a task marked merged is still open")
	}
	if mgr.WorkspaceForPane(elsewhere) == "" {
		t.Fatal("another task's worker was closed")
	}
	if got, _ := st.Parts(); got[0].Status != taskq.Ready {
		t.Fatalf("a built part changed status: %s", got[0].Status)
	}
}

// The session a feature was planned in files it whole: its own part and the
// parts of other repos in its window, in one call, waiting at once. A repo
// in another window is refused and nothing is filed.
func TestTasks_LeadFilesEveryPart(t *testing.T) {
	s, mgr, st := itestServer(t, "ccmux-api-tasks-lead-itest")
	repoIn := func(window, name string) string {
		dir := filepath.Join(t.TempDir(), name)
		os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
		ws, err := mgr.CreateWorkspace(name, dir, dir, "", "tester", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := mgr.SeedWindowMembership(ws.ID, window); err != nil {
			t.Fatal(err)
		}
		return ws.ID
	}
	app := mgr.Workspace(repoIn("ChartLabs", "app"))
	repoIn("ChartLabs", "backend")
	repoIn("Personal", "blog")
	lead := app.Panes[0].ID
	plan := "a plan long enough to say what to build"

	rec := do(t, s, "POST", "/v1/panes/"+lead+"/tasks",
		`{"action":"file","title":"Export","plan":"`+plan+`","parts":[{"repo":"backend","plan":"`+plan+`"},{"repo":"blog","plan":"`+plan+`"}]}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "blog") {
		t.Fatalf("a part in another window's repo = %d %s", rec.Code, rec.Body)
	}
	if got, _ := st.Tasks(); len(got) != 0 {
		t.Fatalf("a refused filing left %d tasks", len(got))
	}

	rec = do(t, s, "POST", "/v1/panes/"+lead+"/tasks",
		`{"action":"file","title":"Export","plan":"`+plan+`","parts":[{"repo":"backend","plan":"the endpoint, `+plan+`"}]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "parts for app, backend") {
		t.Fatalf("filing a two-repo feature = %d %s", rec.Code, rec.Body)
	}
	tasks, _ := st.Tasks()
	parts, _ := st.Parts()
	if len(tasks) != 1 || tasks[0].Status != taskq.Waiting || tasks[0].WindowID == "" || len(parts) != 2 ||
		filepath.Base(parts[1].RepoPath) != "backend" || !strings.HasPrefix(parts[1].Plan, "the endpoint") || parts[1].FiledBy != "app" {
		t.Fatalf("filed = %+v, parts %+v", tasks, parts)
	}
}
