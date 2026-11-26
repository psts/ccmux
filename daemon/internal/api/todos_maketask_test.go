package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/todos"
)

// plannerSup is a supervisor whose sessions are real rows: Create writes
// one and tells the manager, as the real one does, so the routes read
// their planners back from the registry.
type plannerSup struct {
	*fakeRunners
	st      *store.SQLite
	s       *Server
	n       int
	got     map[string][]string // session id → the prompts it was sent
	senders []string
	// noSend refuses every prompt; beforeCreate runs ahead of each insert.
	noSend       bool
	beforeCreate func()
}

func (p *plannerSup) Create(n runner.NewSession) (store.RunnerSession, error) {
	if p.beforeCreate != nil {
		p.beforeCreate()
	}
	p.n++
	sess := store.RunnerSession{
		ID: fmt.Sprintf("made%d", p.n), PaneID: fmt.Sprintf("pane-made%d", p.n), Cwd: n.Cwd, Model: n.Model, Wanted: store.SessionRunning,
		WorkspaceID: n.WorkspaceID, Kind: n.Kind, Title: n.Title, CreatedAt: int64(p.n),
	}
	if err := p.st.AddRunnerSession(sess); err != nil {
		return sess, err
	}
	p.created = append(p.created, n)
	p.s.mgr.SyncRunnerPane(sess)
	return sess, nil
}

func (p *plannerSup) Send(id, text, sender string) (string, error) {
	row, err := p.st.RunnerSession(id)
	switch {
	case err != nil || row == nil:
		return "", store.ErrNoSession
	case row.Wanted == store.SessionDone:
		return "", runner.ErrSessionDone
	case p.noSend:
		return "", errors.New("the inbox is closed")
	}
	p.got[id] = append(p.got[id], text)
	p.senders = append(p.senders, sender)
	return "p", nil
}

func (p *plannerSup) SetWanted(id, wanted string) error {
	if err := p.st.SetRunnerSessionWanted(id, wanted, 99); err != nil {
		return err
	}
	if row, _ := p.st.RunnerSession(id); row != nil {
		p.s.mgr.SyncRunnerPane(*row)
	}
	return nil
}

// plannerServer is a server with one repo session (ws-app, in window
// ChartLabs), its to-do lists and task queue, over the real registry.
func plannerServer(t *testing.T) (*Server, *plannerSup, *store.SQLite, string) {
	t.Helper()
	repo := t.TempDir()
	s, f, st := sessionsServer(t, repo)
	sup := &plannerSup{fakeRunners: f, st: st, s: s, got: map[string][]string{}}
	s.SetRunners(sup, st)
	s.SetTasks(st)
	s.SetTodos(st)
	if err := s.mgr.SeedWindowMembership("ws-app", "ChartLabs"); err != nil {
		t.Fatal(err)
	}
	return s, sup, st, repo
}

