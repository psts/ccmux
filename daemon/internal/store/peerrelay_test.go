package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A registry from before question cards existed has no kind column. Its
// open permission dialogs must come back as permissions, or a "yes <id>"
// after the upgrade would match nothing and the worker would sit on the
// dialog; a row written after the upgrade keeps the kind it was given.
func TestPermRequest_PreMigrationRowIsPermission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE peer_perm_requests (request_id TEXT PRIMARY KEY, worker_id TEXT, resolved INTEGER DEFAULT 0, created_at INTEGER)`,
		`INSERT INTO peer_perm_requests (request_id, worker_id, resolved, created_at) VALUES ('abcde', 'w1', 0, 1000), ('bcdef', 'w1', 1, 900)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open pre-migration registry: %v", err)
	}
	defer st.Close()
	if err := st.SavePermRequest("cdefg", "w2", "question", false, 1100); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadPermRequests()
	if err != nil {
		t.Fatal(err)
	}
	if r := got["abcde"]; r.Kind != "permission" || r.Resolved || r.WorkerID != "w1" || r.CreatedAt != 1000 {
		t.Errorf("old open row = %+v, want an unresolved permission for w1", r)
	}
	if r := got["bcdef"]; r.Kind != "permission" || !r.Resolved {
		t.Errorf("old resolved row = %+v, want a resolved permission", r)
	}
	if r := got["cdefg"]; r.Kind != "question" || r.WorkerID != "w2" {
		t.Errorf("new row = %+v, want the question it was saved as", r)
	}
}
