package llmserve

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

// daemonDB is a database made the way the daemon makes it (store.Open: the
// real schema, WAL), with the daemon's handle still open on it, and a Reader
// on the same file.
func daemonDB(t *testing.T) (*store.SQLite, *Reader, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ccmuxd.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, reader(t, path), path
}

func reader(t *testing.T, path string) *Reader {
	t.Helper()
	r, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func setting(t *testing.T, r *Reader, key string) string {
	t.Helper()
	v, err := r.GetSetting(key)
	if err != nil {
		t.Fatalf("GetSetting(%q): %v", key, err)
	}
	return v
}

// The Reader sees what the daemon writes, as it writes it, and a key nobody
// set reads as "" (the contract llmproxy and harness are written against).
func TestReaderFollowsTheDaemonsWrites(t *testing.T) {
	st, r, _ := daemonDB(t)
	if got := setting(t, r, "llm_route"); got != "" {
		t.Fatalf("unset key = %q, want empty", got)
	}
	for _, v := range []string{"first", "second"} {
		if err := st.SetSetting("llm_route", v); err != nil {
			t.Fatal(err)
		}
		if got := setting(t, r, "llm_route"); got != v {
			t.Fatalf("after the daemon wrote %q the reader saw %q", v, got)
		}
	}
}

// The service never writes: the daemon is the database's only writer.
func TestReaderRefusesToWrite(t *testing.T) {
	st, r, _ := daemonDB(t)
	if err := r.SetSetting("llm_route", "x"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("SetSetting = %v, want ErrReadOnly", err)
	}
	// And the connection itself cannot, whatever the code above it does.
	if _, err := r.db.Exec(`INSERT INTO settings (key, value) VALUES ('k','v')`); err == nil {
		t.Fatal("a write through the reader's connection succeeded; it must be read only")
	}
	if v, _ := st.GetSetting("k"); v != "" {
		t.Fatalf("the refused write landed: %q", v)
	}
}

// The service starts at boot, maybe before the daemon ever made its
// database. It must not fail to start: it answers "unreadable" per request,
// and reads fine once the file is there.
func TestReaderOutlivesAMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-yet", "ccmuxd.db")
	r := reader(t, path)
	if _, err := r.GetSetting("llm_route"); err == nil {
		t.Fatal("a read of a database that does not exist succeeded")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the reader created the database; only the daemon may")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSetting("llm_route", "max"); err != nil {
		t.Fatal(err)
	}
	if got := setting(t, r, "llm_route"); got != "max" {
		t.Fatalf("once the daemon made the database the reader saw %q, want max", got)
	}
}

// The cold case: the daemon died without closing (kill -9, a reboot), so its
// last writes are still in the -wal file, and no writer is attached when the
// service opens the database. A read-only open must still see them.
func TestReaderReadsADatabaseTheDaemonLeftOpen(t *testing.T) {
	st, _, path := daemonDB(t)
	if err := st.SetSetting("llm_route", "written-before-the-crash"); err != nil {
		t.Fatal(err)
	}
	// Copied while the daemon's handle is open: the write above is in the
	// -wal, not yet in the main file. The -shm is left behind, as after a
	// reboot (it is only an index, rebuilt from the -wal).
	cold := filepath.Join(t.TempDir(), "ccmuxd.db")
	for _, suffix := range []string{"", "-wal"} {
		raw, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatalf("the daemon's database has no %q file to copy: %v", suffix, err)
		}
		if err := os.WriteFile(cold+suffix, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := setting(t, reader(t, cold), "llm_route"); got != "written-before-the-crash" {
		t.Fatalf("cold read = %q, want the value still in the -wal", got)
	}
	// The test only means something if the value really was in the -wal: the
	// main file alone must not have it.
	bare := filepath.Join(t.TempDir(), "ccmuxd.db")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bare, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := reader(t, bare).GetSetting("llm_route"); got == "written-before-the-crash" {
		t.Fatal("the value was already in the main file, so this test read no -wal at all")
	}
}

// HarnessForPane must answer what the manager's memory answers
// (manager.HarnessForPane), from the rows that memory is built from. One row
// per kind of pane the manager knows.
func TestHarnessForPane(t *testing.T) {
	st, r, _ := daemonDB(t)
	ws := &model.Workspace{ID: "ws1", Name: "repo", RepoPath: "/tmp/repo", TmuxSession: "s1", Status: model.StatusLive}
	if err := st.SaveWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*model.Pane{
		{ID: "codex-pane", WorkspaceID: "ws1", Harness: "codex"},
		{ID: "shell-pane", WorkspaceID: "ws1"},
	} {
		if err := st.SavePane(p); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []store.RunnerSession{
		{ID: "s-live", Cwd: "/tmp/repo", Wanted: store.SessionRunning, PaneID: "runner-live", WorkspaceID: "ws1"},
		{ID: "s-asleep", Cwd: "/tmp/repo", Wanted: store.SessionAsleep, PaneID: "runner-asleep", WorkspaceID: "ws1"},
		{ID: "s-done", Cwd: "/tmp/repo", Wanted: store.SessionDone, PaneID: "runner-done", WorkspaceID: "ws1"},
		{ID: "s-orphan", Cwd: "/tmp/gone", Wanted: store.SessionRunning, PaneID: "runner-orphan", WorkspaceID: "ws-deleted"},
		{ID: "s-paneless", Cwd: "/tmp/repo", Wanted: store.SessionRunning, WorkspaceID: "ws1"},
	} {
		if err := st.AddRunnerSession(s); err != nil {
			t.Fatal(err)
		}
	}
	for pane, want := range map[string]string{
		"codex-pane":    "codex",         // a tmux pane: its own column
		"shell-pane":    "",              // a bare shell: a row, no harness
		"runner-live":   harness.Builtin, // a runner session always runs claude
		"runner-asleep": harness.Builtin, // asleep is not ended: the pane stays
		"runner-done":   "",              // an ended session's pane is gone
		"runner-orphan": "",              // its workspace was deleted: no pane
		"never-existed": "",
		"":              "", // a session with no pane id must not match the empty id
	} {
		got, err := r.HarnessForPane(pane)
		if err != nil {
			t.Fatalf("HarnessForPane(%q): %v", pane, err)
		}
		if got != want {
			t.Errorf("HarnessForPane(%q) = %q, want %q", pane, got, want)
		}
	}
}