func planners(t *testing.T, st *store.SQLite) []store.RunnerSession {
	t.Helper()
	list, err := st.RunnerPlanners()
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func makeTask(t *testing.T, s *Server, id int64, body string, want int) todoAnswer {
	t.Helper()
	return todoCall(t, s, "POST", fmt.Sprintf("/v1/todos/%d/task", id), body, want)
}

// "Make task" puts an item in its repo's planning line and sends it to the
// repo's planner, one session however many items are clicked. The repo's
// main session hears nothing.
func TestTodos_MakeTaskPutsItemsInTheLineOfOnePlannerAndNeverPromptsTheMainSession(t *testing.T) {
	s, sup, st, repo := plannerServer(t)
	main := store.RunnerSession{ID: "main", PaneID: "pane-main", Cwd: repo, Wanted: store.SessionRunning, WorkspaceID: "ws-app", Kind: store.SessionLead}
	if err := st.AddRunnerSession(main); err != nil {
		t.Fatal(err)
	}
	a, b, c := addItem(t, s, repo, "Fix the flaky test in store_test.go"), addItem(t, s, repo, "Update the readme"), addItem(t, s, repo, "Drop the dead flag")

	first := makeTask(t, s, a, `{"workspaceId":"ws-app","sender":"Pat","model":"opus"}`, http.StatusOK)
	if first.Todo.Status != todos.Planning || first.Todo.PlanningAt == 0 || first.Todo.TaskID != 0 {
		t.Fatalf("made = %+v, want it in the planning line with no task until the planner files one", first.Todo)
	}
	made := planners(t, st)
	if len(made) != 1 || made[0].WorkspaceID != "ws-app" || made[0].Cwd != repo || made[0].Model != "opus" || made[0].Title != "Planner" || made[0].ThreadID != 0 {
		t.Fatalf("the repo had no planner, so one is made for it: %+v", made)
	}
	planner := made[0].ID
	makeTask(t, s, b, `{"workspaceId":"ws-app","sender":"Pat","model":"opus"}`, http.StatusOK)
	makeTask(t, s, c, `{"sender":"Pat"}`, http.StatusOK)
	if len(planners(t, st)) != 1 || len(sup.created) != 1 {
		t.Fatalf("three clicks made %d planners (%d sessions), want one", len(planners(t, st)), len(sup.created))
	}
	if len(sup.got["main"]) != 0 || len(sup.got[planner]) != 3 {
		t.Fatalf("the main session got %d prompts and the planner %d; want 0 and one per item", len(sup.got["main"]), len(sup.got[planner]))
	}
	for _, want := range []string{fmt.Sprintf("To-do #%d is in", a), "planning line", fmt.Sprintf("todo: %d", a), "tasks tool, action file", "Fix the flaky test in store_test.go"} {
		if !strings.Contains(sup.got[planner][0], want) {
			t.Errorf("the first prompt lost %q:\n%s", want, sup.got[planner][0])
		}
	}
	if strings.Contains(sup.got[planner][0], "Also in the line") {
		t.Errorf("the first item has the line to itself:\n%s", sup.got[planner][0])
	}
	third := sup.got[planner][2]
	also := strings.Index(third, "Also in the line")
	if also < 0 || !strings.Contains(third[:also], "Drop the dead flag") || !strings.Contains(third[also:], fmt.Sprintf("#%d", a)) || !strings.Contains(third[also:], "Update the readme") {
		t.Errorf("the third prompt names its item, then the others in the line:\n%s", third)
	}
	if sup.senders[0] != "Pat" {
		t.Errorf("the prompt is from %q, want the person who clicked", sup.senders[0])
	}
	if first.SessionID != planner {
		t.Errorf("the answer names session %q, want the planner %q for the page's link", first.SessionID, planner)
	}

	// Send again: the item stays where it is in the line, and is sent anew.
	again := makeTask(t, s, a, `{"workspaceId":"ws-app","sender":"Pat"}`, http.StatusOK)
	if again.Todo.PlanningAt != first.Todo.PlanningAt || len(sup.got[planner]) != 4 {
		t.Fatalf("sent again = %+v after %d prompts; want its place kept and a fourth prompt", again.Todo, len(sup.got[planner]))
	}

	// The planner files it, naming the item: it leaves the line for its task.
	msg := call(t, s, taskFiler{repo: repo, session: "app", window: "w1"}, paneTaskReq{Action: "file", Title: "Fix the flaky test", Plan: "p", Todo: todoList{a}})
	all, _ := st.Tasks()
	if got := itemOf(t, st, a); len(all) != 1 || got.Status != todos.Tasked || got.TaskID != all[0].ID || got.PlanningAt != 0 || !strings.Contains(msg, "now follows it") {
		t.Fatalf("after the filing: %+v with %d tasks (%q); want the item under its task", got, len(all), msg)
	}

	// The task is cancelled and the person presses Make task again: the item
	// goes back into the line and lets go of the cancelled task.
	if rec := do(t, s, "PATCH", fmt.Sprintf("/v1/tasks/%d", all[0].ID), `{"status":"cancelled"}`); rec.Code != 200 {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body)
	}
	back := makeTask(t, s, a, `{"workspaceId":"ws-app"}`, http.StatusOK)
	s.settleTodos()
	if got := itemOf(t, st, a); back.Todo.Status != todos.Planning || back.Todo.TaskID != 0 || back.Todo.Note != "" || got.Status != todos.Planning {
		t.Fatalf("made again = %+v, then %+v; want it in the line, free of the cancelled task", back.Todo, got)
	}
}

