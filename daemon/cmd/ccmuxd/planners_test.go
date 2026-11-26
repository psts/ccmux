package main

import (
	"path/filepath"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/threads"
)

// The daemon's wiring for a repo's planner, over the real bus hooks: a
// planner that was just made is heard (teamHears), so its message to
// backend routes to a helper of its own, named apart from the main
// session's helper for the same repo, and the bus's start of that route
// makes it in the planner's own side thread. The planner ending takes its
// helper with it and leaves the main session's.
func TestTheBusStartsAPlannersOwnSideHelper(t *testing.T) {
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
	planner := store.RunnerSession{ID: "pl1", PaneID: "plp", Cwd: "/r/hq", Wanted: store.SessionRunning, WorkspaceID: "ws-hq", Kind: store.SessionPlanner, Title: "Planner"}
	for _, s := range []store.RunnerSession{main, planner} {
		if err := st.AddRunnerSession(s); err != nil {
			t.Fatal(err)
		}
		teamHears(team, s)
	}
	started := map[string]string{}
	for pane, want := range map[string]string{"mp": "backend-for-hq", "plp": "backend-for-hq-planner"} {
		route, ok := bus.ThreadRoute(pane, "backend")
		if !ok || route.Name != want || route.Repo != "/r/backend" {
			t.Fatalf("%s's message to backend: %+v %v, want its own helper %s", pane, route, ok, want)
		}
		if err := bus.StartHelper(route); err != nil {
			t.Fatalf("the bus starts %s: %v", want, err)
		}
		started[route.Lead] = want
	}
	helperOf := func(lead string) store.RunnerSession {
		th, err := st.SideThread(lead)
		if err != nil || th == nil {
			t.Fatalf("%s's side thread: %+v %v", lead, th, err)
		}
		members, _ := st.ThreadMembers(th.ID)
		if len(members) != 1 || members[0].Name != started[lead] {
			t.Fatalf("%s's helpers: %+v, want the one called %s", lead, members, started[lead])
		}
		return members[0]
	}
	mine, theirs := helperOf("pl1"), helperOf("m1")

	if err := st.SetRunnerSessionWanted("pl1", store.SessionDone, 2); err != nil {
		t.Fatal(err)
	}
	planner.Wanted = store.SessionDone
	teamHears(team, planner)
	if row, _ := st.RunnerSession(mine.ID); row == nil || row.Wanted != store.SessionDone {
		t.Fatalf("the planner ending ends its helper: %+v", row)
	}
	if row, _ := st.RunnerSession(theirs.ID); row == nil || row.Wanted == store.SessionDone {
		t.Fatalf("the planner ending ended the main session's helper: %+v", row)
	}
}
