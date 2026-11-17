package threads

import (
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

// team is a thread led from app with helpers in backend and admin; app and
// backend have changed something, admin only read.
func team(t *testing.T) (*rig, store.Thread, store.RunnerSession, store.RunnerSession, store.RunnerSession) {
	t.Helper()
	r := newRig(t)
	th, lead := r.thread("Trade CSV export")
	backend, admin := r.helper(th, "backend"), r.helper(th, "admin")
	return r, th, r.edits(lead.ID), r.edits(backend.ID), admin
}

// said waits for the lead to be told how the merge went.
func (r *rig) said(lead store.RunnerSession) string {
	r.t.Helper()
	eventually(r.t, "the lead hears how the merge went", func() bool { return len(r.sess.prompts(lead.ID)) > 0 })
	return r.sess.prompts(lead.ID)[0]
}

func TestAThreadMergesEveryMembersBranchAndStandsItsHelpersDown(t *testing.T) {
	r, th, lead, backend, admin := team(t)
	other, otherLead := r.thread("Second")
	bystander := r.helper(other, "backend")
	started, err := r.svc.Merge(th.ID)
	if err != nil || !strings.Contains(started, "Merging thread #1: app, backend") {
		t.Fatalf("the merge starts and says which repos it lands: %q %v", started, err)
	}
	told := r.said(lead)
	if landed := r.git.did(func(g *fakeGit) []string { return g.landed }); strings.Join(landed, "|") != "app rev-app main|backend rev-backend main" {
		t.Fatalf("each branch lands on its repo's trunk, at the commit that was checked: %v", landed)
	}
	if got := r.state(th.ID); got.State != store.ThreadMerged || !strings.Contains(got.Note, "merged app into main, backend into main") {
		t.Fatalf("the thread reads as merged, with what landed: %+v", got)
	}
	for _, h := range []store.RunnerSession{backend, admin} {
		if r.row(h.ID).Wanted != store.SessionDone {
			t.Fatalf("every helper is stood down, the one that only read too: %+v", r.row(h.ID))
		}
	}
	if removed := r.git.did(func(g *fakeGit) []string { return g.removed }); strings.Join(removed, "|") != "backend|app" {
		t.Fatalf("the worktrees go, the lead's once it is out of it: %v", removed)
	}
	if got := r.row(lead.ID); got.Wanted != store.SessionRunning || got.Cwd != r.repo("app") || !got.ReadOnly || got.Worktree != "" || got.Branch != "" {
		t.Fatalf("the lead is back in its repo's folder, reading, and still there to talk to: %+v", got)
	}
	for _, want := range []string{"merged app into main, backend into main", "Nothing was pushed", "stood down", "You are back in " + r.repo("app")} {
		if !strings.Contains(told, want) {
			t.Fatalf("the lead is told %q: %s", want, told)
		}
	}
	if r.row(bystander.ID).Wanted != store.SessionRunning || r.state(other.ID).State != store.ThreadOpen || len(r.sess.prompts(otherLead.ID)) != 0 {
		t.Fatal("another thread is not touched and hears nothing")
	}
	// New work after the merge: a fresh worktree, and the thread is open again.
	again := r.edits(lead.ID)
	if again.Worktree == "" || r.state(th.ID).State != store.ThreadOpen {
		t.Fatalf("the lead's next change gets a worktree again: %+v %+v", again, r.state(th.ID))
	}
}

func TestAFailedMergeLeavesTheThreadOpenAndTellsTheLeadWhoseItIs(t *testing.T) {
	r, th, lead, backend, admin := team(t)
	r.git.set(func(g *fakeGit) { g.red = map[string]string{"backend": "FAIL: TestExport"} })
	if _, err := r.svc.Merge(th.ID); err != nil {
		t.Fatal(err)
	}
	told := r.said(lead)
	for _, want := range []string{"the merge failed", "backend: checks failed", "FAIL: TestExport", "backend-thread-1's worktree", "Pass it to that helper"} {
		if !strings.Contains(told, want) {
			t.Fatalf("the lead is told %q: %s", want, told)
		}
	}
	if landed := r.git.did(func(g *fakeGit) []string { return g.landed }); len(landed) != 0 {
		t.Fatalf("all or none: nothing landed, the green repo neither: %v", landed)
	}
	if got := r.state(th.ID); got.State != store.ThreadOpen || !strings.Contains(got.Note, "merge failed: backend: checks failed") {
		t.Fatalf("the thread is open again and says why: %+v", got)
	}
	for _, m := range []store.RunnerSession{lead, backend, admin} {
		if got := r.row(m.ID); got.Wanted != store.SessionRunning || got.Cwd != m.Cwd {
			t.Fatalf("the team is as it was: %+v", got)
		}
	}
	// The lead's own branch failing is the lead's to fix.
	r.git.set(func(g *fakeGit) { g.red = nil; g.uncommitted = map[string]string{"app": "M src/a.js"} })
	if _, err := r.svc.Merge(th.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the second result", func() bool { return len(r.sess.prompts(lead.ID)) == 2 })
	if again := r.sess.prompts(lead.ID)[1]; !strings.Contains(again, "uncommitted work in its worktree") || !strings.Contains(again, "That is yours") {
		t.Fatalf("a failure in the lead's own worktree is said to be its own: %s", again)
	}
}

func TestAMergeIsRefusedBeforeItStartsWhenItCannotLand(t *testing.T) {
	r, th, _, backend, _ := team(t)
	refused := func(want string) {
		t.Helper()
		if _, err := r.svc.Merge(th.ID); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("refused with %q, got %v", want, err)
		}
		if got := r.state(th.ID); got.State != store.ThreadOpen {
			t.Fatalf("a refused merge leaves the thread open: %+v", got)
		}
	}
	// A helper mid-turn: its work is not all there yet.
	if err := r.st.SaveRunnerAttention(backend.PaneID, "running", "", 1); err != nil {
		t.Fatal(err)
	}
	refused("backend-thread-1 is still working")
	r.st.SaveRunnerAttention(backend.PaneID, "done", "finished", 2)
	// A person has uncommitted work on the trunk in the repo's own folder.
	r.git.set(func(g *fakeGit) { g.blocked = map[string]string{"backend": "backend has uncommitted changes on main"} })
	refused("cannot merge yet: backend has uncommitted changes on main")
	r.git.set(func(g *fakeGit) { g.blocked = nil })
	// One merge at a time.
	release := make(chan struct{})
	r.git.set(func(g *fakeGit) { g.landHook = func() { <-release } })
	if _, err := r.svc.Merge(th.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Merge(th.ID); err == nil || !strings.Contains(err.Error(), "being merged already") {
		t.Fatalf("a second merge waits for the first: %v", err)
	}
	if _, err := r.svc.End(th.ID); err == nil || !strings.Contains(err.Error(), "being merged") {
		t.Fatalf("a thread is not ended under its merge: %v", err)
	}
	close(release)
	eventually(t, "the merge to finish", func() bool { return r.state(th.ID).State == store.ThreadMerged })
	// Nothing changed since: nothing to merge.
	if _, err := r.svc.Merge(th.ID); err == nil || !strings.Contains(err.Error(), "nothing to merge") {
		t.Fatalf("a thread whose members changed nothing has nothing to land: %v", err)
	}
}

func TestAMergeCutOffByADaemonRestartIsOpenAgainAndSaid(t *testing.T) {
	r, th, lead, _, _ := team(t)
	idle, idleLead := r.thread("Second")
	th.State = store.ThreadMerging
	if err := r.st.SaveThread(th); err != nil {
		t.Fatal(err)
	}
	r.svc.members = nil // a fresh daemon
	r.svc.Recover()
	if got := r.state(th.ID); got.State != store.ThreadOpen || !strings.Contains(got.Note, "daemon restart") {
		t.Fatalf("the thread is open again: %+v", got)
	}
	if said := r.sess.prompts(lead.ID); len(said) != 1 || !strings.Contains(said[0], "Ask for the merge again") {
		t.Fatalf("its lead is told to ask again: %v", said)
	}
	if r.state(idle.ID).State != store.ThreadOpen || len(r.sess.prompts(idleLead.ID)) != 0 {
		t.Fatal("a thread that was not merging hears nothing")
	}
	if _, ok := r.svc.Route(lead.PaneID, "backend"); !ok {
		t.Fatal("and the members are known to the bus again")
	}
}

func TestAThreadsPageSeesEachMemberAndWhatItsBranchHolds(t *testing.T) {
	r, th, lead, backend, admin := team(t)
	d, err := r.svc.Detail(th.ID)
	if err != nil || d.Thread.ID != th.ID || len(d.Members) != 3 {
		t.Fatalf("the thread with its three members: %+v %v", d, err)
	}
	if d.Members[0].Session.ID != lead.ID || d.Members[0].Repo != "app" || d.Members[0].Status != "2 commits, all committed" || d.Members[0].Changes.Files[0].Path != "src/app.js" {
		t.Fatalf("the lead first, with what its branch holds: %+v", d.Members[0])
	}
	byID := map[string]Member{}
	for _, m := range d.Members {
		byID[m.Session.ID] = m
	}
	if got := byID[backend.ID]; got.Repo != "backend" || got.Changes == nil || got.Session.Branch == "" {
		t.Fatalf("a helper that changed something shows its branch and changes: %+v", got)
	}
	if got := byID[admin.ID]; got.Status != "Reading only" || got.Changes != nil {
		t.Fatalf("a helper that only read has no changes: %+v", got)
	}
	if _, err := r.svc.Detail(99); err == nil {
		t.Fatal("an unknown thread is an error")
	}
}
