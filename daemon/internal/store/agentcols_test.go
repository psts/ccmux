package store

import (
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

// The agent columns survive a reopen and the idempotent migration tolerates
// re-running on a registry that already has them.
func TestAgentColumnsPersistAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/reg.db"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveWorkspace(&model.Workspace{ID: "w1", Name: "p", RepoPath: "/r", Agent: "x-poster"}); err != nil {
		t.Fatal(err)
	}
	p := &model.Pane{ID: "p1", WorkspaceID: "w1", Harness: "opencode", Agent: "x-poster", AgentVersion: "1.0.2"}
	if err := s.SavePane(p); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	wss, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(wss) != 1 || len(wss[0].Panes) != 1 || wss[0].Agent != "x-poster" {
		t.Fatalf("load: %+v", wss)
	}
	got := wss[0].Panes[0]
	if got.Agent != "x-poster" || got.AgentVersion != "1.0.2" || got.Harness != "opencode" {
		t.Fatalf("pane after reopen: %+v", got)
	}
	// Clearing the stamp round-trips too (an agent pane turned back into a plain one).
	got.Agent, got.AgentVersion = "", ""
	if err := s.SavePane(got); err != nil {
		t.Fatal(err)
	}
	wss, _ = s.Load()
	if wss[0].Panes[0].Agent != "" {
		t.Fatal("clear did not persist")
	}
}

// A stamped pane inside an ordinary session (the per-repo model) loses its
// stamp on open; a pane inside an agent session keeps it.
func TestLegacyAgentPanesBecomePlainOnOpen(t *testing.T) {
	path := t.TempDir() + "/reg.db"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.SaveWorkspace(&model.Workspace{ID: "repo", Name: "backend", RepoPath: "/r"})
	s.SaveWorkspace(&model.Workspace{ID: "agent", Name: "🐦 x-poster", RepoPath: "/w/agents/x-poster", Agent: "x-poster"})
	s.SavePane(&model.Pane{ID: "p-legacy", WorkspaceID: "repo", Agent: "x-poster", AgentVersion: "1.0.0"})
	s.SavePane(&model.Pane{ID: "p-agent", WorkspaceID: "agent", Agent: "x-poster", AgentVersion: "1.0.0"})
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	wss, _ := s.Load()
	for _, w := range wss {
		p := w.Panes[0]
		if w.ID == "repo" && (p.Agent != "" || p.AgentVersion != "") {
			t.Fatalf("legacy pane kept its stamp: %+v", p)
		}
		if w.ID == "agent" && p.Agent != "x-poster" {
			t.Fatalf("agent session pane lost its stamp: %+v", p)
		}
	}
}
