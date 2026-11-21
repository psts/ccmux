package threads

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

func TestAMembersMessageToARepoGoesToItsThreadsOwnHelper(t *testing.T) {
	r := newRig(t)
	th, lead := r.thread("Trade CSV export")
	got, ok := r.svc.Route(lead.PaneID, "backend")
	if !ok || got != (Route{ThreadID: th.ID, Repo: r.repo("backend"), Name: "backend-thread-1"}) {
		t.Fatalf("a repo's plain name is the thread's own helper there: %+v %v", got, ok)
	}
	r.host.list[2].Name = "admin-ui" // the session is called something other than its folder
	for _, name := range []string{"admin", "admin-ui"} {
		if got, ok := r.svc.Route(lead.PaneID, name); !ok || got.Name != "admin-thread-1" {
			t.Fatalf("%q, the folder's or the session's name, both reach the helper: %+v %v", name, got, ok)
		}
	}
	// What is not this thread's to route keeps the routing it always had.
	r.host.list = append(r.host.list, &model.Workspace{ID: "ws-radar", Name: "radar", Agent: "radar", RepoPath: r.repo("radar")})
	for _, name := range []string{"app", "radar", "nobody", "backend-thread-1"} {
		if got, ok := r.svc.Route(lead.PaneID, name); ok {
			t.Fatalf("%q is not routed by the thread (its own repo, an agent, no repo, a bus name): %+v", name, got)
		}
	}
	if _, ok := r.svc.Route("tmux-pane", "backend"); ok {
		t.Fatal("a sender in no thread is never routed")
	}
	// A helper reaches the lead's repo, and another helper's, the same way.
	helper := r.helper(th, "backend")
	if got, ok := r.svc.Route(helper.PaneID, "app"); !ok || got.Name != "app-thread-1" {
		t.Fatalf("a helper naming the lead's repo reaches the lead: %+v %v", got, ok)
	}
	if got, ok := r.svc.Route(helper.PaneID, "admin"); !ok || got.Name != "admin-thread-1" {
		t.Fatalf("a helper naming a third repo reaches the thread's helper there: %+v %v", got, ok)
	}
	// Another thread led from the same repo has its own team.
	_, other := r.thread("Second")
	if got, _ := r.svc.Route(other.PaneID, "backend"); got.Name != "backend-thread-2" {
		t.Fatalf("each thread has its own helper per repo: %+v", got)
	}
}

func TestAHelperStartsLeanInItsReposFolderOnTheThreadsModel(t *testing.T) {
	r := newRig(t)
	th, _ := r.thread("Trade CSV export")
	h := r.helper(th, "backend")
	want := store.RunnerSession{Name: "backend-thread-1", Cwd: r.repo("backend"), Model: "haiku", WorkspaceID: "ws-backend", Title: "Trade CSV export",
		ThreadID: th.ID, Role: store.RoleHelper, Repo: r.repo("backend"), ReadOnly: true}
	got := h
	got.ID, got.PaneID, got.Wanted, got.CreatedAt, got.UpdatedAt = "", "", "", 0, 0
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("a helper is a fresh session in the repo's own folder, read only:\n got %+v\nwant %+v", got, want)
	}
	// Two messages to the same repo at once: one helper.
	if err := r.svc.StartHelper(th.ID, r.repo("backend")); err != nil {
		t.Fatalf("asking for a helper that is there is no error: %v", err)
	}
	if members, _ := r.st.ThreadMembers(th.ID); len(members) != 2 {
		t.Fatalf("still one helper for the repo: %+v", members)
	}
	if err := r.svc.StartHelper(th.ID, "/elsewhere/payments"); err == nil || !strings.Contains(err.Error(), "not a repo session in thread #1's window") {
		t.Fatalf("a repo outside the thread's window gets no helper: %v", err)
	}
	if err := r.svc.StartHelper(99, r.repo("backend")); !errors.Is(err, store.ErrNoThread) {
		t.Fatalf("an unknown thread has no helpers: %v", err)
	}
}

func TestStandingAHelperDownEndsItAndTidiesItsWorktree(t *testing.T) {
	r := newRig(t)
	th, lead := r.thread("x")
	backend, admin := r.helper(th, "backend"), r.helper(th, "admin")
	r.edits(backend.ID)
	said, err := r.svc.StandDown(th.ID, "backend", ByLead)
	if err != nil || !strings.Contains(said, "Stood down backend-thread-1") {
		t.Fatalf("the lead hears it: %q %v", said, err)
	}
	if got := r.row(backend.ID); got.Wanted != store.SessionDone {
		t.Fatalf("its session is ended: %+v", got)
	}
	if removed := r.git.did(func(g *fakeGit) []string { return g.removed }); len(removed) != 1 || removed[0] != "backend" {
		t.Fatalf("its worktree goes with it: %v", removed)
	}
	if r.row(admin.ID).Wanted != store.SessionRunning || r.row(lead.ID).Wanted != store.SessionRunning {
		t.Fatal("the other helper and the lead stay")
	}
	if _, ok := r.svc.Route(backend.PaneID, "app"); ok {
		t.Fatal("a helper that was stood down is in no thread any more")
	}
	// A helper that only read has no worktree to remove.
	if _, err := r.svc.StandDown(th.ID, "admin", ByLead); err != nil {
		t.Fatal(err)
	}
	if removed := r.git.did(func(g *fakeGit) []string { return g.removed }); len(removed) != 1 {
		t.Fatalf("nothing is removed for a helper that changed nothing: %v", removed)
	}
	if _, err := r.svc.StandDown(th.ID, "backend", ByLead); err == nil || !strings.Contains(err.Error(), "no helper for \"backend\" (its helpers: none)") {
		t.Fatalf("a repo with no helper says so: %v", err)
	}
	if _, err := r.svc.StandDown(th.ID, "app", ByLead); err == nil {
		t.Fatal("the lead is not a helper to stand down")
	}
}

