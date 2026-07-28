// Package store is ccmuxd's durable registry: the record that lets a workspace
// survive a daemon restart or a tmux-server death and be revived. tmux is the
// live session store; this is the resurrection recipe.
package store

import (
	"database/sql"
	"errors"
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
	SetWorkspaceHostnames(id, hostnamesJSON string) error
	SetWorkspaceDevCommand(id, cmd string) error
	Load() ([]*model.Workspace, error)

	// Push notification subscriptions (transport-generic, keyed by login).
	SavePushSubscription(*model.PushSubscription) error
	ListPushSubscriptions() ([]*model.PushSubscription, error)
	DeletePushSubscription(id string) error

	// Small daemon-wide key/value settings (e.g. the default startup command).
	GetSetting(key string) (string, error)
	SetSetting(key, value string) error

	// Peers bus event log + per-peer delivery cursors (see model.PeerEvent).
	AppendPeerEvent(*model.PeerEvent) (int64, error)
	PeerEventsAfter(toID string, afterSeq int64) ([]*model.PeerEvent, error)
	PeerCursor(peerID string) (int64, error)
	AdvancePeerCursor(peerID string, seq int64) error
	RecentPeerSenders(toID string, sinceMillis int64) ([]string, error)
	PeerGroupMessages(group string, sinceMillis int64, limit int) ([]*model.PeerEvent, error)
	PrunePeerEvents(beforeMillis int64) (int64, error)

	io.Closer
}

// SQLite is a modernc.org/sqlite-backed Store (no cgo; cross-compiles to the
// future Linux host).
type SQLite struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS workspaces (
  id TEXT PRIMARY KEY, name TEXT, repo_path TEXT, created_by TEXT,
  created_at INTEGER, tmux_session TEXT, status TEXT,
  layout_json TEXT, layout_version INTEGER, ws_group TEXT DEFAULT '',
  hostnames_json TEXT DEFAULT '', dev_command TEXT DEFAULT ''
);
CREATE TABLE IF NOT EXISTS panes (
  id TEXT PRIMARY KEY, workspace_id TEXT, title TEXT, cwd TEXT,
  startup_command TEXT, created_by TEXT, created_at INTEGER,
  status TEXT, attention TEXT, is_dev INTEGER DEFAULT 0,
  dormant INTEGER DEFAULT 0, hosted_claude INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS panes_by_ws ON panes(workspace_id);
CREATE TABLE IF NOT EXISTS push_subscriptions (
  id TEXT PRIMARY KEY, login TEXT, transport TEXT,
  address TEXT, prefs TEXT, created_at INTEGER
);
CREATE INDEX IF NOT EXISTS push_by_login ON push_subscriptions(login);
CREATE TABLE IF NOT EXISTS peer_events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT, from_id TEXT, from_name TEXT, from_summary TEXT, from_cwd TEXT,
  to_id TEXT, to_name TEXT, grp TEXT, text TEXT,
  request_id TEXT DEFAULT '', behavior TEXT DEFAULT '', sent_at INTEGER
);
CREATE INDEX IF NOT EXISTS peer_events_by_to ON peer_events(to_id, seq);
CREATE INDEX IF NOT EXISTS peer_events_by_grp ON peer_events(grp, seq);
CREATE TABLE IF NOT EXISTS peer_cursors (
  peer_id TEXT PRIMARY KEY, acked_seq INTEGER,
  pane_id TEXT DEFAULT '', updated_at INTEGER DEFAULT 0
);
CREATE TABLE IF NOT EXISTS pane_sessions (
  pane_id TEXT PRIMARY KEY, live_ids TEXT, last_activity INTEGER
);
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY, value TEXT
);`

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
	_, _ = db.Exec(`ALTER TABLE workspaces ADD COLUMN hostnames_json TEXT DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE workspaces ADD COLUMN dev_command TEXT DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE panes ADD COLUMN is_dev INTEGER DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE panes ADD COLUMN dormant INTEGER DEFAULT 0`)
	// hosted_claude is owned by the session hooks from here on, but existing
	// panes have never seen one. Seed them ONCE from the old startup-command
	// guess — inside the ADD COLUMN success branch so it runs on migration only —
	// so no pane silently loses its history the day this ships.
	if _, err := db.Exec(`ALTER TABLE panes ADD COLUMN hosted_claude INTEGER DEFAULT 0`); err == nil {
		_, _ = db.Exec(`UPDATE panes SET hosted_claude=1 WHERE startup_command LIKE 'claude%'`)
	}
	// Pre-mailbox registries have cursors with no record of what they hang off,
	// so nothing could ever garbage-collect them. The columns default empty;
	// every registration backfills its own row (see TouchPeerMailbox).
	_, _ = db.Exec(`ALTER TABLE peer_cursors ADD COLUMN pane_id TEXT DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE peer_cursors ADD COLUMN updated_at INTEGER DEFAULT 0`)
	return &SQLite{db: db}, nil
}

func (s *SQLite) SaveWorkspace(w *model.Workspace) error {
	_, err := s.db.Exec(`
