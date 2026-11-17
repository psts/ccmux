package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/api"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// What the daemon does at its start: the day-one choices are written from
// the repo sessions it holds before any session can start, an agent's
// session is not a repo, and the catalog's delete check hears of choices.
func TestWireLoadoutsSeedsBeforeAnySessionStarts(t *testing.T) {
	root := t.TempDir()
	claude := filepath.Join(root, ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	qa := filepath.Join(root, "cache", "qa")
	mustWrite(t, filepath.Join(qa, ".claude-plugin", "plugin.json"), `{"name":"qa","version":"1"}`)
	mustWrite(t, agent.InstalledPluginsPath(claude), `{"plugins":{"qa@personal":[{"installPath":"`+qa+`","lastUpdated":"1"}]}}`)
	repo := filepath.Join(root, "app")
	mustWrite(t, filepath.Join(repo, ".claude", "settings.local.json"), `{"enabledPlugins":{"qa@personal":true}}`)

	st, err := store.Open(filepath.Join(root, "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, ws := range []*model.Workspace{
		{ID: "ws", Name: "app", RepoPath: repo, TmuxSession: "ccmux-app-loadouts"},
		{ID: "ag", Name: "radar", RepoPath: filepath.Join(root, "radar"), TmuxSession: "ccmux-radar-loadouts", Agent: "radar"},
	} {
		if err := st.SaveWorkspace(ws); err != nil {
			t.Fatal(err)
		}
	}
	tsrv := &tmux.Server{Socket: "ccmux-loadouts-wiring-test"}
	t.Cleanup(func() { _ = tsrv.KillServer() })
	mgr := manager.New(context.Background(), tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Skipf("no tmux to start a manager with: %v", err)
	}
	if repos := repoSessions(mgr); len(repos) != 1 || repos[0].WorkspaceID != "ws" || repos[0].Path != repo {
		t.Fatalf("the repo sessions, never an agent's: %+v", repos)
	}
	agents := agent.NewStore(filepath.Join(root, "ccmux", "agents"))
	svc := wireLoadouts(runnerWiring{st: st, agents: agents, api: api.NewServer(mgr), mgr: mgr})

	if seeded, _ := st.CapabilityChoicesSeeded(); !seeded {
		t.Fatal("the day-one choices were not written at the daemon's start")
	}
	file, record, err := svc.ForSession(store.RunnerSession{ID: "s", Cwd: repo, WorkspaceID: "ws", Wanted: store.SessionRunning})
	if err != nil {
		t.Fatal(err)
	}
	var lo agent.Loadout
	if err := json.Unmarshal(file, &lo); err != nil || !lo.EnabledPlugins["qa@personal"] || record == "" {
		t.Fatalf("the repo's session starts with the plugin its settings switch on: %s %v", file, err)
	}
	if users, _ := agents.CapabilityUsers("qa"); len(users) != 1 || users[0] != "repo app" {
		t.Fatalf("the catalog hears who has an entry on: %v", users)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
