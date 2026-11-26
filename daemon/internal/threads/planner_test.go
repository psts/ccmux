package threads

import (
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
)

// planner makes app's planner: kind planner, in no thread, on haiku.
func (r *rig) planner() store.RunnerSession {
	r.t.Helper()
	sess, err := r.sess.Create(runner.NewSession{Cwd: r.repo("app"), WorkspaceID: "ws-app", Kind: store.SessionPlanner, Title: "Planner", Model: "haiku"})
	if err != nil {
		r.t.Fatal(err)
	}
	r.svc.NoteMain(sess)
	return sess
}

// A planner asks other repos through helpers of its own, under names of
// their own: beside the main session's helper for the same repo, never in
// its place.
func TestAPlannerReachesHelpersOfItsOwnUnderTheirOwnNames(t *testing.T) {
	r := newRig(t)
	main := r.main()
	planner := r.planner()
	route, ok := r.svc.Route(planner.PaneID, "backend")
	if !ok || route.Lead != planner.ID || route.Name != "backend-for-app-planner" || route.ThreadID != 0 || route.Pane != "" {
		t.Fatalf("before its first helper: %+v %v", route, ok)
	}
	if _, ok := r.svc.Route(planner.PaneID, "app"); ok {
		t.Fatal("a planner naming its own repo is not the side thread's to route (the bus refuses it)")
	}

	mine := r.sideHelper(planner, "backend")
	theirs := r.sideHelper(main, "backend")
	if mine.Name != "backend-for-app-planner" || theirs.Name != "backend-for-app" || mine.ID == theirs.ID || mine.ThreadID == theirs.ThreadID {
		t.Fatalf("the planner's helper %+v and the main session's %+v must be two sessions in two side threads", mine, theirs)
	}
	if !mine.ReadOnly || mine.Role != store.RoleHelper || mine.Model != "haiku" || mine.Cwd != r.repo("backend") || mine.Title != "Helping app's planner" {
		t.Fatalf("the planner's helper: %+v", mine)
	}
	if th := r.state(mine.ThreadID); th.Kind != store.ThreadSide || th.LeadSession != planner.ID {
		t.Fatalf("its side thread: %+v", th)
	}
	if route, ok = r.svc.Route(planner.PaneID, "backend"); !ok || route.ThreadID != mine.ThreadID || route.Name != "backend-for-app-planner" {
		t.Fatalf("after its first helper: %+v %v", route, ok)
	}
	if route, ok = r.svc.Route(main.PaneID, "backend"); !ok || route.ThreadID != theirs.ThreadID || route.Name != "backend-for-app" {
		t.Fatalf("the main session still reaches its own: %+v %v", route, ok)
	}

	// The planner's helper is the planner's: naming app reaches the planner,
	// never app's main session, and a third repo is the planner's helper there.
	if route, ok = r.svc.Route(mine.PaneID, "app"); !ok || route.Pane != planner.PaneID || route.Name != "" {
		t.Fatalf("its helper naming the planner's repo reaches the planner: %+v %v", route, ok)
	}
	if route, ok = r.svc.Route(mine.PaneID, "admin"); !ok || route.Lead != planner.ID || route.Name != "admin-for-app-planner" {
		t.Fatalf("its helper naming a third repo: %+v %v", route, ok)
	}
}

// A planner that ends takes its helpers with it, and leaves the main
// session's alone.
func TestAPlannerThatEndsTakesItsHelpersAndOnlyItsOwn(t *testing.T) {
	r := newRig(t)
	main := r.main()
	planner := r.planner()
	mine := r.sideHelper(planner, "backend")
	theirs := r.sideHelper(main, "backend")
	if err := r.sess.SetWanted(planner.ID, store.SessionDone); err != nil {
		t.Fatal(err)
	}
	r.svc.SessionEnded(r.row(planner.ID))
	if r.row(mine.ID).Wanted != store.SessionDone || r.state(mine.ThreadID).State != store.ThreadEnded {
		t.Fatalf("the planner's helper %+v and side thread %+v outlive it", r.row(mine.ID), r.state(mine.ThreadID))
	}
	if r.row(theirs.ID).Wanted == store.SessionDone {
		t.Fatal("the main session's helper ended with the planner")
	}
	if _, ok := r.svc.Route(planner.PaneID, "backend"); ok {
		t.Fatal("an ended planner still routes")
	}
}

