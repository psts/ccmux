// Package store is ccmuxd's durable registry: the record that lets a workspace
// survive a daemon restart or a tmux-server death and be revived. tmux is the
// live session store; this is the resurrection recipe.
package store

import (
	"database/sql"
	"fmt"
	"io"

	"ccmux.dev/ccmuxd/internal/model"
	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

// Store persists workspaces and their panes.
type Store interface {
	SaveWorkspace(*model.Workspace) error
	SavePane(*model.Pane) error
	DeleteWorkspace(id string) error
	DeletePane(id string) error
	SetWorkspaceStatus(id string, status model.Status) error
	SetWorkspaceGroup(id, group string) error
	Load() ([]*model.Workspace, error)

	// Push notification subscriptions (transport-generic, keyed by login).
	SavePushSubscription(*model.PushSubscription) error
	ListPushSubscriptions() ([]*model.PushSubscription, error)
	DeletePushSubscription(id string) error

	io.Closer
}

// SQLite is a modernc.org/sqlite-backed Store (no cgo; cross-compiles to the
// future Linux host).
type SQLite struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS workspaces (
  id TEXT PRIMARY KEY, name TEXT, repo_path TEXT, created_by TEXT,
  created_at INTEGER, tmux_session TEXT, status TEXT,
  layout_json TEXT, layout_version INTEGER, ws_group TEXT DEFAULT ''
);
CREATE TABLE IF NOT EXISTS panes (
  id TEXT PRIMARY KEY, workspace_id TEXT, title TEXT, cwd TEXT,
  startup_command TEXT, created_by TEXT, created_at INTEGER,
  status TEXT, attention TEXT
);
CREATE INDEX IF NOT EXISTS panes_by_ws ON panes(workspace_id);
CREATE TABLE IF NOT EXISTS push_subscriptions (
  id TEXT PRIMARY KEY, login TEXT, transport TEXT,
  address TEXT, prefs TEXT, created_at INTEGER
);
CREATE INDEX IF NOT EXISTS push_by_login ON push_subscriptions(login);`

// Open opens (creating if needed) the registry at path.
func Open(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	// Migration for pre-group registries: ADD COLUMN fails with "duplicate
	// column" once applied, so the error is deliberately ignored. DEFAULT ''
	// keeps existing rows scannable into a plain string.
	_, _ = db.Exec(`ALTER TABLE workspaces ADD COLUMN ws_group TEXT DEFAULT ''`)
	return &SQLite{db: db}, nil
}

func (s *SQLite) SaveWorkspace(w *model.Workspace) error {
	_, err := s.db.Exec(`
INSERT INTO workspaces (id,name,repo_path,created_by,created_at,tmux_session,status,layout_json,layout_version,ws_group)
VALUES (?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, repo_path=excluded.repo_path,
  tmux_session=excluded.tmux_session, status=excluded.status,
  layout_json=excluded.layout_json, layout_version=excluded.layout_version,
  ws_group=excluded.ws_group`,
		w.ID, w.Name, w.RepoPath, w.CreatedBy, w.CreatedAt, w.TmuxSession, w.Status, w.LayoutJSON, w.LayoutVersion, w.Group)
	return err
}

func (s *SQLite) SavePane(p *model.Pane) error {
	_, err := s.db.Exec(`
INSERT INTO panes (id,workspace_id,title,cwd,startup_command,created_by,created_at,status,attention)
VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET title=excluded.title, cwd=excluded.cwd,
  startup_command=excluded.startup_command, status=excluded.status, attention=excluded.attention`,
		p.ID, p.WorkspaceID, p.Title, p.CWD, p.StartupCommand, p.CreatedBy, p.CreatedAt, p.Status, p.Attention)
	return err
}

func (s *SQLite) DeleteWorkspace(id string) error {
	if _, err := s.db.Exec(`DELETE FROM panes WHERE workspace_id=?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM workspaces WHERE id=?`, id)
	return err
}

func (s *SQLite) DeletePane(id string) error {
	_, err := s.db.Exec(`DELETE FROM panes WHERE id=?`, id)
	return err
}

func (s *SQLite) SavePushSubscription(sub *model.PushSubscription) error {
	_, err := s.db.Exec(`
INSERT INTO push_subscriptions (id,login,transport,address,prefs,created_at)
VALUES (?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET login=excluded.login, transport=excluded.transport,
  address=excluded.address, prefs=excluded.prefs`,
		sub.ID, sub.Login, sub.Transport, sub.Address, sub.Prefs, sub.CreatedAt)
	return err
}

func (s *SQLite) ListPushSubscriptions() ([]*model.PushSubscription, error) {
	rows, err := s.db.Query(`SELECT id,login,transport,address,prefs,created_at FROM push_subscriptions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.PushSubscription
	for rows.Next() {
		sub := &model.PushSubscription{}
		if err := rows.Scan(&sub.ID, &sub.Login, &sub.Transport, &sub.Address, &sub.Prefs, &sub.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

func (s *SQLite) DeletePushSubscription(id string) error {
	_, err := s.db.Exec(`DELETE FROM push_subscriptions WHERE id=?`, id)
	return err
}

func (s *SQLite) SetWorkspaceStatus(id string, status model.Status) error {
	_, err := s.db.Exec(`UPDATE workspaces SET status=? WHERE id=?`, status, id)
	return err
}

func (s *SQLite) SetWorkspaceGroup(id, group string) error {
	_, err := s.db.Exec(`UPDATE workspaces SET ws_group=? WHERE id=?`, group, id)
	return err
}

// Load returns all workspaces with their panes attached.
func (s *SQLite) Load() ([]*model.Workspace, error) {
	rows, err := s.db.Query(`SELECT id,name,repo_path,created_by,created_at,tmux_session,status,layout_json,layout_version,ws_group FROM workspaces`)
	if err != nil {
		return nil, err
	}
	byID := map[string]*model.Workspace{}
	var out []*model.Workspace
	for rows.Next() {
		w := &model.Workspace{}
		if err := rows.Scan(&w.ID, &w.Name, &w.RepoPath, &w.CreatedBy, &w.CreatedAt, &w.TmuxSession, &w.Status, &w.LayoutJSON, &w.LayoutVersion, &w.Group); err != nil {
			rows.Close()
			return nil, err
		}
		byID[w.ID] = w
		out = append(out, w)
	}
	rows.Close()
	if err := s.attachPanes(byID); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *SQLite) attachPanes(byID map[string]*model.Workspace) error {
	rows, err := s.db.Query(`SELECT id,workspace_id,title,cwd,startup_command,created_by,created_at,status,attention FROM panes ORDER BY created_at`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		p := &model.Pane{}
		if err := rows.Scan(&p.ID, &p.WorkspaceID, &p.Title, &p.CWD, &p.StartupCommand, &p.CreatedBy, &p.CreatedAt, &p.Status, &p.Attention); err != nil {
			return err
		}
		if w := byID[p.WorkspaceID]; w != nil {
			w.Panes = append(w.Panes, p)
		}
	}
	return rows.Err()
}

func (s *SQLite) Close() error { return s.db.Close() }
