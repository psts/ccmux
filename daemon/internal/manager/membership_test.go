package manager

import (
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
)

// Joining, moving and leaving a window are each one change, carrying the
// workspace and both windows; putting it in the window it is already in
// (an agent restart does) is no change. A delete needs tmux and is driven
// in the api package (TestWindowNotices).
func TestMembershipChange_JoinMoveLeaveAndDelete(t *testing.T) {
	m := windowsManager(t)
	m.byID["w1"] = &entry{ws: &model.Workspace{ID: "w1", Name: "admin", RepoPath: "/r/admin"}}
	got := make(chan MembershipChange, 8)
	m.OnMembershipChange = func(c MembershipChange) { got <- c }
	next := func() MembershipChange {
		t.Helper()
		select {
		case c := <-got:
			return c
		case <-time.After(2 * time.Second):
			t.Fatal("no change reported")
			return MembershipChange{}
		}
	}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(m.SeedWindowMembership("w1", "ChartLabs"))
	if c := next(); c.Name != "admin" || c.RepoPath != "/r/admin" || c.From != "" || c.To != "ChartLabs" {
		t.Fatalf("join = %+v", c)
	}
	must(m.SeedWindowMembership("w1", "ChartLabs"))
	must(m.AssignWorkspace("w1", "Other"))
	if c := next(); c.From != "ChartLabs" || c.To != "Other" {
		t.Fatalf("the same window must be no change, then a move: %+v", c)
	}
	must(m.AssignWorkspace("w1", ""))
	if c := next(); c.From != "Other" || c.To != "" {
		t.Fatalf("leave = %+v", c)
	}
	select {
	case c := <-got:
		t.Fatalf("unexpected extra change %+v", c)
	case <-time.After(100 * time.Millisecond):
	}
}

// Claude starts in the pane it ran in last: a claude-harness pane or one
// the bus started; a session that never had one has none to reuse.
func TestLastClaudePane(t *testing.T) {
	panes := []*model.Pane{
		{ID: "dev", Harness: ""},
		{ID: "old", Harness: "claude"},
		{ID: "bus", CreatedBy: "claude-peers"},
		{ID: "shell"},
	}
	if got := lastClaudePane(panes); got != "bus" {
		t.Fatalf("lastClaudePane = %q, want the newest claude pane", got)
	}
	if got := lastClaudePane(panes[:1]); got != "" {
		t.Fatalf("no claude pane = %q", got)
	}
	ws := &model.Workspace{Name: "Admin Panel", RepoPath: "/r/admin"}
	if !repoNamed(ws, "admin") || repoNamed(ws, "Admin Panel") || repoNamed(ws, "") || repoNamed(ws, "backend") {
		t.Fatal("a repo answers to its folder's name only: its Claude registers under it")
	}
}

// A closed session is found only in the asking window, only among repos
// (never an agent's workspace with a same-named folder), and only while
// closed: an open one is the live spawn's to use.
func TestClosedWorkspaceForRepo(t *testing.T) {
	m := windowsManager(t)
	m.byID["cl"] = &entry{ws: &model.Workspace{ID: "cl", Name: "admin", RepoPath: "/r/cl/admin"}}
	m.byID["other"] = &entry{ws: &model.Workspace{ID: "other", Name: "admin", RepoPath: "/r/other/admin"}}
	m.byID["agent"] = &entry{ws: &model.Workspace{ID: "agent", Name: "admin", RepoPath: "/x/admin", Agent: "admin"}}
	for id, win := range map[string]string{"cl": "CL", "other": "OTHER", "agent": "CL"} {
		if err := m.SeedWindowMembership(id, win); err != nil {
			t.Fatal(err)
		}
	}
	if id, repo, ok := m.ClosedWorkspaceForRepo("CL", "admin"); !ok || id != "cl" || repo != "/r/cl/admin" {
		t.Fatalf("CL admin = %q %q %v, want the CL repo only", id, repo, ok)
	}
	if _, _, ok := m.ClosedWorkspaceForRepo("OTHER", "backend"); ok {
		t.Fatal("no backend anywhere: nothing to reopen")
	}
	delete(m.byID, "cl")
	if id, _, ok := m.ClosedWorkspaceForRepo("CL", "admin"); ok {
		t.Fatalf("with the CL repo gone, neither OTHER's repo nor CL's agent may stand in: got %q", id)
	}
}