INSERT INTO workspaces (id,name,repo_path,created_by,created_at,tmux_session,status,layout_json,layout_version,ws_group,hostnames_json,dev_command)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, repo_path=excluded.repo_path,
  tmux_session=excluded.tmux_session, status=excluded.status,
  layout_json=excluded.layout_json, layout_version=excluded.layout_version,
  ws_group=excluded.ws_group, hostnames_json=excluded.hostnames_json,
  dev_command=excluded.dev_command`,
		w.ID, w.Name, w.RepoPath, w.CreatedBy, w.CreatedAt, w.TmuxSession, w.Status, w.LayoutJSON, w.LayoutVersion, w.Group, model.MarshalHostnames(w.Hostnames), w.DevCommand)
	return err
}

func (s *SQLite) SavePane(p *model.Pane) error {
	_, err := s.db.Exec(`
INSERT INTO panes (id,workspace_id,title,cwd,startup_command,created_by,created_at,status,attention,is_dev,dormant,hosted_claude)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET title=excluded.title, cwd=excluded.cwd,
  startup_command=excluded.startup_command, status=excluded.status, attention=excluded.attention,
  is_dev=excluded.is_dev, dormant=excluded.dormant, hosted_claude=excluded.hosted_claude`,
		p.ID, p.WorkspaceID, p.Title, p.CWD, p.StartupCommand, p.CreatedBy, p.CreatedAt, p.Status, p.Attention, p.DevServer, p.Dormant, p.HostedClaude)
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

// GetSetting returns a stored setting ("" when unset).
func (s *SQLite) GetSetting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSetting stores (or replaces) a setting value.
func (s *SQLite) SetSetting(key, value string) error {
	_, err := s.db.Exec(`
INSERT INTO settings (key, value) VALUES (?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
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

func (s *SQLite) SetWorkspaceHostnames(id, hostnamesJSON string) error {
	_, err := s.db.Exec(`UPDATE workspaces SET hostnames_json=? WHERE id=?`, hostnamesJSON, id)
	return err
}

func (s *SQLite) SetWorkspaceDevCommand(id, cmd string) error {
	_, err := s.db.Exec(`UPDATE workspaces SET dev_command=? WHERE id=?`, cmd, id)
	return err
}

// Load returns all workspaces with their panes attached.
func (s *SQLite) Load() ([]*model.Workspace, error) {
	rows, err := s.db.Query(`SELECT id,name,repo_path,created_by,created_at,tmux_session,status,layout_json,layout_version,ws_group,hostnames_json,dev_command FROM workspaces`)
	if err != nil {
		return nil, err
	}
	byID := map[string]*model.Workspace{}
	var out []*model.Workspace
	for rows.Next() {
		w := &model.Workspace{}
		var hostnamesJSON string
		if err := rows.Scan(&w.ID, &w.Name, &w.RepoPath, &w.CreatedBy, &w.CreatedAt, &w.TmuxSession, &w.Status, &w.LayoutJSON, &w.LayoutVersion, &w.Group, &hostnamesJSON, &w.DevCommand); err != nil {
			rows.Close()
			return nil, err
		}
		w.Hostnames = model.UnmarshalHostnames(hostnamesJSON)
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
	rows, err := s.db.Query(`SELECT id,workspace_id,title,cwd,startup_command,created_by,created_at,status,attention,is_dev,dormant,hosted_claude FROM panes ORDER BY created_at`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		p := &model.Pane{}
		if err := rows.Scan(&p.ID, &p.WorkspaceID, &p.Title, &p.CWD, &p.StartupCommand, &p.CreatedBy, &p.CreatedAt, &p.Status, &p.Attention, &p.DevServer, &p.Dormant, &p.HostedClaude); err != nil {
			return err
		}
		if w := byID[p.WorkspaceID]; w != nil {
			w.Panes = append(w.Panes, p)
		}
	}
	return rows.Err()
}

func (s *SQLite) Close() error { return s.db.Close() }
