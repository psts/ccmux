package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
	"ccmux.dev/ccmuxd/internal/tmux"
	"ccmux.dev/ccmuxd/internal/todos"
)

// todoGitRepo makes a repo with one commit, named name, and returns its
// folder as git names it.
func todoGitRepo(t *testing.T, name string) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	todoGit(t, dir, "init", "-q", "-b", "main")
	todoGit(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

func todoGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// The tool, as its three kinds of caller use it: a repo's own session, a
// task builder working in a worktree of the repo, and a session naming
// another repo in its window. What a builder finds is on the repo's list.
func TestTodoTool_ASessionAddsListsAndTicksOffOnItsReposList(t *testing.T) {
	s, mgr, st := itestServer(t, "ccmux-api-todo-pane-itest")
	s.SetTodos(st)
	app, backend, blog := todoGitRepo(t, "app"), todoGitRepo(t, "backend"), todoGitRepo(t, "blog")
	worktree := filepath.Join(filepath.Dir(app), "wt-task-7")
	todoGit(t, app, "worktree", "add", "-q", "-b", "task/7", worktree)
	appWS := openRepo(t, mgr, "ChartLabs", app).ID
	openRepo(t, mgr, "ChartLabs", backend)
	openRepo(t, mgr, "Personal", blog)
	own := mgr.Workspace(appWS).Panes[0].ID
	builder, err := mgr.StartTaskPane(appWS, worktree, "true")
	if err != nil {
		t.Fatal(err)
	}
	tool := func(pane, body string, want int) todoAnswer {
		t.Helper()
		return todoCall(t, s, "POST", "/v1/panes/"+pane+"/todo", body, want)
	}
	message := func(pane, body string) string {
		t.Helper()
		rec := do(t, s, "POST", "/v1/panes/"+pane+"/todo", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", body, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}

	// A builder in a worktree: its item is on the repo's own list, signed.
	if got := message(builder, `{"action":"add","text":"Fix the flaky test in store_test.go","urgency":"high","by":"app-task-7"}`); !strings.Contains(got, `Added to-do #1 [high] to app's list.`) {
		t.Fatalf("a builder's add = %s", got)
	}
	item, _ := st.Todo(1)
	if item == nil || item.Repo != app || item.AddedBy != "app-task-7" {
		t.Fatalf("the builder's item = %+v, want it on %s by app-task-7", item, app)
	}
	// The repo's own session sees it, and adding it again adds nothing.
	if got := message(own, `{"action":"add","text":"fix the flaky test in store_test.go."}`); !strings.Contains(got, "Already on app's to-do list as #1 [high, open]; not added again.") {
		t.Fatalf("the same thing again = %s", got)
	}
	// A caller that names nobody is its session.
	tool(own, `{"action":"add","text":"Update the readme","urgency":"low"}`, http.StatusOK)
	if second, _ := st.Todo(2); second == nil || second.AddedBy != "app" || second.Urgency != "low" {
		t.Fatalf("an unsigned add = %+v, want it by the session, app", second)
	}
	list := tool(own, `{"action":"list"}`, http.StatusOK)
	if len(list.Todos) != 2 || list.Todos[0].ID != 1 || list.Todos[1].ID != 2 {
		t.Fatalf("the list = %+v, want #1 (high) then #2 (low)", list.Todos)
	}

	// Another repo in the window, by its folder name; not one in another window.
	if got := message(own, `{"action":"add","repo":"backend","text":"The endpoint returns 500 on an empty body","urgency":"medium"}`); !strings.Contains(got, "to backend's list") {
		t.Fatalf("an add to a window peer = %s", got)
	}
	if third, _ := st.Todo(3); third == nil || third.Repo != backend {
		t.Fatalf("the peer's item = %+v, want it on %s", third, backend)
	}
	if got := tool(own, `{"action":"add","repo":"blog","text":"x"}`, http.StatusNotFound); !strings.Contains(got.Error, "no repo session named blog") {
		t.Fatalf("a repo in another window: %q", got.Error)
	}
	if got := tool(own, `{"action":"list","repo":"backend"}`, http.StatusOK); len(got.Todos) != 1 || got.Todos[0].ID != 3 {
		t.Fatalf("the peer's list = %+v", got.Todos)
	}

	// Done: only an item of the caller's list, with who fixed it and how.
	if got := tool(own, `{"action":"done","id":3}`, http.StatusNotFound); !strings.Contains(got.Error, "no to-do #3 on app's list") {
		t.Fatalf("ticking off another repo's item: %q", got.Error)
	}
	if third, _ := st.Todo(3); third.Status != todos.Open {
		t.Fatalf("another repo's item was ticked off: %+v", third)
	}
	if got := message(builder, `{"action":"done","id":1,"note":"fixed in 1a2b3c","by":"app-task-7"}`); !strings.Contains(got, "Marked to-do #1 done.") {
		t.Fatalf("done = %s", got)
	}
	if first, _ := st.Todo(1); first.Status != todos.Done || first.DoneBy != "app-task-7" || first.Note != "fixed in 1a2b3c" {
		t.Fatalf("ticked off = %+v", first)
	}
	if got := message(own, `{"action":"done","id":1}`); !strings.Contains(got, "was done already") {
		t.Fatalf("done twice = %s", got)
	}
	if first, _ := st.Todo(1); first.DoneBy != "app-task-7" {
		t.Fatalf("a second done rewrote who finished it: %+v", first)
	}
	if got := tool(own, `{"action":"list"}`, http.StatusOK); len(got.Todos) != 1 || got.Todos[0].ID != 2 {
		t.Fatalf("the list after a done = %+v, want only #2", got.Todos)
	}

	// What the tool refuses.
	tool(own, `{"action":"done","id":999}`, http.StatusNotFound)
	tool(own, `{"action":"add","text":"   "}`, http.StatusBadRequest)
	tool(own, `{"action":"sweep"}`, http.StatusBadRequest)
	tool("nope", `{"action":"list"}`, http.StatusNotFound)
}

// openRepo opens a repo session for the folder repo in window.
func openRepo(t *testing.T, mgr *manager.Manager, window, repo string) *model.Workspace {
	t.Helper()
	ws, err := mgr.CreateWorkspace(filepath.Base(repo), repo, repo, "", "tester", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.SeedWindowMembership(ws.ID, window); err != nil {
		t.Fatal(err)
	}
	return ws
}

// A repo can be named by any folder of it, for a session that is not in
// its window; the caller is then still signed as itself.
func TestTodoTool_ARepoIsNamedByItsFolderNameOrAFolderOfIt(t *testing.T) {
	s, mgr, st := itestServer(t, "ccmux-api-todo-named-itest")
	s.SetTodos(st)
	app, blog := todoGitRepo(t, "app"), todoGitRepo(t, "blog")
	openRepo(t, mgr, "ChartLabs", app)
	pane := openRepo(t, mgr, "Personal", blog).Panes[0].ID
	sub := filepath.Join(app, "docs")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	who, status, err := s.todoCallerOfPane(pane, paneTodoReq{Repo: sub, By: "blog-task-3"})
	if err != nil || who.repo != app || who.name != "app" || who.by != "blog-task-3" {
		t.Fatalf("naming a repo by a folder of it = %+v, %d, %v", who, status, err)
	}
	if who, _, err := s.todoCallerOfPane(pane, paneTodoReq{}); err != nil || who.repo != blog || who.by != "blog" {
		t.Fatalf("naming none = %+v, %v; want the session's own repo, signed as the session", who, err)
	}
	if _, status, err := s.todoCallerOfPane(pane, paneTodoReq{Repo: "app"}); err == nil || status != http.StatusNotFound {
		t.Fatalf("a folder name from another window = %d, %v; want it not found", status, err)
	}
}

// A role agent's folder is no repo, and no row on the page shows a list
// for it: it adds only by naming the repo the item is about.
func TestTodoTool_ARoleAgentHasNoListOfItsOwn(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	tsrv := &tmux.Server{Socket: "ccmux-api-todo-agent-itest", ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	folder := t.TempDir()
	if err := st.SaveWorkspace(&model.Workspace{ID: "ws-radar", Name: "radar", RepoPath: folder, Agent: "radar", TmuxSession: "ccmux-radar-itest"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePane(&model.Pane{ID: "radar-pane", WorkspaceID: "ws-radar", CWD: folder}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr := manager.New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	s := &Server{mgr: mgr}
	s.SetTodos(st)

	refused := todoCall(t, s, "POST", "/v1/panes/radar-pane/todo", `{"action":"add","text":"The changelog page 404s"}`, http.StatusBadRequest)
	if !strings.Contains(refused.Error, "name the repo") {
		t.Fatalf("an agent adding to no repo: %q", refused.Error)
	}
	if all, _ := st.Todos(""); len(all) != 0 {
		t.Fatalf("an item was filed on the agent's own folder: %+v", all)
	}
	app := todoGitRepo(t, "app")
	todoCall(t, s, "POST", "/v1/panes/radar-pane/todo", fmt.Sprintf(`{"action":"add","repo":%q,"text":"The changelog page 404s"}`, app), http.StatusOK)
	if all, _ := st.Todos(app); len(all) != 1 || all[0].AddedBy != "radar" {
		t.Fatalf("naming the repo: %+v, want one item on %s by radar", all, app)
	}
	// The page's map of sessions to lists has no entry for the agent.
	if repos := todoCall(t, s, "GET", "/v1/todos", "", http.StatusOK).Repos; len(repos) != 0 {
		t.Fatalf("repos = %+v, want none: an agent's session has no list", repos)
	}
}

// todoFiler is a filer whose repo is a folder that is not on this disk:
// its list's key is the path itself.
var todoFiler = taskFiler{repo: "/p/app", session: "app", window: "w1"}

func fileFor(t *testing.T, s *Server, todo int64) int64 {
	t.Helper()
	msg := call(t, s, todoFiler, paneTaskReq{Action: "file", Title: "Fix the flaky test", Plan: "p", Todo: todo})
	if !strings.Contains(msg, fmt.Sprintf("To-do #%d now follows it", todo)) {
		t.Fatalf("the filing's message = %q, want it to say the item follows the task", msg)
	}
	all, _ := s.tasks.Tasks()
	return all[len(all)-1].ID
}

func addItem(t *testing.T, s *Server, repo, text string) int64 {
	t.Helper()
	return todoCall(t, s, "POST", "/v1/todos", fmt.Sprintf(`{"repo":%q,"text":%q}`, repo, text), http.StatusCreated).Todo.ID
}

func itemOf(t *testing.T, st *store.SQLite, id int64) store.Todo {
	t.Helper()
	got, err := st.Todo(id)
	if err != nil || got == nil {
		t.Fatalf("to-do #%d: %v, %v", id, got, err)
	}
	return *got
}

// The tasks tool files a task for a to-do item, and the item follows that
// task from then on, whoever moves it: a person on the page, or the runner.
func TestTodos_AnItemFiledAsATaskFollowsIt(t *testing.T) {
	s, st := todoServer(t)
	merged, cancelled, byRunner, plain := addItem(t, s, "/p/app", "Fix the flaky test"), addItem(t, s, "/p/app", "Update the readme"),
		addItem(t, s, "/p/app", "Drop the dead flag"), addItem(t, s, "/p/app", "Rename the helper")

	taskA, taskB, taskC := fileFor(t, s, merged), fileFor(t, s, cancelled), fileFor(t, s, byRunner)
	for id, task := range map[int64]int64{merged: taskA, cancelled: taskB, byRunner: taskC} {
		if got := itemOf(t, st, id); got.Status != todos.Tasked || got.TaskID != task {
			t.Fatalf("filed: #%d = %+v, want tasked under task #%d", id, got, task)
		}
	}

	// A person marks a built task merged on the page (the routes' save).
	a, _ := st.Task(taskA)
	a.Status = taskq.Done
	if err := s.saveTask(*a); err != nil {
		t.Fatal(err)
	}
	if got := itemOf(t, st, merged); got.Status != todos.Done || got.DoneBy != fmt.Sprintf("task #%d", taskA) || got.DoneAt == 0 {
		t.Fatalf("its task merged: %+v, want done by the task", got)
	}

	// Cancelled: open again, still linked. Removed: the link goes.
	if rec := do(t, s, "PATCH", fmt.Sprintf("/v1/tasks/%d", taskB), `{"status":"cancelled"}`); rec.Code != 200 {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body)
	}
	if got := itemOf(t, st, cancelled); got.Status != todos.Open || got.TaskID != taskB || !strings.Contains(got.Note, "was cancelled") {
		t.Fatalf("its task was cancelled: %+v, want it open, linked, with a note", got)
	}
	if rec := do(t, s, "DELETE", fmt.Sprintf("/v1/tasks/%d", taskB), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove = %d %s", rec.Code, rec.Body)
	}
	if got := itemOf(t, st, cancelled); got.Status != todos.Open || got.TaskID != 0 {
		t.Fatalf("its task was removed: %+v, want it open with no task", got)
	}

	// The runner merges a task: it saves through the store and tells its
	// host, which is where the item hears of it.
	c, _ := st.Task(taskC)
	c.Status = taskq.Done
	if _, err := st.SaveTask(*c); err != nil {
		t.Fatal(err)
	}
	if got := itemOf(t, st, byRunner); got.Status != todos.Tasked {
		t.Fatalf("before the runner tells its host: %+v", got)
	}
	host := taskHost{mgr: s.mgr, settle: s.settleTodos}
	host.TasksChanged()
	host.TasksChanged() // a merge saves its task twice
	if got := itemOf(t, st, byRunner); got.Status != todos.Done || got.DoneBy != fmt.Sprintf("task #%d", taskC) {
		t.Fatalf("the runner merged its task: %+v, want done by the task", got)
	}
	// What no task was filed for never moved.
	if got := itemOf(t, st, plain); got.Status != todos.Open || got.TaskID != 0 || got.UpdatedAt != got.CreatedAt {
		t.Fatalf("an item with no task changed: %+v", got)
	}
}

// A filing that names a to-do item it cannot be for is refused whole: no
// task is filed, and the item is as it was.
func TestTodos_AFilingForTheWrongItemFilesNothing(t *testing.T) {
	s, st := todoServer(t)
	other := addItem(t, s, "/p/backend", "Not this repo's")
	done := addItem(t, s, "/p/app", "Fixed already")
	todoCall(t, s, "PATCH", fmt.Sprintf("/v1/todos/%d", done), `{"status":"done"}`, http.StatusOK)

	for name, c := range map[string]struct {
		todo int64
		want int
	}{"no such item": {999, http.StatusNotFound}, "another repo's": {other, http.StatusNotFound}, "a done one": {done, http.StatusConflict}} {
		_, status, err := s.applyPaneTask(todoFiler, paneTaskReq{Action: "file", Title: "Fix it", Plan: "p", Todo: c.todo})
		if err == nil || status != c.want || !strings.Contains(err.Error(), "file again") {
			t.Errorf("%s: %d %v, want %d and a way on", name, status, err, c.want)
		}
	}
	if all, _ := st.Tasks(); len(all) != 0 {
		t.Fatalf("%d tasks were filed by refused filings", len(all))
	}
	if got := itemOf(t, st, other); got.Status != todos.Open || got.TaskID != 0 {
		t.Fatalf("another repo's item moved: %+v", got)
	}
	// A filing for no item is as it always was.
	if msg := call(t, s, todoFiler, paneTaskReq{Action: "file", Title: "Plain", Plan: "p"}); strings.Contains(msg, "To-do") {
		t.Fatalf("a plain filing speaks of a to-do: %q", msg)
	}
}