// What Make task refuses sends nothing, makes no planner and moves no item.
func TestTodos_MakeTaskRefusals(t *testing.T) {
	repo := t.TempDir()
	s, f, st := sessionsServer(t, repo)
	sup := &plannerSup{fakeRunners: f, st: st, s: s, got: map[string][]string{}}
	s.SetRunners(sup, st)
	s.SetTasks(st)
	s.SetTodos(st)
	id := addItem(t, s, repo, "Fix the flaky test")

	// A session in no window has no task queue to file into.
	refused := makeTask(t, s, id, `{"workspaceId":"ws-app","sender":"Pat"}`, http.StatusConflict)
	if !strings.Contains(refused.Error, "no ccmux window") {
		t.Fatalf("no window: %q", refused.Error)
	}
	if err := s.mgr.SeedWindowMembership("ws-app", "ChartLabs"); err != nil {
		t.Fatal(err)
	}
	// The row named must be one of the item's own repo.
	makeTask(t, s, id, `{"workspaceId":"ws-other"}`, http.StatusConflict)
	// A done item is no task, an item a task has is that task's, and
	// planning is no status a person sets.
	done := addItem(t, s, repo, "Fixed already")
	todoCall(t, s, "PATCH", fmt.Sprintf("/v1/todos/%d", done), `{"status":"done"}`, http.StatusOK)
	makeTask(t, s, done, `{"workspaceId":"ws-app"}`, http.StatusConflict)
	filed := addItem(t, s, repo, "Has a task")
	call(t, s, taskFiler{repo: repo, session: "app", window: "w1"}, paneTaskReq{Action: "file", Title: "Has a task", Plan: "p", Todo: todoList{filed}})
	makeTask(t, s, filed, `{"workspaceId":"ws-app"}`, http.StatusConflict)
	makeTask(t, s, 999, `{}`, http.StatusNotFound)
	todoCall(t, s, "PATCH", fmt.Sprintf("/v1/todos/%d", id), `{"status":"planning"}`, http.StatusBadRequest)

	if len(sup.got) != 0 || len(sup.created) != 0 || itemOf(t, st, id).Status != todos.Open {
		t.Fatalf("refused Make tasks sent %v, made %v, and left the item %s", sup.got, sup.created, itemOf(t, st, id).Status)
	}
}

// An item that reached no planner is open again, never left marked as
// handed on; and a planner made for it, which was sent nothing, is ended.
func TestTodos_MakeTaskThatCouldNotBeSentLeavesTheItemOpen(t *testing.T) {
	s, sup, st, repo := plannerServer(t)
	id := addItem(t, s, repo, "Fix the flaky test")
	sup.noSend = true
	refused := makeTask(t, s, id, `{"workspaceId":"ws-app"}`, http.StatusInternalServerError)
	if got := itemOf(t, st, id); got.Status != todos.Open || got.PlanningAt != 0 || !strings.Contains(refused.Error, "inbox is closed") {
		t.Fatalf("the prompt was not sent (%q), yet the item reads %+v", refused.Error, got)
	}
	if left := planners(t, st); len(left) != 0 {
		t.Fatalf("a planner that was sent nothing stays: %+v", left)
	}

	// A planner that is there and takes the first item, then refuses a
	// resend: the item stays in its line, where the first send put it.
	sup.noSend = false
	makeTask(t, s, id, `{"workspaceId":"ws-app"}`, http.StatusOK)
	sup.noSend = true
	makeTask(t, s, id, `{"workspaceId":"ws-app"}`, http.StatusInternalServerError)
	if got := itemOf(t, st, id); got.Status != todos.Planning || len(planners(t, st)) != 1 {
		t.Fatalf("a failed resend: item %+v, %d planners; want both as they were", got, len(planners(t, st)))
	}
}

