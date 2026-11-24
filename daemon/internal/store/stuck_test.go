package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// A session the daemon gave up starting is marked stuck; whatever wants
// it next (a wake, a prompt's wake, a sleep) clears the mark.
func TestAParkedSessionIsStuckUntilItIsWantedAgain(t *testing.T) {
	st := openSessions(t)
	for _, id := range []string{"s1", "s2", "s3"} {
		if err := st.AddRunnerSession(RunnerSession{ID: id, Cwd: "/r/" + id, Wanted: SessionRunning, CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		if err := st.ParkRunnerSession(id, 2); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := st.RunnerSession("s1"); !got.Stuck || got.Wanted != SessionAsleep {
		t.Fatalf("parked: %+v", got)
	}
	if err := st.SetRunnerSessionWanted("s1", SessionRunning, 3); err != nil {
		t.Fatal(err)
	}
	if err := st.AddInboxPromptAndWake(InboxPrompt{ID: "p1", SessionID: "s2", Text: "hi", CreatedAt: 3}, true, 3); err != nil {
		t.Fatal(err)
	}
	if err := st.SleepRunnerSession("s3", false, 3); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"s1", "s2", "s3"} {
		if got, _ := st.RunnerSession(id); got.Stuck {
			t.Errorf("%s still stuck: %+v", id, got)
		}
	}
}

// A row parked before the mark existed is marked by the migration, from
// the note every park writes.
func TestTheMigrationMarksSessionsParkedBefore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for id, note := range map[string]string{"parked": "put to sleep: it failed to start 5 times", "slept": "", "running": "put to sleep: old"} {
		wanted := SessionAsleep
		if id == "running" {
			wanted = SessionRunning
		}
		if err := st.AddRunnerSession(RunnerSession{ID: id, Cwd: "/r/" + id, Wanted: wanted, Note: note, CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE sessions DROP COLUMN stuck`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for id, want := range map[string]bool{"parked": true, "slept": false, "running": false} {
		if got, _ := st.RunnerSession(id); got.Stuck != want {
			t.Errorf("%s: stuck %v, want %v", id, got.Stuck, want)
		}
	}
}
