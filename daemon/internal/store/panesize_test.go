package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

// A pane's size has to survive the registry, or a daemon restart forgets how wide
// every pane was and the next revive rebuilds them all at 80x24.
func TestPaneSize_SurvivesAReload(t *testing.T) {
	st := openTestStore(t)

	ws := &model.Workspace{ID: "ws-1", Name: "w", RepoPath: "/tmp", TmuxSession: "s", Status: model.StatusLive}
	if err := st.SaveWorkspace(ws); err != nil {
		t.Fatalf("save workspace: %v", err)
	}
	p := &model.Pane{ID: "p-1", WorkspaceID: ws.ID, CWD: "/tmp", Status: model.StatusLive, Cols: 137, Rows: 42}
	if err := st.SavePane(p); err != nil {
		t.Fatalf("save pane: %v", err)
	}

	loaded := loadPane(t, st, "ws-1", "p-1")
	if loaded.Cols != 137 || loaded.Rows != 42 {
		t.Fatalf("reloaded size = %dx%d, want 137x42", loaded.Cols, loaded.Rows)
	}

	// A resize is an upsert on an existing row, not an insert.
	p.Cols, p.Rows = 96, 30
	if err := st.SavePane(p); err != nil {
		t.Fatalf("resave pane: %v", err)
	}
	loaded = loadPane(t, st, "ws-1", "p-1")
	if loaded.Cols != 96 || loaded.Rows != 30 {
		t.Fatalf("after upsert size = %dx%d, want 96x30", loaded.Cols, loaded.Rows)
	}
}

// A pane saved through the new schema without a size reloads as 0, which the
// manager reads as "never sized" and falls back to the default.
func TestPaneSize_UnsizedPaneReloadsAsZero(t *testing.T) {
	st := openTestStore(t)

	ws := &model.Workspace{ID: "ws-2", Name: "w", RepoPath: "/tmp", TmuxSession: "s", Status: model.StatusLive}
	if err := st.SaveWorkspace(ws); err != nil {
		t.Fatalf("save workspace: %v", err)
	}
	if err := st.SavePane(&model.Pane{ID: "p-2", WorkspaceID: ws.ID, CWD: "/tmp", Status: model.StatusLive}); err != nil {
		t.Fatalf("save pane: %v", err)
	}

	loaded := loadPane(t, st, "ws-2", "p-2")
	if loaded.Cols != 0 || loaded.Rows != 0 {
		t.Fatalf("unsized pane = %dx%d, want 0x0", loaded.Cols, loaded.Rows)
	}
}

// The real upgrade: a registry written by a daemon that had no cols/rows columns
// at all. Every install on the fleet is one of these. If the ALTER did not apply,
// the widened SELECT fails and Load returns an error, which makes the daemon come
// up with no workspaces — so this path needs a test that does not go through the
// current CREATE TABLE.
func TestPaneSize_PreMigrationRegistryUpgrades(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	writePreMigrationRegistry(t, path)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open pre-migration registry: %v", err)
	}
	defer st.Close()

	loaded := loadPane(t, st, "ws-old", "p-old")
	if loaded.Cols != 0 || loaded.Rows != 0 {
		t.Fatalf("migrated pane = %dx%d, want 0x0", loaded.Cols, loaded.Rows)
	}
	// The row survived intact, not just its new columns.
	if loaded.CWD != "/tmp/old" || loaded.Title != "old pane" {
		t.Fatalf("migrated pane lost fields: cwd=%q title=%q", loaded.CWD, loaded.Title)
	}

	// And the upgraded table still round-trips a size.
	loaded.Cols, loaded.Rows = 137, 42
	if err := st.SavePane(loaded); err != nil {
		t.Fatalf("save into migrated table: %v", err)
	}
	again := loadPane(t, st, "ws-old", "p-old")
	if again.Cols != 137 || again.Rows != 42 {
		t.Fatalf("after migration size = %dx%d, want 137x42", again.Cols, again.Rows)
	}
}

// writePreMigrationRegistry creates a registry with the panes table as it stood
// before cols/rows existed — twelve columns, no size.
func writePreMigrationRegistry(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE workspaces (
  id TEXT PRIMARY KEY, name TEXT, repo_path TEXT, created_by TEXT,
  created_at INTEGER, tmux_session TEXT, status TEXT,
  layout_json TEXT, layout_version INTEGER, ws_group TEXT DEFAULT '',
  hostnames_json TEXT DEFAULT '', dev_command TEXT DEFAULT ''
)`,
		`CREATE TABLE panes (
  id TEXT PRIMARY KEY, workspace_id TEXT, title TEXT, cwd TEXT,
  startup_command TEXT, created_by TEXT, created_at INTEGER,
  status TEXT, attention TEXT, is_dev INTEGER DEFAULT 0,
  dormant INTEGER DEFAULT 0, hosted_claude INTEGER DEFAULT 0
)`,
		`INSERT INTO workspaces (id,name,repo_path,created_by,created_at,tmux_session,status,layout_json,layout_version)
 VALUES ('ws-old','old','/tmp/old','tester',1,'ccmux-old','live','',0)`,
		`INSERT INTO panes (id,workspace_id,title,cwd,startup_command,created_by,created_at,status,attention,is_dev,dormant,hosted_claude)
 VALUES ('p-old','ws-old','old pane','/tmp/old','',' tester',1,'live','idle',0,0,0)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed pre-migration registry: %v", err)
		}
	}
}

func loadPane(t *testing.T, st *SQLite, wsID, paneID string) *model.Pane {
	t.Helper()
	all, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, w := range all {
		if w.ID != wsID {
			continue
		}
		for _, p := range w.Panes {
			if p.ID == paneID {
				return p
			}
		}
	}
	t.Fatalf("pane %s not found in workspace %s", paneID, wsID)
	return nil
}
