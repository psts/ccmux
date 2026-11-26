package main

import (
	"ccmux.dev/ccmuxd/internal/api"
	"ccmux.dev/ccmuxd/internal/runner"
)

// wirePlanners gives the repo planners' minute pass (api.PlannerPass, run
// on the side helpers' timer in runners.go) what it ends an idle planner
// on: the supervisor says when one's cache has gone cold with nothing
// waiting on it, and the bus whether it still owes a delegator a result.
// With no bus nothing is owed.
func wirePlanners(w runnerWiring, sup *runner.Supervisor) {
	doors := api.PlannerDoors{Cold: sup.Cold}
	if w.peers != nil {
		doors.Owes = func(paneID string) bool { return w.peers.OpenTaskCountForPane(paneID) > 0 }
	}
	w.api.SetPlannerDoors(doors)
}
