package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/threads"
)

// rowSessions makes the rows the supervisor would, and starts nothing.
type rowSessions struct {
	st *store.SQLite
	n  int
}

func (f *rowSessions) Create(n runner.NewSession) (store.RunnerSession, error) {
	f.n++
	sess := store.RunnerSession{ID: fmt.Sprintf("h%d", f.n), PaneID: fmt.Sprintf("hp%d", f.n), Name: n.Name, Cwd: n.Cwd, Model: n.Model,
		Wanted: store.SessionRunning, WorkspaceID: n.WorkspaceID, Kind: n.Kind, ThreadID: n.ThreadID, Role: n.Role, Repo: n.Repo, ReadOnly: n.ReadOnly}
	return sess, f.st.AddRunnerSession(sess)
}
func (f *rowSessions) Place(string, store.SessionPlace) error { return nil }
func (f *rowSessions) Relocate(string) error                  { return nil }
func (f *rowSessions) SetWanted(id, w string) error           { return f.st.SetRunnerSessionWanted(id, w, 1) }
func (f *rowSessions) SetModel(string, string) error          { return nil }
func (f *rowSessions) Send(string, string, string) (string, error) {
	return "", nil
}
func (f *rowSessions) SendAs(string, string, string, string) (string, error) { return "", nil }

// oneWindow is hq and backend in one window.
type oneWindow struct{ list []*model.Workspace }

func (h oneWindow) Workspace(id string) *model.Workspace {
	for _, ws := range h.list {
		if ws.ID == id {
			return ws
		}
	}
	return nil
}
func (h oneWindow) WindowOfWorkspace(string) (string, error) { return "w1", nil }
func (h oneWindow) WindowPeers(string) []*model.Workspace    { return h.list }

// The daemon's wiring, end to end over the real bus hooks: a main session
// that was just made is heard (teamHears), so its message to backend routes
// to a side helper of its own (wireHelpers' ThreadRoute keeps Lead), and the
// bus's start of that route makes the helper in a side thread. A session
// that is no main session is not routed, and the main session ending takes
// its helper with it.
func TestTheBusStartsAMainSessionsSideHelper(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	team := &threads.Service{St: st, Sessions: &rowSessions{st: st}, Now: time.Now,
		Host: oneWindow{list: []*model.Workspace{{ID: "ws-hq", Name: "hq", RepoPath: "/r/hq"}, {ID: "ws-be", Name: "backend", RepoPath: "/r/backend"}}}}
	bus := &peers.Service{}
	wireHelpers(team, bus)

	main := store.RunnerSession{ID: "m1", PaneID: "mp", Cwd: "/r/hq", Wanted: store.SessionRunning, WorkspaceID: "ws-hq", Kind: store.SessionLead}
	plain := store.RunnerSession{ID: "p1", PaneID: "pp", Cwd: "/r/hq", Wanted: store.SessionRunning, WorkspaceID: "ws-hq"}
	for _, s := range []store.RunnerSession{main, plain} {
		if err := st.AddRunnerSession(s); err != nil {
			t.Fatal(err)
		}
		teamHears(team, s)
	}
	if _, ok := bus.ThreadRoute("pp", "backend"); ok {
		t.Fatal("a session that is no main session keeps the old routing")
	}
	route, ok := bus.ThreadRoute("mp", "backend")
	if !ok || route.Lead != "m1" || route.Name != "backend-for-hq" || route.Repo != "/r/backend" {
		t.Fatalf("the main session's message: %+v %v", route, ok)
	}
	if err := bus.StartHelper(route); err != nil {
		t.Fatalf("the bus starts the side helper: %v", err)
	}
	th, err := st.SideThread("m1")
	if err != nil || th == nil || th.Kind != store.ThreadSide {
		t.Fatalf("side thread: %+v %v", th, err)
	}
	members, _ := st.ThreadMembers(th.ID)
	if len(members) != 1 || members[0].Name != "backend-for-hq" || !members[0].ReadOnly {
		t.Fatalf("its helper: %+v", members)
	}
	main.Wanted = store.SessionDone
	if err := st.SetRunnerSessionWanted("m1", store.SessionDone, 2); err != nil {
		t.Fatal(err)
	}
	teamHears(team, main)
	if row, _ := st.RunnerSession(members[0].ID); row == nil || row.Wanted != store.SessionDone {
		t.Fatalf("the main session ending ends its helper: %+v", row)
	}
}

// The runner's CleanStarted, as wired: a main session given a new
// conversation has its side helpers stood down.
func TestACleanStartStandsAMainSessionsHelpersDown(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	team := &threads.Service{St: st, Sessions: &rowSessions{st: st}, Now: time.Now,
		Host: oneWindow{list: []*model.Workspace{{ID: "ws-hq", Name: "hq", RepoPath: "/r/hq"}, {ID: "ws-be", Name: "backend", RepoPath: "/r/backend"}}}}
	main := store.RunnerSession{ID: "m1", PaneID: "mp", Cwd: "/r/hq", Wanted: store.SessionRunning, WorkspaceID: "ws-hq", Kind: store.SessionLead}
	if err := st.AddRunnerSession(main); err != nil {
		t.Fatal(err)
	}
	teamHears(team, main)
	if err := team.StartSideHelper("m1", "/r/backend"); err != nil {
		t.Fatal(err)
	}
	cleanStartStandsHelpersDown(team)(main)
	th, _ := st.SideThread("m1")
	if members, _ := st.ThreadMembers(th.ID); len(members) != 0 || th.State != store.ThreadEnded {
		t.Fatalf("its helpers are stood down and its side thread closed: %+v %+v", members, th)
	}
}
