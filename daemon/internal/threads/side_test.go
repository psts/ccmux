package threads

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
)

// main makes app's main session: kind lead, in no thread, on sonnet.
func (r *rig) main() store.RunnerSession {
	r.t.Helper()
	sess, err := r.sess.Create(runner.NewSession{Cwd: r.repo("app"), WorkspaceID: "ws-app", Kind: store.SessionLead, Model: "sonnet"})
	if err != nil {
		r.t.Fatal(err)
	}
	r.svc.NoteMain(sess)
	return sess
}

// main2 makes the main session of the repo called name.
func (r *rig) main2(name string) store.RunnerSession {
	r.t.Helper()
	sess, err := r.sess.Create(runner.NewSession{Cwd: r.repo(name), WorkspaceID: "ws-" + name, Kind: store.SessionLead, Model: "sonnet"})
	if err != nil {
		r.t.Fatal(err)
	}
	r.svc.NoteMain(sess)
	return sess
}

// sideHelper has main ask for its helper in the repo called name, as the
// bus does, and returns that helper's row.
func (r *rig) sideHelper(main store.RunnerSession, name string) store.RunnerSession {
	r.t.Helper()
	route, ok := r.svc.Route(main.PaneID, name)
	if !ok || route.Lead != main.ID {
		r.t.Fatalf("main session's message to %s: %+v %v", name, route, ok)
	}
	if err := r.svc.StartSideHelper(route.Lead, route.Repo); err != nil {
		r.t.Fatal(err)
	}
	th, err := r.st.SideThread(main.ID)
	if err != nil || th == nil {
		r.t.Fatalf("no side thread: %+v %v", th, err)
	}
	members, err := r.st.ThreadMembers(th.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, m := range members {
		if m.Repo == r.repo(name) {
			return m
		}
	}
	r.t.Fatalf("no side helper for %s among %+v", name, members)
	return store.RunnerSession{}
}

func TestMainSessionReachesItsOwnHelperNeverTheRepo(t *testing.T) {
	r := newRig(t)
	main := r.main()
	route, ok := r.svc.Route(main.PaneID, "backend")
	if !ok || route.Lead != main.ID || route.Name != "backend-for-app" || route.ThreadID != 0 || route.Pane != "" {
		t.Fatalf("before its first helper: %+v %v", route, ok)
	}
	if _, ok := r.svc.Route(main.PaneID, "app"); ok {
		t.Fatal("a main session naming its own repo is not the side thread's to route")
	}
	if _, ok := r.svc.Route(main.PaneID, "nobody"); ok {
		t.Fatal("a name that is no repo here keeps its usual routing")
	}
	h := r.sideHelper(main, "backend")
	if h.Name != "backend-for-app" || !h.ReadOnly || h.Role != store.RoleHelper || h.Model != "sonnet" || h.Cwd != r.repo("backend") {
		t.Fatalf("side helper: %+v", h)
	}
	th := r.state(h.ThreadID)
	if th.Kind != store.ThreadSide || th.LeadSession != main.ID || th.Group != "w1" {
		t.Fatalf("side thread: %+v", th)
	}
	route, ok = r.svc.Route(main.PaneID, "backend")
	if !ok || route.ThreadID != th.ID || route.Name != "backend-for-app" {
		t.Fatalf("after its first helper: %+v %v", route, ok)
	}
}

func TestSideHelperIsStartedOnce(t *testing.T) {
	r := newRig(t)
	main := r.main()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.svc.StartSideHelper(main.ID, r.repo("backend")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	th, _ := r.st.SideThread(main.ID)
	members, _ := r.st.ThreadMembers(th.ID)
	if len(members) != 1 {
		t.Fatalf("four asks at once must make one helper: %+v", members)
	}
}

func TestSideHelperRoutes(t *testing.T) {
	r := newRig(t)
	main := r.main()
	h := r.sideHelper(main, "backend")
	route, ok := r.svc.Route(h.PaneID, "app")
	if !ok || route.Pane != main.PaneID || route.Name != "" {
		t.Fatalf("a helper naming its main session's repo reaches the main session: %+v %v", route, ok)
	}
	route, ok = r.svc.Route(h.PaneID, "admin")
	if !ok || route.Lead != main.ID || route.Name != "admin-for-app" {
		t.Fatalf("a helper naming a third repo reaches the main session's helper there: %+v %v", route, ok)
	}
	if _, ok := r.svc.Route(h.PaneID, "backend"); ok {
		t.Fatal("a helper naming its own repo is not routed: it may want that repo's own session")
	}
}

func TestStartSideHelperRefusesWhatIsNoMainSession(t *testing.T) {
	r := newRig(t)
	_, lead := r.thread("Trade CSV export")
	if err := r.svc.StartSideHelper(lead.ID, r.repo("backend")); err == nil {
		t.Fatal("a thread's lead is no main session")
	}
	main := r.main()
	if err := r.svc.StartSideHelper(main.ID, "/elsewhere/payments"); err == nil {
		t.Fatal("a repo outside the window has no helper")
	}
}

func TestSideHelperMovesIntoAScratchCopyAndBriefSaysHandBack(t *testing.T) {
	r := newRig(t)
	main := r.main()
	h := r.sideHelper(main, "backend")
	brief := r.svc.Brief(h)
	for _, want := range []string{"never land a change", "files it as a task", "scratch copy", "to_name=\"app\""} {
		if !strings.Contains(brief, want) {
			t.Fatalf("side brief lacks %q:\n%s", want, brief)
		}
	}
	moved, err := r.svc.FirstEdit(h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(moved.Notice, "Nothing here is ever merged") || strings.Contains(moved.Notice, "Commit on this branch") {
		t.Fatalf("a side helper is told of a scratch copy: %+v", moved)
	}
	if row := r.row(h.ID); row.ReadOnly || row.Worktree == "" {
		t.Fatalf("moved: %+v", row)
	}
}

func TestSideThreadIsNeverMerged(t *testing.T) {
	r := newRig(t)
	h := r.sideHelper(r.main(), "backend")
	if _, err := r.svc.Merge(h.ThreadID, ByPerson); err == nil || !strings.Contains(err.Error(), "task") {
		t.Fatalf("merging a side thread: %v", err)
	}
}

func TestColdSideHelperEndsAndItsScratchCopyGoes(t *testing.T) {
	r := newRig(t)
	main := r.main()
	cold := r.sideHelper(main, "backend")
	cold = r.edits(cold.ID)
	warm := r.sideHelper(main, "admin")
	var dropped []string
	r.svc.DropScratch = func(repo, worktree, branch string) error {
		dropped = append(dropped, worktree)
		return nil
	}
	r.svc.Cold = func(id string) bool { return id == cold.ID }
	r.svc.ExpireSide()
	if r.row(cold.ID).Wanted != store.SessionDone || r.row(warm.ID).Wanted == store.SessionDone {
		t.Fatalf("only the cold helper ends: cold %+v warm %+v", r.row(cold.ID), r.row(warm.ID))
	}
	if len(dropped) != 1 || dropped[0] != cold.Worktree {
		t.Fatalf("its scratch copy goes, forced: %v", dropped)
	}
	if _, ok := r.svc.Route(main.PaneID, "backend"); !ok {
		t.Fatal("the next question still routes, and starts a fresh helper")
	}
	if err := r.svc.StartSideHelper(main.ID, r.repo("backend")); err != nil {
		t.Fatalf("a fresh helper after the cold one ended: %v", err)
	}
}

func TestSideHelperThatOwesWorkStays(t *testing.T) {
	r := newRig(t)
	h := r.sideHelper(r.main(), "backend")
	r.svc.Cold = func(string) bool { return true }
	r.svc.Owes = func(pane string) bool { return pane == h.PaneID }
	r.svc.ExpireSide()
	if r.row(h.ID).Wanted == store.SessionDone {
		t.Fatal("a helper that owes a delegator its result is kept")
	}
}

func TestMainSessionEndingStandsItsHelpersDown(t *testing.T) {
	r := newRig(t)
	main := r.main()
	h := r.sideHelper(main, "backend")
	if err := r.sess.SetWanted(main.ID, store.SessionDone); err != nil {
		t.Fatal(err)
	}
	main.Wanted = store.SessionDone
	r.svc.SessionEnded(main)
	if r.row(h.ID).Wanted != store.SessionDone {
		t.Fatal("a main session that ended takes its helpers with it")
	}
	if th := r.state(h.ThreadID); th.State != store.ThreadEnded {
		t.Fatalf("its side thread is closed: %+v", th)
	}
}

func TestExpiryLeavesAThreadAPersonStartedAlone(t *testing.T) {
	r := newRig(t)
	th, _ := r.thread("Trade CSV export")
	normal := r.helper(th, "backend")
	side := r.sideHelper(r.main(), "admin")
	r.svc.Cold = func(string) bool { return true }
	r.svc.ExpireSide()
	if r.row(normal.ID).Wanted == store.SessionDone {
		t.Fatal("a helper of a thread a person started never ends on a cold cache")
	}
	if r.row(side.ID).Wanted != store.SessionDone {
		t.Fatal("the side helper beside it does")
	}
}

func TestExpiryClosesASideThreadWhoseMainSessionEndedUnheard(t *testing.T) {
	r := newRig(t)
	main := r.main()
	h := r.sideHelper(main, "backend")
	// The main session ended and its stand-down never ran (the daemon stopped first).
	if err := r.st.SetRunnerSessionWanted(main.ID, store.SessionDone, 5); err != nil {
		t.Fatal(err)
	}
	r.svc.Cold = func(string) bool { return false } // warm: only the ended main session can end it
	r.svc.ExpireSide()
	if r.row(h.ID).Wanted != store.SessionDone {
		t.Fatal("a helper whose main session ended is stood down on the next pass")
	}
	if th := r.state(h.ThreadID); th.State != store.ThreadEnded {
		t.Fatalf("and its side thread closed: %+v", th)
	}
	r.svc.ExpireSide() // a closed side thread is not walked again
}

func TestASideHelperThatDidNotStopIsTriedAgainOnTheNextPass(t *testing.T) {
	r := newRig(t)
	main := r.main()
	h := r.sideHelper(main, "backend")
	if err := r.st.SetRunnerSessionWanted(main.ID, store.SessionDone, 5); err != nil {
		t.Fatal(err)
	}
	r.svc.Cold = func(string) bool { return false }
	r.sess.wantFails = 1
	r.svc.ExpireSide()
	if r.row(h.ID).Wanted == store.SessionDone {
		t.Fatal("the helper's stop failed, so it still runs")
	}
	if th := r.state(h.ThreadID); th.State == store.ThreadEnded {
		t.Fatalf("its side thread stays open while a helper runs: %+v", th)
	}
	r.svc.ExpireSide()
	if r.row(h.ID).Wanted != store.SessionDone {
		t.Fatal("the next pass stands it down")
	}
	if th := r.state(h.ThreadID); th.State != store.ThreadEnded {
		t.Fatalf("and closes the side thread: %+v", th)
	}
}

func TestAPersonStandingASideHelperDownTellsTheMainSessionWhichThread(t *testing.T) {
	r := newRig(t)
	main := r.main()
	h := r.sideHelper(main, "backend")
	if _, err := r.svc.StandDown(h.ThreadID, "backend", ByPerson); err != nil {
		t.Fatal(err)
	}
	got := r.sess.prompts(main.ID)
	want := fmt.Sprintf("Thread #%d:", h.ThreadID)
	if len(got) != 1 || !strings.Contains(got[0], want) || strings.Contains(got[0], "Thread #0") {
		t.Fatalf("the main session is told of its side thread by number %q: %v", want, got)
	}
}

func TestAMainSessionMovingOnStandsItsHelpersDownAndTheNextQuestionStartsFresh(t *testing.T) {
	r := newRig(t)
	main := r.main()
	backend := r.sideHelper(main, "backend")
	admin := r.sideHelper(main, "admin")
	th := r.state(backend.ThreadID)
	other := r.main2("backend") // another repo's main session, with a helper of its own
	theirs := r.sideHelper(other, "admin")

	r.svc.MainMovedOn(main.ID, "cleared its conversation (/clear)")
	if r.row(backend.ID).Wanted != store.SessionDone || r.row(admin.ID).Wanted != store.SessionDone {
		t.Fatal("both of its helpers are stood down")
	}
	if r.row(theirs.ID).Wanted == store.SessionDone {
		t.Fatal("another main session's helper is left alone")
	}
	if got := r.state(th.ID); got.State != store.ThreadEnded {
		t.Fatalf("its side thread is closed: %+v", got)
	}
	r.svc.MainMovedOn(main.ID, "filed task #4") // closed already: nothing to do, nothing to break
	r.svc.MainMovedOn("no-such-session", "filed task #5")

	fresh := r.sideHelper(main, "backend")
	if fresh.ID == backend.ID || fresh.ThreadID != th.ID || r.state(th.ID).State != store.ThreadOpen {
		t.Fatalf("the next question starts a fresh helper in the same side thread, open again: %+v %+v", fresh, r.state(th.ID))
	}
}
