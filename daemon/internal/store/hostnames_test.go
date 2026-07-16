package store

import (
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

// TestHostnamesPersistAcrossReopen pins the hostnames_json column: mappings
// survive a store reopen (daemon restart), runtime-only fields (URL/Listening)
// are NOT persisted, and the idempotent ALTER migration tolerates re-running.
func TestHostnamesPersistAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/reg.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ws := &model.Workspace{ID: "w1", Name: "chartlabs", RepoPath: "/r"}
	if err := s.SaveWorkspace(ws); err != nil {
		t.Fatalf("save: %v", err)
	}
	hs := []model.Hostname{
		{Name: "chartlabs-app", Port: 3001, URL: "https://runtime.only", Listening: true},
		{Name: "chartlabs-api", Port: 3002},
	}
	if err := s.SetWorkspaceHostnames("w1", model.MarshalHostnames(hs)); err != nil {
		t.Fatalf("set hostnames: %v", err)
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
	if len(loaded) != 1 || len(loaded[0].Hostnames) != 2 {
		t.Fatalf("loaded = %+v, want 1 workspace with 2 hostnames", loaded)
	}
	got := loaded[0].Hostnames[0]
	if got.Name != "chartlabs-app" || got.Port != 3001 {
		t.Fatalf("hostname[0] = %+v, want chartlabs-app:3001", got)
	}
	if got.URL != "" || got.Listening {
		t.Fatalf("runtime fields persisted: %+v", got)
	}
}

// TestHostnamesEmptyAndMalformed pins the degenerate blobs: no mappings store
// as "" and load as none; a corrupt blob loads as none instead of failing Load.
func TestHostnamesEmptyAndMalformed(t *testing.T) {
	if got := model.MarshalHostnames(nil); got != "" {
		t.Fatalf("marshal(nil) = %q, want empty", got)
	}
	if got := model.UnmarshalHostnames("not json"); got != nil {
		t.Fatalf("unmarshal(garbage) = %v, want nil", got)
	}
	s, err := Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if err := s.SaveWorkspace(&model.Workspace{ID: "w1", Name: "n", RepoPath: "/r"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := s.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Hostnames != nil {
		t.Fatalf("loaded = %+v, want no hostnames", loaded[0])
	}
}