func TestAWorktreeWithUnmergedWorkStaysWhenItsHelperIsStoodDown(t *testing.T) {
	r := newRig(t)
	th, _ := r.thread("x")
	backend := r.helper(th, "backend")
	r.edits(backend.ID)
	r.git.set(func(g *fakeGit) { g.kept = map[string]string{"backend": "kept, it holds uncommitted files"} })
	said, err := r.svc.StandDown(th.ID, "backend", ByLead)
	if err != nil || !strings.Contains(said, "left for you") || !strings.Contains(said, "uncommitted files") {
		t.Fatalf("what could not be removed is named, never forced: %q %v", said, err)
	}
	if r.row(backend.ID).Wanted != store.SessionDone {
		t.Fatal("the helper is still stood down")
	}
}

func TestEndingAThreadStandsItsHelpersDownAndTheLeadStays(t *testing.T) {
	r := newRig(t)
	th, lead := r.thread("x")
	other, otherLead := r.thread("y")
	backend, kept := r.helper(th, "backend"), r.helper(other, "backend")
	said, err := r.svc.End(th.ID, ByLead)
	if err != nil || !strings.Contains(said, "Thread #1 is closed") {
		t.Fatalf("the lead hears it: %q %v", said, err)
	}
	if r.row(backend.ID).Wanted != store.SessionDone || r.row(lead.ID).Wanted != store.SessionRunning {
		t.Fatal("its helpers are stood down, the lead says its last words itself")
	}
	if r.state(th.ID).State != store.ThreadEnded {
		t.Fatalf("the thread reads as ended: %+v", r.state(th.ID))
	}
	if r.row(kept.ID).Wanted != store.SessionRunning || r.row(otherLead.ID).Wanted != store.SessionRunning || r.state(other.ID).State != store.ThreadOpen {
		t.Fatal("another thread's team is not touched")
	}
	// The lead takes the work up again: a helper is stood up, and the thread is open.
	again := r.helper(th, "backend")
	if again.ID == backend.ID || r.state(th.ID).State != store.ThreadOpen {
		t.Fatalf("new work opens the thread again with a fresh helper: %+v %+v", again, r.state(th.ID))
	}
}

func TestALeadThatEndsTakesItsHelpersWithIt(t *testing.T) {
	r := newRig(t)
	th, lead := r.thread("x")
	other, _ := r.thread("y")
	backend, kept := r.helper(th, "backend"), r.helper(other, "admin")
	lead = r.edits(lead.ID)
	// A person ends the lead's session on its page.
	if err := r.sess.SetWanted(lead.ID, store.SessionDone); err != nil {
		t.Fatal(err)
	}
	lead.Wanted = store.SessionDone
	r.svc.SessionEnded(lead)
	if r.row(backend.ID).Wanted != store.SessionDone {
		t.Fatal("a thread with no lead keeps no helpers: nothing would ever merge or end them")
	}
	if removed := r.git.did(func(g *fakeGit) []string { return g.removed }); len(removed) != 1 || removed[0] != "app" {
		t.Fatalf("the lead's own worktree is tidied too: %v", removed)
	}
	if r.state(th.ID).State != store.ThreadEnded {
		t.Fatalf("the thread is over: %+v", r.state(th.ID))
	}
	if list, _ := r.st.Threads(); len(list) != 1 || list[0].ID != other.ID {
		t.Fatalf("and gone from the list, the other thread still there: %+v", list)
	}
	if r.row(kept.ID).Wanted != store.SessionRunning {
		t.Fatal("another thread's helper is not touched")
	}
	if err := r.svc.StartHelper(th.ID, r.repo("admin")); err == nil || !strings.Contains(err.Error(), "its lead session has ended") {
		t.Fatalf("nothing joins a thread that is over: %v", err)
	}
	// A helper ending on its own only leaves its team.
	r.sess.SetWanted(kept.ID, store.SessionDone)
	gone := r.row(kept.ID)
	r.svc.SessionEnded(gone)
	if r.state(other.ID).State != store.ThreadOpen {
		t.Fatal("a helper that ended does not end its thread")
	}
	if _, ok := r.svc.Route(kept.PaneID, "app"); ok {
		t.Fatal("and it is no member any more")
	}
	r.svc.SessionEnded(store.RunnerSession{ID: "plain", Wanted: store.SessionDone}) // a session in no thread: nothing to do
}
