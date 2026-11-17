package threads

import (
	"errors"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

func TestTheThreadToolIsTheLeadsAlone(t *testing.T) {
	r, th, lead, backend, _ := team(t)
	said, err := r.svc.Tool(lead.ID, "helpers", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`Thread #1 "Trade CSV export" is open.`,
		"- app-thread-1 (lead, app): in its worktree " + lead.Worktree + " on thread/1-trade-csv-export: 2 commits, all committed; running",
		"- backend-thread-1 (helper, backend): in its worktree",
		"- admin-thread-1 (helper, admin): reading only in " + r.repo("admin"),
	} {
		if !strings.Contains(said, want) {
			t.Fatalf("the lead reads its team, lacks %q:\n%s", want, said)
		}
	}
	if _, err := r.svc.Tool(backend.ID, "merge", ""); err == nil || !strings.Contains(err.Error(), "the thread tool is the lead's") || !strings.Contains(err.Error(), "app-thread-1") {
		t.Fatalf("a helper is sent to its lead: %v", err)
	}
	if r.state(th.ID).State != store.ThreadOpen {
		t.Fatal("and nothing was merged for it")
	}
	if _, err := r.svc.Tool(lead.ID, "explode", ""); err == nil || !strings.Contains(err.Error(), `"stand_down"`) {
		t.Fatalf("an unknown action names the ones there are: %v", err)
	}
	if _, err := r.svc.Tool("plain", "helpers", ""); !errors.Is(err, ErrNotMember) {
		t.Fatalf("a session in no thread has no thread tool: %v", err)
	}
	if said, err := r.svc.Tool(lead.ID, "stand_down", " admin "); err != nil || !strings.Contains(said, "Stood down admin-thread-1") {
		t.Fatalf("stand_down names the repo: %q %v", said, err)
	}
	if said, err := r.svc.Tool(lead.ID, "merge", ""); err != nil || !strings.Contains(said, "Merging thread #1") {
		t.Fatalf("merge starts the merge: %q %v", said, err)
	}
	r.said(lead)
	if said, err := r.svc.Tool(lead.ID, "end", ""); err != nil || !strings.Contains(said, "closed") {
		t.Fatalf("end closes the thread: %q %v", said, err)
	}
	alone, err := r.svc.Tool(lead.ID, "helpers", "")
	if err != nil || !strings.Contains(alone, "is ended") || !strings.Contains(alone, "No helpers.") {
		t.Fatalf("a lead with no helpers is told how to get one: %q %v", alone, err)
	}
}

func TestAThreadFollowsItsLeadsTitleGoalAndModel(t *testing.T) {
	r := newRig(t)
	th, lead := r.thread("")
	other, otherLead := r.thread("Second")
	backend, bystander := r.helper(th, "backend"), r.helper(other, "backend")
	r.st.SetRunnerSessionTitle(lead.ID, "Trade CSV export", 2)
	r.st.SetRunnerSessionGoal(lead.ID, "Endpoint done, button next.", 2)
	r.st.SetRunnerSessionModel(lead.ID, "opus", 2)
	r.svc.LeadChanged(lead.ID)
	if got := r.state(th.ID); got.Title != "Trade CSV export" || got.Goal != "Endpoint done, button next." || got.Model != "opus" {
		t.Fatalf("the thread carries its lead's title, goal and model: %+v", got)
	}
	if r.row(backend.ID).Model != "opus" {
		t.Fatalf("its helpers follow the thread's model: %+v", r.row(backend.ID))
	}
	if r.row(bystander.ID).Model != "haiku" || r.state(other.ID).Model != "haiku" {
		t.Fatal("another thread keeps its own")
	}
	// A helper's own row changing is not the thread's.
	r.st.SetRunnerSessionModel(backend.ID, "sonnet", 3)
	r.svc.LeadChanged(backend.ID)
	r.svc.LeadChanged("plain")
	if got := r.state(th.ID); got.Model != "opus" {
		t.Fatalf("only the lead sets the thread's model: %+v", got)
	}
	_ = otherLead
	if later := r.helper(th, "admin"); later.Model != "opus" || later.Title != "Trade CSV export" {
		t.Fatalf("a helper stood up later starts on the thread's model, under its title: %+v", later)
	}
}

func TestAMembersStatusSaysWhatItsBranchHolds(t *testing.T) {
	for _, c := range []struct {
		commits, uncommitted int
		want                 string
	}{
		{0, 0, "No changes yet"}, {1, 0, "1 commit, all committed"}, {3, 0, "3 commits, all committed"},
		{0, 2, "2 files not committed"}, {2, 1, "2 commits, 1 file not committed"},
	} {
		if got := changeStatus(taskq.BranchChanges{Commits: c.commits, Uncommitted: c.uncommitted}); got != c.want {
			t.Errorf("%d commits, %d uncommitted: %q, want %q", c.commits, c.uncommitted, got, c.want)
		}
	}
}
