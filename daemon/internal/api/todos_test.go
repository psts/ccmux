package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/todos"
)

// todoServer is taskServer with the to-do lists on the same registry.
func todoServer(t *testing.T) (*Server, *store.SQLite) {
	t.Helper()
	s := agentsServer(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "todos.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s.SetTasks(st)
	s.SetTodos(st)
	return s, st
}

type todoAnswer struct {
	Todo  store.Todo        `json:"todo"`
	Added bool              `json:"added"`
	Todos []store.Todo      `json:"todos"`
	Repos map[string]string `json:"repos"`
	Error string            `json:"error"`
	// SessionID is Make task's: the planner the item was sent to.
	SessionID string `json:"sessionId"`
}

func todoCall(t *testing.T, s *Server, method, path, body string, want int) todoAnswer {
	t.Helper()
	rec := do(t, s, method, path, body)
	if rec.Code != want {
		t.Fatalf("%s %s %s = %d %s, want %d", method, path, body, rec.Code, rec.Body, want)
	}
	var out todoAnswer
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s answered %q: %v", method, path, rec.Body, err)
		}
	}
	return out
}

func TestTodos_RoutesAreOffWithoutAStore(t *testing.T) {
	s := agentsServer(t)
	for _, c := range [][2]string{{"GET", "/v1/todos"}, {"POST", "/v1/todos"}, {"PATCH", "/v1/todos/1"}, {"DELETE", "/v1/todos/1"}} {
		if rec := do(t, s, c[0], c[1], `{}`); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", c[0], c[1], rec.Code)
		}
	}
}

// A person's whole round on one item, as the thread page makes it: add,
// see it listed, raise it, tick it off, put it back, delete it.
func TestTodos_AddListChangeDelete(t *testing.T) {
	s, _ := todoServer(t)
	repo := t.TempDir() // no git repo: the folder is its own list

	made := todoCall(t, s, "POST", "/v1/todos", fmt.Sprintf(`{"repo":%q,"text":"Fix the flaky test","urgency":"low","by":"patric"}`, repo), http.StatusCreated)
	if !made.Added || made.Todo.ID == 0 || made.Todo.Repo != repo || made.Todo.AddedBy != "patric" || made.Todo.Status != "open" {
		t.Fatalf("added = %+v", made)
	}
	id := made.Todo.ID
	path := fmt.Sprintf("/v1/todos/%d", id)

	// Seen again: the same item, raised, and still one row.
	again := todoCall(t, s, "POST", "/v1/todos", fmt.Sprintf(`{"repo":%q,"text":"fix the flaky test.","urgency":"high"}`, repo+"/"), http.StatusOK)
	if again.Added || again.Todo.ID != id || again.Todo.Urgency != "high" {
		t.Fatalf("second sighting = %+v, want #%d raised to high", again, id)
	}
	todoCall(t, s, "POST", "/v1/todos", fmt.Sprintf(`{"repo":%q,"text":"Update the readme","urgency":"medium"}`, repo), http.StatusCreated)
	todoCall(t, s, "POST", "/v1/todos", `{"repo":"/some/other/repo","text":"Not ours","urgency":"high"}`, http.StatusCreated)

	mine := todoCall(t, s, "GET", "/v1/todos?repo="+repo, "", http.StatusOK)
	if len(mine.Todos) != 2 || mine.Todos[0].ID != id {
		t.Fatalf("the repo's list = %+v, want its two items with the high one first", mine.Todos)
	}
	if all := todoCall(t, s, "GET", "/v1/todos", "", http.StatusOK); len(all.Todos) != 3 {
		t.Fatalf("every repo's list = %d items, want 3", len(all.Todos))
	}

	done := todoCall(t, s, "PATCH", path, `{"status":"done","by":"patric"}`, http.StatusOK)
	if done.Todo.Status != "done" || done.Todo.DoneBy != "patric" || done.Todo.DoneAt == 0 {
		t.Fatalf("ticked off = %+v", done.Todo)
	}
	back := todoCall(t, s, "PATCH", path, `{"status":"open","urgency":"low","text":"Fix the flaky store test"}`, http.StatusOK)
	if back.Todo.Status != "open" || back.Todo.Urgency != "low" || back.Todo.Text != "Fix the flaky store test" || back.Todo.DoneBy != "" {
		t.Fatalf("put back = %+v", back.Todo)
	}

	todoCall(t, s, "DELETE", path, "", http.StatusNoContent)
	todoCall(t, s, "DELETE", path, "", http.StatusNotFound)
}

