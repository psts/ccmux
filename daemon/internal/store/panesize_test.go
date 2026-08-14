package store

import (
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

// A pane written before this column existed reloads as 0, which the manager reads
// as "never sized" and falls back to the default. It must not fail to scan.
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