// A cold helper of a planner ends like any side helper; one of a planner
// that moved on to a new conversation is stood down.
func TestAPlannersHelpersEndWhenColdOrWhenItStartsOver(t *testing.T) {
	r := newRig(t)
	planner := r.planner()
	cold := r.sideHelper(planner, "backend")
	warm := r.sideHelper(planner, "admin")
	r.svc.Cold = func(id string) bool { return id == cold.ID }
	r.svc.ExpireSide()
	if r.row(cold.ID).Wanted != store.SessionDone || r.row(warm.ID).Wanted == store.SessionDone {
		t.Fatalf("only the cold helper ends: cold %+v warm %+v", r.row(cold.ID), r.row(warm.ID))
	}
	r.svc.MainMovedOn(planner.ID, "started a new conversation")
	if r.row(warm.ID).Wanted != store.SessionDone {
		t.Fatal("a planner given a new conversation keeps the old one's helper")
	}
}

// What a planner starts with: it files and never builds, how it asks other
// repos, and when a choice is the person's. Its helper is told whose it is.
func TestAPlannerStartsWithItsBrief(t *testing.T) {
	r := newRig(t)
	r.svc.BusName = func(paneID string) string { return "app-s-" + paneID }
	planner := r.planner()
	brief, err := r.svc.SessionBrief(planner)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# You are app's planner", "Your name on the peers bus is app-s-" + planner.PaneID, "You are not app's main session", "never message it",
		"tasks tool (action file", "pass todo: the item's number", "stays in planning for good", "Never build",
		`never ask "Build it now, or file it as a task?"`, `to_name="<repo>"`, "<repo>-for-app-planner", "never lands a change",
		"pass todo as the list of their numbers", "after: [the first task's number]", "todo tool (action done",
		"only when a choice is genuinely theirs", "plan and file without asking first",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("the planner's brief lacks %q", want)
		}
	}
	if t.Failed() {
		t.Logf("the brief:\n%s", brief)
	}
	if got, _ := r.svc.SessionBrief(r.main()); got != "" {
		t.Fatalf("a main session starts with no brief, as before: %q", got)
	}

	h := r.sideHelper(planner, "backend")
	helper := r.svc.Brief(h)
	for _, want := range []string{"is the planner of app", "builds nothing itself", "the planner puts it in the task it files", "under the planner's helpers", `to_name="app"`, "never land a change"} {
		if !strings.Contains(helper, want) {
			t.Errorf("its helper's brief lacks %q:\n%s", want, helper)
		}
	}
	if strings.Contains(helper, "main session") {
		t.Errorf("a planner's helper is told of a main session:\n%s", helper)
	}
}

// A planner whose pane is gone is not given a made-up name.
func TestAPlannersBriefLeavesOutANameItDoesNotKnow(t *testing.T) {
	r := newRig(t)
	brief, err := r.svc.SessionBrief(r.planner())
	if err != nil || strings.Contains(brief, "Your name on the peers bus") || !strings.Contains(brief, "You are not app's main session") {
		t.Fatalf("brief with no bus name known (%v):\n%s", err, brief)
	}
}

func TestStartSideHelperTakesAPlannerAndRefusesAPlainSession(t *testing.T) {
	r := newRig(t)
	plain, err := r.sess.Create(runner.NewSession{Cwd: r.repo("app"), WorkspaceID: "ws-app"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.StartSideHelper(plain.ID, r.repo("backend")); err == nil {
		t.Fatal("a plain session in no thread leads no side thread")
	}
	if err := r.svc.StartSideHelper(r.planner().ID, r.repo("backend")); err != nil {
		t.Fatalf("a planner's helper: %v", err)
	}
}
