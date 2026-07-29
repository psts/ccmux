package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpen_DBIsOwnerOnly: the registry holds peer history + workspace state, so
// Open must leave it mode 0600 (not SQLite's default 0644).
func TestOpen_DBIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("db perms = %o, want 0600", perm)
	}
}