func TestTodos_ARefusalSaysWhyWithItsStatus(t *testing.T) {
	s, _ := todoServer(t)
	a := todoCall(t, s, "POST", "/v1/todos", `{"repo":"/r/app","text":"Fix the flaky test"}`, http.StatusCreated).Todo
	b := todoCall(t, s, "POST", "/v1/todos", `{"repo":"/r/app","text":"Update the readme"}`, http.StatusCreated).Todo
	if a.Urgency != "medium" {
		t.Fatalf("no urgency given = %q, want medium", a.Urgency)
	}
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/v1/todos", `{"repo":"/r/app","text":"  "}`, http.StatusBadRequest},
		{"POST", "/v1/todos", `{"text":"Fix it"}`, http.StatusBadRequest},
		{"POST", "/v1/todos", `{"repo":"/r/app","text":"Fix it","urgency":"now"}`, http.StatusBadRequest},
		{"POST", "/v1/todos", `not json`, http.StatusBadRequest},
		{"PATCH", "/v1/todos/abc", `{}`, http.StatusBadRequest},
		{"PATCH", "/v1/todos/999", `{"status":"done"}`, http.StatusNotFound},
		{"PATCH", fmt.Sprintf("/v1/todos/%d", a.ID), `{"status":"later"}`, http.StatusBadRequest},
		{"PATCH", fmt.Sprintf("/v1/todos/%d", b.ID), `{"text":"fix the flaky test"}`, http.StatusConflict},
		{"DELETE", "/v1/todos/abc", ``, http.StatusBadRequest},
	} {
		if got := todoCall(t, s, c.method, c.path, c.body, c.want); got.Error == "" {
			t.Errorf("%s %s %s: refused with no reason", c.method, c.path, c.body)
		}
	}
}

// Every change tells the lenses, so an open page reads the lists again.
func TestTodos_AChangeIsToldOnTheEventsFeed(t *testing.T) {
	s, _ := todoServer(t)
	_, events := s.mgr.SubscribeEvents()
	told := func() int {
		n := 0
		for {
			select {
			case ev := <-events:
				if ev.Kind == "todos-changed" {
					n++
				}
			default:
				return n
			}
		}
	}
	id := todoCall(t, s, "POST", "/v1/todos", `{"repo":"/r/app","text":"Fix the flaky test"}`, http.StatusCreated).Todo.ID
	if n := told(); n != 1 {
		t.Fatalf("an add told the lenses %d times, want 1", n)
	}
	todoCall(t, s, "POST", "/v1/todos", `{"repo":"/r/app","text":"Fix the flaky test"}`, http.StatusOK)
	if n := told(); n != 0 {
		t.Fatalf("a repeat that changed nothing told the lenses %d times", n)
	}
	todoCall(t, s, "PATCH", fmt.Sprintf("/v1/todos/%d", id), `{"urgency":"high"}`, http.StatusOK)
	todoCall(t, s, "DELETE", fmt.Sprintf("/v1/todos/%d", id), "", http.StatusNoContent)
	if n := told(); n != 2 {
		t.Fatalf("a change and a delete told the lenses %d times, want 2", n)
	}
}

// An item added from a worktree of a repo (a task builder's, a thread
// helper's) is on the repo's own list, and the list names each repo
// session's key so the page files it under that session's row.
func TestTodos_AWorktreeFilesIntoItsReposListAndTheListNamesEachSessionsKey(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(filepath.Dir(repo), "wt-task-7")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"worktree", "add", "-q", "-b", "task/7", worktree},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	s, _, st := sessionsServer(t, repo)
	s.SetTodos(st)
	call := func(method, path, body string, want int) todoAnswer {
		t.Helper()
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		if rec.Code != want {
			t.Fatalf("%s %s = %d %s, want %d", method, path, rec.Code, rec.Body, want)
		}
		var out todoAnswer
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}

	made := call("POST", "/v1/todos", fmt.Sprintf(`{"repo":%q,"text":"Fix the flaky test","by":"app-task-7"}`, worktree), http.StatusCreated)
	if made.Todo.Repo != repo {
		t.Fatalf("filed under %q, want the repo's own folder %q", made.Todo.Repo, repo)
	}
	for _, from := range []string{repo, worktree} {
		list := call("GET", "/v1/todos?repo="+from, "", http.StatusOK)
		if len(list.Todos) != 1 || list.Todos[0].ID != made.Todo.ID {
			t.Errorf("asked by %s: %+v, want the one item", from, list.Todos)
		}
		if list.Repos["ws-app"] != repo {
			t.Errorf("repos = %+v, want ws-app → %s", list.Repos, repo)
		}
	}
}

// When git cannot say which repo a folder is in, nothing is filed under a
// guess: the add and the list say so, and the item is nowhere.
func TestTodos_AFolderGitCouldNotNameGetsNoItem(t *testing.T) {
	s, _ := todoServer(t)
	s.todoKeys = &todos.Keys{
		Exists: func(string) bool { return true },
		Git: func(dir string, _ ...string) (string, error) {
			if dir == "/wt/refused" {
				return "", errors.New("exit status 128: fatal: detected dubious ownership in repository")
			}
			return "/r/app/.git", nil
		},
	}
	refused := todoCall(t, s, "POST", "/v1/todos", `{"repo":"/wt/refused","text":"Fix the flaky test"}`, http.StatusInternalServerError)
	if !strings.Contains(refused.Error, "dubious ownership") || !strings.Contains(refused.Error, "/wt/refused") {
		t.Fatalf("the refusal = %q, want the folder and git's own words", refused.Error)
	}
	todoCall(t, s, "GET", "/v1/todos?repo=/wt/refused", "", http.StatusInternalServerError)
	if all := todoCall(t, s, "GET", "/v1/todos", "", http.StatusOK); len(all.Todos) != 0 {
		t.Fatalf("an item was filed anyway: %+v", all.Todos)
	}
	// A folder git does name is filed as before.
	if made := todoCall(t, s, "POST", "/v1/todos", `{"repo":"/wt/fine","text":"Fix the flaky test"}`, http.StatusCreated); made.Todo.Repo != "/r/app" {
		t.Fatalf("filed under %q, want /r/app", made.Todo.Repo)
	}
}