// Two Make tasks that both find no planner end with one: the second insert
// is refused by the registry, and the planner that won is read back.
func TestEnsurePlanner_RereadsWhenAnotherWasMadeMeanwhile(t *testing.T) {
	s, sup, st, repo := plannerServer(t)
	id := addItem(t, s, repo, "Fix the flaky test")
	key := itemOf(t, st, id).Repo
	sup.beforeCreate = func() {
		sup.beforeCreate = nil
		other := store.RunnerSession{ID: "winner", PaneID: "pane-winner", Cwd: repo, Wanted: store.SessionRunning, WorkspaceID: "ws-app", Kind: store.SessionPlanner}
		if err := st.AddRunnerSession(other); err != nil {
			t.Fatal(err)
		}
	}
	got, made, err := s.ensurePlanner(s.mgr.Workspace("ws-app"), key, "")
	if err != nil || made || got.ID != "winner" {
		t.Fatalf("ensurePlanner = %+v, made %v, %v; want the planner that was made first", got, made, err)
	}
	again, made, err := s.ensurePlanner(s.mgr.Workspace("ws-app"), key, "")
	if err != nil || made || again.ID != "winner" || len(planners(t, st)) != 1 {
		t.Fatalf("asked again = %+v, made %v, %v with %d planners", again, made, err, len(planners(t, st)))
	}
}

// A person who takes an item out of the line (Put back, Done, Delete) is
// heard by the planner, which would file a task for it otherwise.
func TestTodos_ThePlannerHearsOfAnItemAPersonTookOutOfItsLine(t *testing.T) {
	s, sup, st, repo := plannerServer(t)
	back, done, gone, kept := addItem(t, s, repo, "Put me back"), addItem(t, s, repo, "Tick me off"), addItem(t, s, repo, "Delete me"), addItem(t, s, repo, "Leave me")
	for _, id := range []int64{back, done, gone, kept} {
		makeTask(t, s, id, `{"workspaceId":"ws-app"}`, http.StatusOK)
	}
	planner := planners(t, st)[0].ID
	sent := len(sup.got[planner])

	if put := todoCall(t, s, "PATCH", fmt.Sprintf("/v1/todos/%d", back), `{"status":"open"}`, http.StatusOK); put.Todo.Status != todos.Open || put.Todo.PlanningAt != 0 {
		t.Fatalf("put back = %+v", put.Todo)
	}
	todoCall(t, s, "PATCH", fmt.Sprintf("/v1/todos/%d", done), `{"status":"done","by":"Pat"}`, http.StatusOK)
	todoCall(t, s, "DELETE", fmt.Sprintf("/v1/todos/%d", gone), "", http.StatusNoContent)
	// A change that leaves it in the line, and one to an item that was
	// never in it, are nobody's to tell.
	todoCall(t, s, "PATCH", fmt.Sprintf("/v1/todos/%d", kept), `{"urgency":"high"}`, http.StatusOK)
	todoCall(t, s, "PATCH", fmt.Sprintf("/v1/todos/%d", back), `{"urgency":"high"}`, http.StatusOK)

	told := sup.got[planner][sent:]
	if len(told) != 3 {
		t.Fatalf("the planner was told %d things, want one per item taken out:\n%s", len(told), strings.Join(told, "\n"))
	}
	for i, want := range []string{
		fmt.Sprintf("#%d out of your planning line (they put it back on the list)", back),
		fmt.Sprintf("#%d out of your planning line (they marked it done)", done),
		fmt.Sprintf("#%d out of your planning line (they deleted it)", gone),
	} {
		if !strings.Contains(told[i], want) || !strings.Contains(told[i], "do not plan or file it") {
			t.Errorf("notice %d = %q, want it to say %q", i, told[i], want)
		}
	}
}

func TestTodos_MakeTaskIsOffWithoutRunners(t *testing.T) {
	s, _ := todoServer(t)
	id := addItem(t, s, "/p/app", "Fix the flaky test")
	todoCall(t, s, "POST", fmt.Sprintf("/v1/todos/%d/task", id), `{}`, http.StatusServiceUnavailable)
}
