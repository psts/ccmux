package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
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

func boolp(b bool) *bool    { return &b }
func int64p(n int64) *int64 { return &n }

var (
	appRepo     = taskFiler{repo: "/p/app", session: "app"}
	backendRepo = taskFiler{repo: "/p/backend", session: "backend"}
	adminRepo   = taskFiler{repo: "/p/admin", session: "admin"}
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
	Tasks  []taskView     `json:"tasks"`
	Counts map[string]int `json:"counts"`
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

// The lead files the feature as a draft with its part; another repo adds
// its own part by number; the lead marks it ready. One task, two parts,
// each plan written by its own repo.
func TestTasks_LeadFilesOthersAdd(t *testing.T) {
	s := taskServer(t)
	msg := call(t, s, appRepo, paneTaskReq{Action: "file", Title: "Export", Plan: "app side", Draft: boolp(true)})
	if !strings.Contains(msg, "add_part") {
		t.Fatalf("a draft's message must say how the others join: %q", msg)
	}
	call(t, s, backendRepo, paneTaskReq{Action: "add_part", ID: 1, Plan: "backend side"})
	call(t, s, backendRepo, paneTaskReq{Action: "add_part", ID: 1, Plan: "backend side, better"})
	call(t, s, appRepo, paneTaskReq{Action: "edit", ID: 1, Draft: boolp(false)})
	got := tasksOf(t, s)
	if len(got.Tasks) != 1 || got.Tasks[0].Status != taskq.Waiting || got.Counts[taskq.Waiting] != 1 {
		t.Fatalf("tasks = %+v", got)
	}
	parts := got.Tasks[0].Parts
	if len(parts) != 2 || parts[0].Repo != "app" || parts[1].Plan != "backend side, better" ||
		parts[1].BusName != "backend-task-1" || parts[1].FiledBy != "backend" {
		t.Fatalf("parts = %+v", parts)
	}
}

func TestTasks_FileRefusals(t *testing.T) {
	s := taskServer(t)
	cases := []paneTaskReq{
		{Action: "file", Title: "", Plan: "p"},
		{Action: "file", Title: "t", Plan: " "},
		{Action: "file", Title: "t", Plan: "p", After: int64p(9)},
		{Action: "nope"},
	}
	for _, req := range cases {
		if _, status, err := s.applyPaneTask(appRepo, req); err == nil || status != 400 {
			t.Errorf("%+v = %d %v, want a 400", req, status, err)
		}
	}
}

// A session reaches only the tasks its repo has a part in, and adds parts
// only before the task starts.
func TestTasks_SessionsStayInTheirTasks(t *testing.T) {
	s := taskServer(t)
	call(t, s, appRepo, paneTaskReq{Action: "file", Title: "t", Plan: "p"})
	if _, status, _ := s.applyPaneTask(adminRepo, paneTaskReq{Action: "cancel", ID: 1}); status != 404 {
		t.Fatalf("a repo with no part cancelled it: %d", status)
	}
	if _, status, _ := s.applyPaneTask(adminRepo, paneTaskReq{Action: "edit", ID: 1, Title: "x"}); status != 404 {
		t.Fatalf("a repo with no part edited it: %d", status)
	}
	task, _ := s.tasks.Task(1)
	task.Status = taskq.Running
	s.tasks.SaveTask(*task)
	if _, status, err := s.applyPaneTask(backendRepo, paneTaskReq{Action: "add_part", ID: 1, Plan: "p"}); status != 409 || !strings.Contains(err.Error(), "bus") {
		t.Fatalf("a part added to a running task from a session = %d %v", status, err)
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
	call(t, s, appRepo, paneTaskReq{Action: "file", Title: "t", Plan: "p", Draft: boolp(true)})
	call(t, s, backendRepo, paneTaskReq{Action: "add_part", ID: 1, Plan: "p"})
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
	call(t, s, appRepo, paneTaskReq{Action: "file", Title: "t", Plan: "p", Draft: boolp(true)})
	call(t, s, backendRepo, paneTaskReq{Action: "add_part", ID: 1, Plan: "p"})
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
// to a part this repo already has.
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
}
