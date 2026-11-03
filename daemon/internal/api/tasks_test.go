package api

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

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

var adminRepo = taskFiler{repo: "/p/admin", session: "admin"}

func fileOne(t *testing.T, s *Server, who taskFiler, req paneTaskReq) {
	t.Helper()
	req.Action = "file"
	if _, status, err := s.applyPaneTask(who, req); err != nil {
		t.Fatalf("file %q = %d %v", req.Title, status, err)
	}
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

func TestTasks_FileListAndCount(t *testing.T) {
	s := taskServer(t)
	fileOne(t, s, adminRepo, paneTaskReq{Title: "Export button", Plan: "Add it."})
	fileOne(t, s, taskFiler{repo: "/p/backend", session: "backend"}, paneTaskReq{Title: "Export route", Plan: "p", Draft: boolp(true)})
	fileOne(t, s, adminRepo, paneTaskReq{Title: "After export", Plan: "p", After: int64p(2)})
	got := tasksOf(t, s)
	if len(got.Tasks) != 3 || got.Tasks[0].Repo != "admin" || got.Tasks[0].FiledBy != "admin" {
		t.Fatalf("tasks = %+v", got.Tasks)
	}
	if got.Counts[taskq.Waiting] != 2 || got.Counts[taskq.Planning] != 1 || got.Counts[taskq.Done] != 0 {
		t.Fatalf("counts = %v", got.Counts)
	}
	if got.Tasks[2].AfterID != 2 {
		t.Fatalf("after = %d, want 2", got.Tasks[2].AfterID)
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
		if _, status, err := s.applyPaneTask(adminRepo, req); err == nil || status != 400 {
			t.Errorf("%+v = %d %v, want a 400", req, status, err)
		}
	}
}

func TestTasks_PaneEditStaysInItsRepo(t *testing.T) {
	s := taskServer(t)
	fileOne(t, s, adminRepo, paneTaskReq{Title: "t", Plan: "p"})
	backend := taskFiler{repo: "/p/backend", session: "backend"}
	if _, status, _ := s.applyPaneTask(backend, paneTaskReq{Action: "cancel", ID: 1}); status != 404 {
		t.Fatalf("another repo's cancel = %d, want 404", status)
	}
	msg, _, err := s.applyPaneTask(adminRepo, paneTaskReq{Action: "edit", ID: 1, Plan: "better"})
	if err != nil || !strings.Contains(msg, "(waiting)") {
		t.Fatalf("edit = %q %v; an edit without draft must keep the status", msg, err)
	}
	if _, _, err := s.applyPaneTask(adminRepo, paneTaskReq{Action: "edit", ID: 1, Draft: boolp(true)}); err != nil {
		t.Fatal(err)
	}
	got := tasksOf(t, s).Tasks[0]
	if got.Plan != "better" || got.Status != taskq.Planning {
		t.Fatalf("after edits = %+v", got)
	}
}

// A session can point a task at another and take that back with after 0;
// an edit that leaves after out keeps it.
func TestTasks_PaneEditClearsAfter(t *testing.T) {
	s := taskServer(t)
	fileOne(t, s, adminRepo, paneTaskReq{Title: "first", Plan: "p"})
	fileOne(t, s, adminRepo, paneTaskReq{Title: "second", Plan: "p", After: int64p(1)})
	if _, _, err := s.applyPaneTask(adminRepo, paneTaskReq{Action: "edit", ID: 2, Plan: "q"}); err != nil {
		t.Fatal(err)
	}
	if got := tasksOf(t, s).Tasks[1].AfterID; got != 1 {
		t.Fatalf("an edit without after moved it to %d", got)
	}
	if _, _, err := s.applyPaneTask(adminRepo, paneTaskReq{Action: "edit", ID: 2, After: int64p(0)}); err != nil {
		t.Fatal(err)
	}
	if got := tasksOf(t, s).Tasks[1].AfterID; got != 0 {
		t.Fatalf("after 0 left it waiting on %d", got)
	}
}

func TestTasks_UnknownPaneIsRefused(t *testing.T) {
	s := taskServer(t)
	rec := do(t, s, "POST", "/v1/panes/nope/tasks", `{"action":"list"}`)
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "hosted on this daemon") {
		t.Fatalf("unknown pane = %d %s", rec.Code, rec.Body)
	}
}

func TestTasks_LensPatchMoveDelete(t *testing.T) {
	s := taskServer(t)
	for _, title := range []string{"one", "two", "three"} {
		fileOne(t, s, adminRepo, paneTaskReq{Title: title, Plan: "p"})
	}
	if rec := do(t, s, "POST", "/v1/tasks/3/move", `{"dir":"up"}`); rec.Code != 200 {
		t.Fatalf("move = %d %s", rec.Code, rec.Body)
	}
	if order := tasksOf(t, s).Tasks; order[1].ID != 3 || order[2].ID != 2 {
		t.Fatalf("after move up = %d,%d,%d", order[0].ID, order[1].ID, order[2].ID)
	}
	if rec := do(t, s, "PATCH", "/v1/tasks/1", `{"status":"done"}`); rec.Code != 409 {
		t.Fatalf("lens finishing a task = %d, want 409", rec.Code)
	}
	if rec := do(t, s, "PATCH", "/v1/tasks/2", `{"afterId":1}`); rec.Code != 200 {
		t.Fatalf("set after = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "PATCH", "/v1/tasks/1", `{"afterId":2}`); rec.Code != 409 || !strings.Contains(rec.Body.String(), "loop") {
		t.Fatalf("loop = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "DELETE", "/v1/tasks/1", ""); rec.Code != 409 {
		t.Fatalf("delete a waiting task = %d, want 409", rec.Code)
	}
	do(t, s, "PATCH", "/v1/tasks/1", `{"status":"cancelled"}`)
	if rec := do(t, s, "DELETE", "/v1/tasks/1", ""); rec.Code != 409 || !strings.Contains(rec.Body.String(), "#2") {
		t.Fatalf("delete with a waiter = %d %s", rec.Code, rec.Body)
	}
	do(t, s, "PATCH", "/v1/tasks/2", `{"afterId":0}`)
	if rec := do(t, s, "DELETE", "/v1/tasks/1", ""); rec.Code != 204 {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "PATCH", "/v1/tasks/1", `{"plan":"x"}`); rec.Code != 404 {
		t.Fatalf("patch a deleted task = %d, want 404", rec.Code)
	}
}
