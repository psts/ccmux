package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/todos"
)

// A planner ends only when all three hold: its line is empty, its cache has
// gone cold, and it owes nobody a delegation.
func TestPlannerPass_EndsAPlannerOnlyWhenItsLineIsEmptyAndItIsColdAndOwesNothing(t *testing.T) {
	s, _, st, repo := plannerServer(t)
	id := addItem(t, s, repo, "Fix the flaky test")
	makeTask(t, s, id, `{"workspaceId":"ws-app"}`, http.StatusOK)
	planner := planners(t, st)[0]
	cold, owes := true, false
	var asked []string
	s.SetPlannerDoors(PlannerDoors{
		Cold: func(id string) bool { asked = append(asked, id); return cold },
		Owes: func(pane string) bool { return pane == planner.PaneID && owes },
	})

	s.PlannerPass()
	if len(planners(t, st)) != 1 || len(asked) != 0 {
		t.Fatalf("a planner with an item in its line was ended, or asked whether it is cold (%v)", asked)
	}
	todoCall(t, s, "PATCH", fmt.Sprintf("/v1/todos/%d", id), `{"status":"done"}`, http.StatusOK)

	cold = false
	s.PlannerPass()
	if len(planners(t, st)) != 1 {
		t.Fatal("a planner that is not cold was ended")
	}
	cold, owes = true, true
	s.PlannerPass()
	if len(planners(t, st)) != 1 {
		t.Fatal("a planner that owes a delegation was ended")
	}
	owes = false
	s.PlannerPass()
	if left := planners(t, st); len(left) != 0 {
		t.Fatalf("an idle planner with an empty line stays: %+v", left)
	}
	if row, _ := st.RunnerSession(planner.ID); row == nil || row.Wanted != store.SessionDone {
		t.Fatalf("the planner's row = %+v, want it done", row)
	}
	if _, ok := s.mgr.RunnerPane(planner.PaneID); ok {
		t.Fatal("an ended planner still has its pane")
	}
}

// With no way to ask whether it is cold, no planner is ended.
func TestPlannerPass_EndsNothingWithoutColdToAsk(t *testing.T) {
	s, _, st, repo := plannerServer(t)
	id := addItem(t, s, repo, "Fix the flaky test")
	makeTask(t, s, id, `{"workspaceId":"ws-app"}`, http.StatusOK)
	todoCall(t, s, "PATCH", fmt.Sprintf("/v1/todos/%d", id), `{"status":"done"}`, http.StatusOK)
	s.PlannerPass()
	if len(planners(t, st)) != 1 {
		t.Fatal("a planner was ended with nothing saying it is cold")
	}
}

// A line whose planner ended (a person ended it, or it is gone for good)
// gets a new one, a fresh session on the old one's model, which is sent
// the whole line at once.
func TestPlannerPass_ALineWithNoPlannerGetsANewOneWithTheWholeLine(t *testing.T) {
	s, sup, st, repo := plannerServer(t)
	a, b := addItem(t, s, repo, "Fix the flaky test"), addItem(t, s, repo, "Update the readme")
	makeTask(t, s, a, `{"workspaceId":"ws-app","model":"haiku"}`, http.StatusOK)
	makeTask(t, s, b, `{"workspaceId":"ws-app","model":"haiku"}`, http.StatusOK)
	old := planners(t, st)[0]
	if err := sup.SetWanted(old.ID, store.SessionDone); err != nil {
		t.Fatal(err)
	}

	s.PlannerPass()
	now := planners(t, st)
	if len(now) != 1 || now[0].ID == old.ID || now[0].Model != "haiku" || now[0].Kind != store.SessionPlanner || now[0].Title != "Planner" {
		t.Fatalf("after the pass: %+v, want a new planner on the old one's model", now)
	}
	got := sup.got[now[0].ID]
	if len(got) != 1 || !strings.Contains(got[0], "2 to-do items are in") || strings.Index(got[0], "Fix the flaky test") > strings.Index(got[0], "Update the readme") ||
		!strings.Contains(got[0], "a list of numbers when one task covers several") {
		t.Fatalf("the new planner was sent %q, want the whole line in one message, the longest in it first", got)
	}
	if sup.senders[len(sup.senders)-1] != "ccmux" {
		t.Errorf("the line is from %q, want ccmux", sup.senders[len(sup.senders)-1])
	}
	s.PlannerPass()
	if len(planners(t, st)) != 1 || len(sup.got[now[0].ID]) != 1 {
		t.Fatalf("a second pass made %d planners and sent %d messages; want nothing more", len(planners(t, st)), len(sup.got[now[0].ID]))
	}

	// Make task with a line left behind and no planner: the new one gets
	// every item, not only the one clicked.
	if err := sup.SetWanted(now[0].ID, store.SessionDone); err != nil {
		t.Fatal(err)
	}
	c := addItem(t, s, repo, "Drop the dead flag")
	makeTask(t, s, c, `{"workspaceId":"ws-app"}`, http.StatusOK)
	third := planners(t, st)[0]
	if got := sup.got[third.ID]; len(got) != 1 || !strings.Contains(got[0], "3 to-do items are in") {
		t.Fatalf("a planner made by Make task for a line left behind was sent %q", got)
	}
}

// A new planner that could not be sent the line is ended again, so the next
// pass tries afresh: one that was sent nothing would sit there for good.
func TestPlannerPass_APlannerThatCouldNotBeSentTheLineIsTriedAgain(t *testing.T) {
	s, sup, st, repo := plannerServer(t)
	id := addItem(t, s, repo, "Fix the flaky test")
	makeTask(t, s, id, `{"workspaceId":"ws-app"}`, http.StatusOK)
	if err := sup.SetWanted(planners(t, st)[0].ID, store.SessionDone); err != nil {
		t.Fatal(err)
	}
	sup.noSend = true
	s.PlannerPass()
	if left := planners(t, st); len(left) != 0 || itemOf(t, st, id).Status != todos.Planning {
		t.Fatalf("after a pass that could not send: %d planners, item %s; want none, and the item still in the line", len(left), itemOf(t, st, id).Status)
	}
	sup.noSend = false
	s.PlannerPass()
	if now := planners(t, st); len(now) != 1 || len(sup.got[now[0].ID]) != 1 {
		t.Fatalf("the next pass: %+v", now)
	}
}

// A planner reads the planner's rule on the bus, never the owner rule that
// tells a session to delegate work to a repo's own session.
func TestReaderOfPane_APlannerIsAPlanner(t *testing.T) {
	s, _, st, repo := plannerServer(t)
	id := addItem(t, s, repo, "Fix the flaky test")
	makeTask(t, s, id, `{"workspaceId":"ws-app"}`, http.StatusOK)
	r := s.readerOfPane(planners(t, st)[0].PaneID, "")
	if !r.Planner || r.Main || r.Agent {
		t.Fatalf("the planner's reader = %+v", r)
	}
	if plain := s.readerOfPane("tmux1", ""); plain.Planner || plain.Main {
		t.Fatalf("a terminal pane's reader = %+v", plain)
	}
}
