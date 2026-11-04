package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

func taskServer(t *testing.T) *Server {
	t.Helper()
	s := agentsServer(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s.SetTasks(st)
	return s
}

func int64p(n int64) *int64 { return &n }
func boolp(b bool) *bool    { return &b }

var (
	appRepo     = taskFiler{repo: "/p/app", session: "app", window: "w1"}
	backendRepo = taskFiler{repo: "/p/backend", session: "backend", window: "w1"}
	adminRepo   = taskFiler{repo: "/p/admin", session: "admin", window: "w1"}
)

func call(t *testing.T, s *Server, who taskFiler, req paneTaskReq) string {
	t.Helper()
	msg, status, err := s.applyPaneTask(who, req)
	if err != nil {
		t.Fatalf("%s %+v = %d %v", req.Action, req, status, err)
	}
	return msg
}

type taskList struct {
	Tasks  []taskView                `json:"tasks"`
	Counts map[string]map[string]int `json:"counts"`
}

func tasksOf(t *testing.T, s *Server) taskList {
	t.Helper()
	rec := do(t, s, "GET", "/v1/tasks", "")
	if rec.Code != 200 {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	var out taskList
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// twoParts files task #1 in window w1 with parts in app and backend, the
// way a lead's one filing does (multi-repo filing itself needs live sessions
// to find the repos by: tasks_itest_test.go).
func twoParts(t *testing.T, s *Server) {
	t.Helper()
	_, err := s.tasks.AddTaskWithParts(store.Task{Title: "t", Status: taskq.Waiting, WindowID: "w1", FiledBy: "app"},
		[]store.Part{{RepoPath: "/p/app", Plan: "p", Status: taskq.Pending}, {RepoPath: "/p/backend", Plan: "p", Status: taskq.Pending}})
	if err != nil {
		t.Fatal(err)
	}
}

// One repo files its feature in one step: it waits at once, in the
// session's window, with the session's part.
func TestTasks_FileOneRepo(t *testing.T) {
	s := taskServer(t)
	msg := call(t, s, appRepo, paneTaskReq{Action: "file", Title: "Export", Plan: "app side"})
	if !strings.Contains(msg, "#1 with parts for app") {
		t.Fatalf("message = %q", msg)
	}
	got := tasksOf(t, s)
	if len(got.Tasks) != 1 || got.Tasks[0].Status != taskq.Waiting || got.Counts["w1"][taskq.Waiting] != 1 {
		t.Fatalf("tasks = %+v", got)
	}
	if parts := got.Tasks[0].Parts; len(parts) != 1 || parts[0].Repo != "app" || parts[0].FiledBy != "app" {
		t.Fatalf("parts = %+v", parts)
	}
}

func TestTasks_FileRefusals(t *testing.T) {
	s := taskServer(t)
	cases := []paneTaskReq{
		{Action: "file", Title: "", Plan: "p"},
		{Action: "file", Title: "t", Plan: " "},
		{Action: "file", Title: "t", Plan: "p", After: int64p(9)},
		{Action: "file", Title: "t", Plan: "p", Parts: []partReq{{Repo: "elsewhere", Plan: "p"}}}, // not in this window
		{Action: "file", Title: "t", Parts: []partReq{{Repo: "", Plan: "p"}}},
		{Action: "file", Title: "t", Parts: []partReq{{Repo: "backend", Plan: "p"}}}, // no part of its own
		{Action: "file", Title: "t", Plan: "p", Draft: boolp(true)},                  // the old tool's draft
		{Action: "edit", ID: 1, Draft: boolp(false)},
		{Action: "nope"},
	}
	for _, req := range cases {
		if _, status, err := s.applyPaneTask(appRepo, req); err == nil || status != 400 {
			t.Errorf("%+v = %d %v, want a 400", req, status, err)
		}
	}
}

// A session reaches only the tasks its repo has a part in, and never adds
// parts by number: it files a feature with all of them.
func TestTasks_SessionsStayInTheirTasks(t *testing.T) {
	s := taskServer(t)
	call(t, s, appRepo, paneTaskReq{Action: "file", Title: "t", Plan: "p"})
	if _, status, _ := s.applyPaneTask(adminRepo, paneTaskReq{Action: "cancel", ID: 1}); status != 404 {
		t.Fatalf("a repo with no part cancelled it: %d", status)
	}
	if _, status, _ := s.applyPaneTask(adminRepo, paneTaskReq{Action: "edit", ID: 1, Title: "x"}); status != 404 {
		t.Fatalf("a repo with no part edited it: %d", status)
	}
	if _, status, err := s.applyPaneTask(backendRepo, paneTaskReq{Action: "add_part", ID: 1, Plan: "p"}); status != 409 || !strings.Contains(err.Error(), "file with parts") {
		t.Fatalf("a part added by number from a session = %d %v", status, err)
	}
	if parts, _ := s.tasks.Parts(); len(parts) != 1 {
		t.Fatalf("a refused add_part added a part: %+v", parts)
	}
}

// A worker reports on its own part, and pulls another repo into its running
// feature with a part that starts without waiting for a slot.
func TestTasks_Worker(t *testing.T) {
	s := taskServer(t)
	call(t, s, appRepo, paneTaskReq{Action: "file", Title: "t", Plan: "p"})
	task, _ := s.tasks.Task(1)
	task.Status = taskq.Running
	s.tasks.SaveTask(*task)
	parts, _ := s.tasks.Parts()
	mine := parts[0]
	mine.Status, mine.PaneID = taskq.Running, "worker-pane"
	s.tasks.SavePart(mine)
	worker := appRepo
	worker.pane = "worker-pane"

	if _, status, _ := s.applyPaneTask(appRepo, paneTaskReq{Action: "report", Status: "done"}); status != 404 {
		t.Fatalf("a session not building it reported: %d", status)
	}
	msg, _, err := s.joinPart(worker, mine, "/p/admin", "a setting for it")
	if err != nil || !strings.Contains(msg, "admin-task-1") {
		t.Fatalf("join = %q, %v", msg, err)
	}
	if _, status, _ := s.joinPart(worker, mine, "/p/admin", "again"); status != 409 {
		t.Fatalf("a second part for admin = %d, want 409", status)
	}
	call(t, s, worker, paneTaskReq{Action: "report", Status: "done", Note: " built it "})
	got := tasksOf(t, s).Tasks[0].Parts
	if got[0].Status != taskq.Ready || got[0].Note != "built it" || got[1].Status != taskq.Pending || got[1].FiledBy != "app-task-1" {
		t.Fatalf("parts after = %+v", got)
	}
}

func TestRepoNamed(t *testing.T) {
	app := t.TempDir()
	os.Mkdir(filepath.Join(app, ".git"), 0o755)
	notGit := filepath.Join(t.TempDir(), "admin")
	os.Mkdir(notGit, 0o755)
	list := []*model.Workspace{
		{RepoPath: notGit},
		{RepoPath: app, Agent: "scout"},
		{RepoPath: app},
	}
	if got, err := repoNamed(list, filepath.Base(app)); err != nil || got != app {
		t.Fatalf("by name = %q, %v", got, err)
	}
	if _, err := repoNamed(list, "admin"); err == nil {
		t.Fatal("a folder that is no git repo was taken")
	}
	if _, err := repoNamed(list[1:2], filepath.Base(app)); err == nil {
		t.Fatal("an agent's session was taken as a repo")
	}
}

func TestTasks_UnknownPaneIsRefused(t *testing.T) {
	s := taskServer(t)
	rec := do(t, s, "POST", "/v1/panes/nope/tasks", `{"action":"list"}`)
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "hosted on this daemon") {
		t.Fatalf("unknown pane = %d %s", rec.Code, rec.Body)
	}
}

func TestTasks_LensRoutes(t *testing.T) {
	s := taskServer(t)
	for _, title := range []string{"one", "two", "three"} {
		call(t, s, appRepo, paneTaskReq{Action: "file", Title: title, Plan: "p"})
	}
	if rec := do(t, s, "POST", "/v1/tasks/3/move", `{"dir":"up"}`); rec.Code != 200 {
		t.Fatalf("move = %d %s", rec.Code, rec.Body)
	}
	if order := tasksOf(t, s).Tasks; order[1].ID != 3 {
		t.Fatalf("after move up = %d,%d,%d", order[0].ID, order[1].ID, order[2].ID)
	}
	if rec := do(t, s, "PATCH", "/v1/tasks/1/parts/1", `{"plan":"edited"}`); rec.Code != 200 {
		t.Fatalf("plan edit = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "PATCH", "/v1/tasks/2/parts/1", `{"plan":"x"}`); rec.Code != 404 {
		t.Fatalf("another task's part = %d, want 404", rec.Code)
	}
	if rec := do(t, s, "PATCH", "/v1/tasks/2", `{"afterId":1}`); rec.Code != 200 {
		t.Fatalf("set after = %d %s", rec.Code, rec.Body)
	}
	do(t, s, "PATCH", "/v1/tasks/1", `{"status":"cancelled"}`)
	if rec := do(t, s, "DELETE", "/v1/tasks/1", ""); rec.Code != 409 || !strings.Contains(rec.Body.String(), "#2") {
		t.Fatalf("delete with a waiter = %d %s", rec.Code, rec.Body)
	}
	do(t, s, "PATCH", "/v1/tasks/2", `{"afterId":0}`)
	if rec := do(t, s, "DELETE", "/v1/tasks/1", ""); rec.Code != 204 {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
}

// Queued again after a cancel, a feature's unbuilt parts start again and a
// built one stays built; a failed part of a running feature can be retried.
func TestTasks_RequeueAndRetry(t *testing.T) {
	s := taskServer(t)
	twoParts(t, s)
	parts, _ := s.tasks.Parts()
	parts[0].Status, parts[1].Status = taskq.Ready, taskq.NeedsYou
	s.tasks.SavePart(parts[0])
	s.tasks.SavePart(parts[1])
	task, _ := s.tasks.Task(1)
	task.Status = taskq.Cancelled
	s.tasks.SaveTask(*task)
	if rec := do(t, s, "PATCH", "/v1/tasks/1", `{"status":"waiting"}`); rec.Code != 200 {
		t.Fatalf("requeue = %d %s", rec.Code, rec.Body)
	}
	got := tasksOf(t, s).Tasks[0].Parts
	if got[0].Status != taskq.Ready || got[1].Status != taskq.Pending {
		t.Fatalf("after requeue = %s, %s", got[0].Status, got[1].Status)
	}
	task.Status = taskq.NeedsYou
	s.tasks.SaveTask(*task)
	parts, _ = s.tasks.Parts()
	parts[1].Status = taskq.Failed
	s.tasks.SavePart(parts[1])
	if v := tasksOf(t, s).Tasks[0].Parts[1]; !v.Retryable {
		t.Fatal("a failed part of a waiting-on-you task is not offered for retry")
	}
	if rec := do(t, s, "PATCH", "/v1/tasks/1/parts/2", `{"retry":true}`); rec.Code != 200 {
		t.Fatalf("retry = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "PATCH", "/v1/tasks/1/parts/1", `{"retry":true}`); rec.Code != 409 {
		t.Fatalf("retry of a built part = %d, want 409", rec.Code)
	}
}

// A cancel stops a feature's working parts at once, not on the runner's
// next pass, so a quick Remove leaves nothing running untracked. A built
// part stays built.
func TestTasks_CancelStopsWorkersAtOnce(t *testing.T) {
	s := taskServer(t)
	twoParts(t, s)
	task, _ := s.tasks.Task(1)
	task.Status = taskq.Running
	s.tasks.SaveTask(*task)
	parts, _ := s.tasks.Parts()
	parts[0].Status, parts[0].PaneID, parts[0].Worktree = taskq.Running, "pane-a", t.TempDir()
	parts[1].Status = taskq.Ready
	s.tasks.SavePart(parts[0])
	s.tasks.SavePart(parts[1])
	call(t, s, appRepo, paneTaskReq{Action: "cancel", ID: 1})
	got := tasksOf(t, s).Tasks[0]
	if got.Status != taskq.Cancelled || got.Parts[0].Status != taskq.Pending || got.Parts[0].PaneID != "" ||
		!strings.Contains(got.Parts[0].Note, "cancelled") || got.Parts[1].Status != taskq.Ready {
		t.Fatalf("after cancel = %+v", got)
	}
	if rec := do(t, s, "DELETE", "/v1/tasks/1", ""); rec.Code != 204 {
		t.Fatalf("remove right after cancel = %d %s", rec.Code, rec.Body)
	}
}

// An edit is checked whole before anything is saved, and a plan goes only
// to a part the task already has: the caller's own, or the one it names.
func TestTasks_EditIsAllOrNothing(t *testing.T) {
	s := taskServer(t)
	call(t, s, appRepo, paneTaskReq{Action: "file", Title: "t", Plan: "first"})
	long := strings.Repeat("x", taskq.MaxTitle+1)
	if _, status, _ := s.applyPaneTask(appRepo, paneTaskReq{Action: "edit", ID: 1, Plan: "second", Title: long}); status != 409 {
		t.Fatalf("an edit with a bad title = %d, want 409", status)
	}
	if got := tasksOf(t, s).Tasks[0].Parts[0].Plan; got != "first" {
		t.Fatalf("a refused edit saved the plan: %q", got)
	}
	if _, status, _ := s.applyPaneTask(backendRepo, paneTaskReq{Action: "edit", ID: 1, Plan: "mine"}); status != 404 {
		t.Fatalf("a plan edit from a repo with no part = %d, want 404", status)
	}
	if n := len(tasksOf(t, s).Tasks[0].Parts); n != 1 {
		t.Fatalf("an edit added a part: %d parts", n)
	}
	call(t, s, appRepo, paneTaskReq{Action: "edit", ID: 1, Plan: "second"})
	if got := tasksOf(t, s).Tasks[0].Parts[0].Plan; got != "second" {
		t.Fatalf("plan edit = %q", got)
	}

	// The lead wrote every part, so it can fix another repo's plan by name.
	twoParts(t, s) // #2: app and backend
	call(t, s, appRepo, paneTaskReq{Action: "edit", ID: 2, Repo: "backend", Plan: "backend, better"})
	parts := tasksOf(t, s).Tasks[1].Parts
	if parts[0].Plan != "p" || parts[1].Plan != "backend, better" {
		t.Fatalf("named-part edit = %q / %q, want only backend changed", parts[0].Plan, parts[1].Plan)
	}
	if _, status, _ := s.applyPaneTask(appRepo, paneTaskReq{Action: "edit", ID: 2, Repo: "admin", Plan: "x"}); status != 404 {
		t.Fatalf("an edit naming a repo with no part = %d, want 404", status)
	}
}

// Each ccmux window has its own queue: a session in no window cannot file,
// counts come per window, "starts after" stays inside the window, and a
// move trades places only with a task of the same window.
func TestTasks_EachWindowHasItsOwnQueue(t *testing.T) {
	s := taskServer(t)
	plan := "a plan long enough to say what to build"
	if _, status, err := s.applyPaneTask(taskFiler{repo: "/p/loose", session: "loose"}, paneTaskReq{Action: "file", Title: "Loose", Plan: plan}); err == nil || status != 409 || !strings.Contains(err.Error(), "no ccmux window") {
		t.Fatalf("filing from no window = %d %v", status, err)
	}
	other := taskFiler{repo: "/p/site", session: "site", window: "w2"}
	call(t, s, appRepo, paneTaskReq{Action: "file", Title: "One", Plan: plan})   // #1 in w1
	call(t, s, other, paneTaskReq{Action: "file", Title: "Two", Plan: plan})     // #2 in w2
	call(t, s, appRepo, paneTaskReq{Action: "file", Title: "Three", Plan: plan}) // #3 in w1

	got := tasksOf(t, s)
	if got.Counts["w1"][taskq.Waiting] != 2 || got.Counts["w2"][taskq.Waiting] != 1 {
		t.Fatalf("counts = %v", got.Counts)
	}
	if got.Tasks[1].WindowID != "w2" {
		t.Fatalf("task 2 window = %q", got.Tasks[1].WindowID)
	}
	if _, _, err := s.applyPaneTask(appRepo, paneTaskReq{Action: "file", Title: "Four", Plan: plan, After: int64p(2)}); err == nil {
		t.Fatal("a task started after another window's task")
	}

	// #1 and #3 wait in w1: up trades #3 with #1, never with w2's #2
	// sitting between them.
	rec := do(t, s, "POST", "/v1/tasks/3/move", `{"dir":"up"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"moved":true`) {
		t.Fatalf("move = %d %s", rec.Code, rec.Body)
	}
	order := []int64{}
	for _, v := range tasksOf(t, s).Tasks {
		order = append(order, v.ID)
	}
	if fmt.Sprint(order) != "[3 2 1]" {
		t.Fatalf("order after the move = %v, want [3 2 1] (#3 traded with #1, #2 untouched)", order)
	}
}

// "Send back to worker" is offered only while the worker's pane is open,
// and the route answers the rule's refusal as a conflict, not a crash; with
// no runner it says the runner is not there.
func TestTasks_SendBackRoute(t *testing.T) {
	s := taskServer(t)
	twoParts(t, s)
	parts, _ := s.tasks.Parts()
	parts[0].Status, parts[0].PaneID = taskq.Ready, "gone-pane"
	s.tasks.SavePart(parts[0])
	task, _ := s.tasks.Task(1)
	task.Status = taskq.MergeFailed
	s.tasks.SaveTask(*task)
	if v := tasksOf(t, s).Tasks[0].Parts[0]; v.SendBackable {
		t.Fatal("offered to send back to a worker whose pane is gone")
	}
	if rec := do(t, s, "POST", "/v1/tasks/1/parts/1/send-back", ""); rec.Code != 503 {
		t.Fatalf("no runner = %d %s, want 503", rec.Code, rec.Body)
	}
	s.taskRunner = &taskq.Runner{Store: s.tasks, Host: taskHost{mgr: s.mgr}, Lock: &s.tasksMu, Now: time.Now}
	if rec := do(t, s, "POST", "/v1/tasks/1/parts/2/send-back", ""); rec.Code != 409 || !strings.Contains(rec.Body.String(), "only a built part") {
		t.Fatalf("unbuilt part = %d %s, want 409", rec.Code, rec.Body)
	}
	if rec := do(t, s, "POST", "/v1/tasks/1/parts/9/send-back", ""); rec.Code != 409 {
		t.Fatalf("no such part = %d %s, want 409", rec.Code, rec.Body)
	}
	if got := tasksOf(t, s).Tasks[0]; got.Status != taskq.MergeFailed || got.Parts[1].Status != parts[1].Status {
		t.Fatalf("a refused send-back changed the task: %+v", got)
	}
}

// With no bus, the runner's message reaches nobody and says so.
func TestTaskHost_TellWorkerWithoutABus(t *testing.T) {
	s := taskServer(t)
	if err := (taskHost{mgr: s.mgr}).TellWorker("pane-1", "hi"); err == nil {
		t.Fatal("a message with no bus was reported delivered")
	}
}

// Started after EnablePeers, the runner holds the bus. This does not pin
// the order main.go calls them in (only its comment there does): the first
// build started the runner first, and no worker could ever be told.
func TestStartTaskRunner_TakesTheBus(t *testing.T) {
	s := agentsServer(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s.SetTasks(st)
	s.EnablePeers(peers.NewService(st, s.mgr, testSecret))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.StartTaskRunner(ctx, time.Hour, 1, t.TempDir())
	if h, ok := s.taskRunner.Host.(taskHost); !ok || h.peers == nil {
		t.Fatalf("the runner's host has no bus: %+v", s.taskRunner.Host)
	}
}
