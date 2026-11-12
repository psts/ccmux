package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// What a part's Claude starts with is saved with the part on every way in:
// filed with its feature, added to a running one, and saved by the runner.
// A part filed without one reads back empty, not as another part's.
func TestParts_ModelAndEffortRoundTrip(t *testing.T) {
	st := taskStore(t)
	id, err := st.AddTaskWithParts(Task{Title: "two", Status: "waiting"}, []Part{
		{RepoPath: "/r/app", Plan: "a", Status: "pending", Model: "fable", Effort: "high"},
		{RepoPath: "/r/api", Plan: "b", Status: "pending"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddPart(Part{TaskID: id, RepoPath: "/r/web", Plan: "c", Status: "pending", Model: "opus", Effort: "medium"}); err != nil {
		t.Fatal(err)
	}
	parts, err := st.Parts()
	if err != nil || len(parts) != 3 {
		t.Fatalf("parts = %+v, %v", parts, err)
	}
	if parts[0].Model != "fable" || parts[0].Effort != "high" || parts[1].Model != "" || parts[1].Effort != "" ||
		parts[2].Model != "opus" || parts[2].Effort != "medium" {
		t.Fatalf("filed parts read back as %+v", parts)
	}
	parts[1].Model, parts[1].Effort = "opus", "medium"
	if ok, err := st.SavePart(parts[1]); err != nil || !ok {
		t.Fatalf("save = %v, %v", ok, err)
	}
	if again, _ := st.Parts(); again[1].Model != "opus" || again[1].Effort != "medium" || again[0].Model != "fable" {
		t.Fatalf("after a save = %+v", again)
	}
}

// A registry from before parts had a model opens, and its parts read back
// with none: every part read selects the columns, so a missed migration
// would stop the whole queue.
func TestParts_PreMigrationPartHasNoModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE task_parts (id INTEGER PRIMARY KEY AUTOINCREMENT, task_id INTEGER NOT NULL,
			repo_path TEXT NOT NULL, plan TEXT NOT NULL, status TEXT NOT NULL, note TEXT DEFAULT '', filed_by TEXT DEFAULT '',
			branch TEXT DEFAULT '', worktree TEXT DEFAULT '', pane_id TEXT DEFAULT '', created_at INTEGER, updated_at INTEGER,
			started_at INTEGER DEFAULT 0, finished_at INTEGER DEFAULT 0, UNIQUE (task_id, repo_path))`,
		`INSERT INTO task_parts (task_id, repo_path, plan, status, created_at, updated_at) VALUES (1, '/r/app', 'old', 'ready', 1, 1)`,
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
	parts, err := st.Parts()
	if err != nil || len(parts) != 1 || parts[0].Plan != "old" || parts[0].Model != "" || parts[0].Effort != "" {
		t.Fatalf("old part = %+v, %v", parts, err)
	}
}
