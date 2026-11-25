package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func plannerRow(id, ws string, at int64) RunnerSession {
	return RunnerSession{ID: id, Cwd: "/r/" + ws, Wanted: SessionRunning, WorkspaceID: ws, PaneID: "p-" + id, Kind: SessionPlanner, Title: "Planner", CreatedAt: at}
}

// A workspace has one planner that has not ended, beside its lead: the
// index refuses a second, and an ended one makes room for the next.
func TestPlanners_OnePerWorkspaceBesideItsLead(t *testing.T) {
	st := openTestStore(t)
	lead := RunnerSession{ID: "l1", Cwd: "/r/app", Wanted: SessionRunning, WorkspaceID: "app", PaneID: "p-l1", Kind: SessionLead, CreatedAt: 1}
	if err := st.AddRunnerSession(lead); err != nil {
		t.Fatal(err)
	}
	first := plannerRow("pl1", "app", 2)
	first.Model = "haiku"
	if err := st.AddRunnerSession(first); err != nil {
		t.Fatalf("a planner beside the lead: %v", err)
	}
	if err := st.AddRunnerSession(plannerRow("pl2", "app", 3)); !errors.Is(err, ErrPlannerExists) {
		t.Fatalf("a second planner for the workspace: %v, want ErrPlannerExists", err)
	}
	if err := st.AddRunnerSession(plannerRow("pl3", "blog", 4)); err != nil {
		t.Fatalf("another workspace's planner: %v", err)
	}
	got, err := st.RunnerPlanners()
	if err != nil || len(got) != 2 || got[0].ID != "pl1" || got[1].ID != "pl3" {
		t.Fatalf("planners = %+v, %v; want pl1 then pl3", got, err)
	}
	if l, err := st.RunnerLead("app"); err != nil || l == nil || l.ID != "l1" {
		t.Fatalf("the workspace's lead is still its lead, never its planner: %+v, %v", l, err)
	}

	if err := st.SetRunnerSessionWanted("pl1", SessionDone, 5); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRunnerSession(plannerRow("pl4", "app", 6)); err != nil {
		t.Fatalf("a new planner once the old one ended: %v", err)
	}
	if got, _ := st.RunnerPlanners(); len(got) != 2 || got[0].ID != "pl3" || got[1].ID != "pl4" {
		t.Fatalf("an ended planner is not listed: %+v", got)
	}
}

// A planner started again for a line that was left runs on the model the
// last one was given.
func TestPlanners_LastModelIsTheNewestPlannersEndedOrNot(t *testing.T) {
	st := openTestStore(t)
	if m, err := st.LastPlannerModel("app"); err != nil || m != "" {
		t.Fatalf("no planner yet = %q, %v", m, err)
	}
	old := plannerRow("pl1", "app", 2)
	old.Model, old.Wanted = "opus", SessionDone
	newer := plannerRow("pl2", "app", 9)
	newer.Model, newer.Wanted = "haiku", SessionDone
	for _, r := range []RunnerSession{old, newer} {
		if err := st.AddRunnerSession(r); err != nil {
			t.Fatal(err)
		}
	}
	if m, err := st.LastPlannerModel("app"); err != nil || m != "haiku" {
		t.Fatalf("last model = %q, %v; want the newest planner's", m, err)
	}
}

// Main sessions and planners both ask other repos through side helpers; a
// thread's lead and a plain thread session do not.
func TestSideLeads_AreMainSessionsAndPlanners(t *testing.T) {
	st := openTestStore(t)
	rows := []RunnerSession{
		{ID: "main", Cwd: "/r/app", Wanted: SessionRunning, WorkspaceID: "app", Kind: SessionLead, CreatedAt: 1},
		plannerRow("plan", "app", 2),
		{ID: "plain", Cwd: "/r/app", Wanted: SessionRunning, WorkspaceID: "app", CreatedAt: 3},
		{ID: "member", Cwd: "/r/app", Wanted: SessionRunning, WorkspaceID: "blog", ThreadID: 4, Role: RoleLead, Repo: "/r/blog", CreatedAt: 4},
		{ID: "ended", Cwd: "/r/blog", Wanted: SessionDone, WorkspaceID: "blog", Kind: SessionPlanner, CreatedAt: 5},
	}
	for _, r := range rows {
		if err := st.AddRunnerSession(r); err != nil {
			t.Fatalf("%s: %v", r.ID, err)
		}
	}
	got, err := st.SideLeads()
	if err != nil || len(got) != 2 || got[0].ID != "main" || got[1].ID != "plan" {
		t.Fatalf("side leads = %+v, %v; want the main session and the planner", got, err)
	}
}

// A registry from before the planning line opens with the new column and
// index, and its items are in no line.
func TestTodos_ARegistryFromBeforeThePlanningLineOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id := addTodo(t, st, "/r/app", "Fix the flaky test", "fix the flaky test")
	for _, q := range []string{`ALTER TABLE repo_todos DROP COLUMN planning_at`, `DROP INDEX sessions_one_planner`} {
		if _, err := st.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	st.Close()
	if st, err = Open(path); err != nil {
		t.Fatalf("open again: %v", err)
	}
	defer st.Close()
	got, err := st.Todo(id)
	if err != nil || got == nil || got.PlanningAt != 0 || got.Status != "open" {
		t.Fatalf("the old item = %+v, %v; want it open and in no line", got, err)
	}
	if err := st.AddRunnerSession(plannerRow("pl1", "app", 1)); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRunnerSession(plannerRow("pl2", "app", 2)); !errors.Is(err, ErrPlannerExists) {
		t.Fatalf("the index is back: %v, want ErrPlannerExists", err)
	}
}

// The planning line is read in the order items entered it, per repo.
func TestTodos_ThePlanningLineIsReadOldestFirst(t *testing.T) {
	st := openTestStore(t)
	a := addTodo(t, st, "/r/app", "one", "one")
	b := addTodo(t, st, "/r/app", "two", "two")
	c := addTodo(t, st, "/r/blog", "three", "three")
	addTodo(t, st, "/r/app", "not in the line", "not in the line")
	for id, at := range map[int64]int64{a: 30, b: 20, c: 25} {
		it, _ := st.Todo(id)
		it.Status, it.PlanningAt = "planning", at
		if ok, err := st.SaveTodo(*it, it.Text); err != nil || !ok {
			t.Fatalf("save #%d: %v, %v", id, ok, err)
		}
	}
	ids := func(repo string) []int64 {
		list, err := st.PlanningTodos(repo)
		if err != nil {
			t.Fatal(err)
		}
		out := []int64{}
		for _, it := range list {
			out = append(out, it.ID)
		}
		return out
	}
	if got := ids("/r/app"); len(got) != 2 || got[0] != b || got[1] != a {
		t.Fatalf("app's line = %v, want #%d then #%d", got, b, a)
	}
	if got := ids(""); len(got) != 3 || got[0] != b || got[1] != c || got[2] != a {
		t.Fatalf("every line = %v, want by the time each entered", got)
	}
}
