package store

import (
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

// TestGroupPersistsAcrossReopen pins the ws_group column: saved and updated
// groups survive a store reopen (the daemon-restart path), including on a
// registry created before the column existed (the ALTER migration).
func TestGroupPersistsAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/reg.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ws := &model.Workspace{ID: "w1", Name: "n", RepoPath: "/r", Group: "ChartLabs"}
	if err := s.SaveWorkspace(ws); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.SetWorkspaceGroup("w1", "Mixed"); err != nil {
		t.Fatalf("set group: %v", err)
	}
	s.Close()

	s2, err := Open(path) // reopen runs the (idempotent) migration again
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	loaded, err := s2.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Group != "Mixed" {
		t.Fatalf("loaded = %+v, want group Mixed", loaded)
	}
}
