package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/todos"
)

// endedLead is the rows of a daemon whose repo has a lead that no longer
// takes prompts: what sending to it fails on.
type endedLead struct{ fakeRunnerRows }

func (endedLead) RunnerLead(string) (*store.RunnerSession, error) {
	return &store.RunnerSession{ID: "ended", WorkspaceID: "ws-app", Kind: store.SessionLead}, nil
}

// "Make task" hands an item to its repo's own session, with words that
// leave it nothing to ask, and marks the item tasked only once they are on
// their way. The item gets its task when that session files one for it.
func TestTodos_MakeTaskSendsTheItemToItsReposSessionThenMarksItTasked(t *testing.T) {
	repo := t.TempDir()
	s, f, st := sessionsServer(t, repo)
	s.SetTasks(st)
	s.SetTodos(st)
	id := addItem(t, s, repo, "Fix the flaky test in store_test.go")
	path := fmt.Sprintf("/v1/todos/%d/task", id)

	// A session in no window has no task queue to file into: nothing is
	// sent, and the item is not marked.
	refused := todoCall(t, s, "POST", path, `{"workspaceId":"ws-app","sender":"Pat"}`, http.StatusConflict)
	if !strings.Contains(refused.Error, "no ccmux window") || len(f.sent) != 0 || len(f.created) != 0 {
		t.Fatalf("no window: %q, sent %v, created %v", refused.Error, f.sent, f.created)
	}
	if got := itemOf(t, st, id); got.Status != todos.Open {
		t.Fatalf("a refused Make task marked the item: %+v", got)
	}
	if err := s.mgr.SeedWindowMembership("ws-app", "ChartLabs"); err != nil {
		t.Fatal(err)
	}
	// The row named must be one of the item's own repo.
	todoCall(t, s, "POST", path, `{"workspaceId":"ws-other"}`, http.StatusConflict)
	if len(f.sent) != 0 {
		t.Fatalf("a wrong row still sent: %v", f.sent)
	}

	made := todoCall(t, s, "POST", path, `{"workspaceId":"ws-app","sender":"Pat","model":"opus"}`, http.StatusOK)
	if made.Todo.Status != todos.Tasked || made.Todo.TaskID != 0 {
		t.Fatalf("made = %+v, want tasked, with no task until the session files one", made.Todo)
	}
	if len(f.created) != 1 || f.created[0].Kind != store.SessionLead || f.created[0].WorkspaceID != "ws-app" || f.created[0].Cwd != repo || f.created[0].Model != "opus" {
		t.Fatalf("the repo had no lead, so one is made for it: %+v", f.created)
	}
	if len(f.sent) != 1 {
		t.Fatalf("sent %d prompts, want 1", len(f.sent))
	}
	for _, want := range []string{
		"Fix the flaky test in store_test.go", fmt.Sprintf("(to-do #%d)", id), fmt.Sprintf("todo: %d", id),
		`without asking the human "Build it now, or file it as a task?"`, "tasks tool (action file)",
	} {
		if !strings.Contains(f.sent[0], want) {
			t.Errorf("the prompt lost %q:\n%s", want, f.sent[0])
		}
	}

	// The session does as told: the filing names the item, which now has
	// its task.
	msg := call(t, s, taskFiler{repo: repo, session: "app", window: "w1"}, paneTaskReq{Action: "file", Title: "Fix the flaky test", Plan: "p", Todo: id})
	all, _ := st.Tasks()
	if got := itemOf(t, st, id); len(all) != 1 || got.Status != todos.Tasked || got.TaskID != all[0].ID || !strings.Contains(msg, "now follows it") {
		t.Fatalf("after the filing: %+v with %d tasks (%q); want the item under its task", got, len(all), msg)
	}

	// The task is cancelled and the person presses Make task again: the
	// item is sent anew and lets go of the cancelled task, so the page
	// offers "Send again" and not a task that will never build it.
	if rec := do(t, s, "PATCH", fmt.Sprintf("/v1/tasks/%d", all[0].ID), `{"status":"cancelled"}`); rec.Code != 200 {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body)
	}
	if got := itemOf(t, st, id); got.Status != todos.Open || got.TaskID != all[0].ID {
		t.Fatalf("its task was cancelled: %+v, want it open and still linked", got)
	}
	again := todoCall(t, s, "POST", path, `{"workspaceId":"ws-app","sender":"Pat"}`, http.StatusOK)
	if again.Todo.Status != todos.Tasked || again.Todo.TaskID != 0 || again.Todo.Note != "" || len(f.sent) != 2 {
		t.Fatalf("made again = %+v after %d prompts; want tasked with no task and no note, and a second prompt", again.Todo, len(f.sent))
	}
	s.settleTodos()
	if got := itemOf(t, st, id); got.Status != todos.Tasked {
		t.Fatalf("the cancelled task pulled the item back to %s", got.Status)
	}

	// A done item is no task.
	done := addItem(t, s, repo, "Fixed already")
	todoCall(t, s, "PATCH", fmt.Sprintf("/v1/todos/%d", done), `{"status":"done"}`, http.StatusOK)
	todoCall(t, s, "POST", fmt.Sprintf("/v1/todos/%d/task", done), `{"workspaceId":"ws-app"}`, http.StatusConflict)
	todoCall(t, s, "POST", "/v1/todos/999/task", `{}`, http.StatusNotFound)
	if len(f.sent) != 2 {
		t.Fatalf("a refused Make task sent a prompt: %v", f.sent)
	}
}

// A prompt that could not be sent leaves the item open: it was handed to
// nobody. And a repo that has a lead is not given a second one.
func TestTodos_MakeTaskThatCouldNotBeSentLeavesTheItemOpen(t *testing.T) {
	repo := t.TempDir()
	s, f, st := sessionsServer(t, repo)
	s.SetRunners(f, endedLead{})
	s.SetTodos(st)
	if err := s.mgr.SeedWindowMembership("ws-app", "ChartLabs"); err != nil {
		t.Fatal(err)
	}
	id := addItem(t, s, repo, "Fix the flaky test")
	todoCall(t, s, "POST", fmt.Sprintf("/v1/todos/%d/task", id), `{}`, http.StatusConflict)
	if got := itemOf(t, st, id); got.Status != todos.Open {
		t.Fatalf("the prompt was not sent, yet the item reads %s", got.Status)
	}
	if len(f.created) != 0 {
		t.Fatalf("a repo with a lead was given another: %+v", f.created)
	}
}

func TestTodos_MakeTaskIsOffWithoutRunners(t *testing.T) {
	s, _ := todoServer(t)
	id := addItem(t, s, "/p/app", "Fix the flaky test")
	todoCall(t, s, "POST", fmt.Sprintf("/v1/todos/%d/task", id), `{}`, http.StatusServiceUnavailable)
}
