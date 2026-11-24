package main

import (
	"log"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/loadout"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
)

// wireLoadouts makes the loadout service (internal/loadout): what a thread
// session loads from the capability catalog, as its group and repo chose.
// It writes the day-one choices before any session starts, so a session the
// supervisor starts at boot already has its repo's plugins (the commit
// gates among them). A seed that fails is said here and tried again by
// every start, which fails with the cause until it is fixed.
func wireLoadouts(w runnerWiring) *loadout.Service {
	svc := &loadout.Service{
		St: w.st, Catalog: w.agents, ClaudeDir: agent.ClaudeConfigDir, Now: time.Now,
		Repos: func() []loadout.Repo { return repoSessions(w.mgr) },
	}
	// The catalog refuses to delete an entry a group or repo has on.
	w.agents.ChoiceUsers = w.st.CapabilityChoiceUsers
	if err := svc.Seed(); err != nil {
		log.Printf("skills and plugins: the day-one choices were not written; thread sessions will not start until they are: %v", err)
	}
	w.api.SetLoadouts(svc, w.st)
	return svc
}

// repoSessions are the repo sessions on this daemon, open or closed; never
// an agent's.
func repoSessions(mgr *manager.Manager) []loadout.Repo {
	var out []loadout.Repo
	for _, ws := range mgr.List() {
		if ws.Agent == "" && ws.RepoPath != "" {
			out = append(out, loadout.Repo{WorkspaceID: ws.ID, Path: ws.RepoPath})
		}
	}
	return out
}

// staleLoadouts is the refresh's check for a session whose skills and
// plugins changed since it started: the same one the page's bar shows
// (api loadoutOf). Choices that cannot be read check nothing this pass,
// said at most once an hour (a pass runs every few seconds).
func staleLoadouts(svc *loadout.Service) func() func(store.RunnerSession) bool {
	var said time.Time
	return func() func(store.RunnerSession) bool {
		snap, err := svc.Snapshot()
		if err != nil {
			if time.Since(said) >= time.Hour {
				said = time.Now()
				log.Printf("runner: refresh: skills and plugins not read, no session refreshed for them: %v", err)
			}
			return nil
		}
		return snap.Stale
	}
}
