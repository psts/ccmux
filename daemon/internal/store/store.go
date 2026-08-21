// Package store is ccmuxd's durable registry: the record that lets a workspace
// survive a daemon restart or a tmux-server death and be revived. tmux is the
// live session store; this is the resurrection recipe.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

// Store persists workspaces and their panes.
type Store interface {
	SaveWorkspace(*model.Workspace) error
	SavePane(*model.Pane) error
	UpdatePaneSize(paneID string, cols, rows int) error
	DeleteWorkspace(id string) error
	DeletePane(id string) error
	SetWorkspaceStatus(id string, status model.Status) error
	SetWorkspaceGroup(id, group string) error
	SetWorkspaceHostnames(id, hostnamesJSON string) error
	SetWorkspaceDevCommand(id, cmd string) error
	Load() ([]*model.Workspace, error)

	// Per-user views: which window a given login keeps a workspace in. The
	// daemon a lens talks to (the hub in a federation, a lone daemon otherwise)
	// is the view authority; rows may reference remote workspace ids the local
	// manager has never heard of. See docs/multitenant-plan.md.
	SetView(login, wsID, window string) error
	AllViews() (map[string]map[string]string, error)
	DeleteWorkspaceViews(wsID string) error
	// The one-time legacy-group import per workspace: without the marker,
	// "no view rows" is indistinguishable from "the owner put it away", and
	// the import would resurrect a deliberately cleared arrangement.
	MarkViewImported(wsID string) error
	ViewImports() (map[string]bool, error)

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
  dormant INTEGER DEFAULT 0, hosted_claude INTEGER DEFAULT 0,
  cols INTEGER DEFAULT 0, rows INTEGER DEFAULT 0
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
-- Outstanding tool-approval relays. Held here rather than only in memory because
-- a dialog can sit open for hours: a daemon restart used to orphan every one of
-- them, and the delegator's "yes <id>" then stopped matching and arrived as
-- ordinary chat while the worker sat waiting.
CREATE TABLE IF NOT EXISTS peer_perm_requests (
  request_id TEXT PRIMARY KEY, worker_id TEXT, resolved INTEGER DEFAULT 0,
  created_at INTEGER
);
-- Cross-group reply licences. Same reason: a restart used to revoke the return
-- path mid-conversation, so a teammate reached into from another project got
-- "cannot send messages across projects" when it tried to answer.
CREATE TABLE IF NOT EXISTS peer_reply_grants (
  replier TEXT, sender TEXT, expires_at INTEGER,
  PRIMARY KEY (replier, sender)
);
-- Delegation tasks. Durable for the same reason as the relay tables: a
-- delegation routinely outlives the sessions on both ends, and the transcripts
-- show what in-memory-only tracking costs — silent stalls, chase-up pings, and
-- full-context re-sends. status runs sent → acked → working → completed|failed;
-- to_id may be '' while a spawned worker is still coming up, backfilled on its
-- first update_task.
CREATE TABLE IF NOT EXISTS peer_tasks (
  task_id TEXT PRIMARY KEY, from_id TEXT, to_id TEXT, grp TEXT,
  text TEXT, status TEXT, status_message TEXT DEFAULT '', result TEXT DEFAULT '',
  created_at INTEGER, updated_at INTEGER
);
CREATE INDEX IF NOT EXISTS peer_tasks_by_from ON peer_tasks(from_id, updated_at);
CREATE INDEX IF NOT EXISTS peer_tasks_by_to ON peer_tasks(to_id, updated_at);
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY, value TEXT
);
CREATE TABLE IF NOT EXISTS views (
  login TEXT, ws_id TEXT, window TEXT NOT NULL,
  updated_at INTEGER, PRIMARY KEY (login, ws_id)
);
CREATE TABLE IF NOT EXISTS view_imports (
  ws_id TEXT PRIMARY KEY
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
	// A pane's size used to be runtime-only, so a daemon restart forgot it and a
	// revive re-created every pane at 80x24 — the inner program then drew its
	// output at a width the pane no longer had. 0 means "never sized", which the
	// manager reads as "fall back to the default".
	//
	// Unlike the migrations above, only the expected error is swallowed. Every
	// other one (locked database, read-only file, disk full) leaves the column
	// missing, and the widened SELECT below then fails Load with "no such column:
	// cols" — sending whoever debugs it after a missing column rather than the
	// locked database that caused it.
	for _, col := range []string{"cols", "rows"} {
		if _, err := db.Exec(`ALTER TABLE panes ADD COLUMN ` + col + ` INTEGER DEFAULT 0`); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("migrate panes.%s: %w", col, err)
		}
	}
	// Pre-mailbox registries have cursors with no record of what they hang off,
	// so nothing could ever garbage-collect them. The columns default empty;
	// every registration backfills its own row (see TouchPeerMailbox).
	_, _ = db.Exec(`ALTER TABLE peer_cursors ADD COLUMN pane_id TEXT DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE peer_cursors ADD COLUMN updated_at INTEGER DEFAULT 0`)
	// The registry holds peer message history and workspace state; SQLite creates
	// it with the process umask (typically 0644), but the config dir is 0700 and
	// the peers secret/info files are 0600 (see internal/peers/token.go) — keep the
	// DB owner-only too. WAL sidecars inherit the loose mode, so tighten them
	// best-effort (they exist only after the first write in WAL mode).
	_ = os.Chmod(path, 0o600)
	_ = os.Chmod(path+"-wal", 0o600)
	_ = os.Chmod(path+"-shm", 0o600)
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

// SavePane upserts a pane. cols/rows are written on INSERT (a new pane's starting
// size) but deliberately NOT in the DO UPDATE set: callers build their `*p` copy
// under the lock and then write it after unrelated work, so a title or attention
// update carrying a stale size would put the old size back — and because memory
// stays correct, ResizePane's `changed` check is false from then on and nothing
// ever re-persists it. Size updates go through UpdatePaneSize instead.
func (s *SQLite) SavePane(p *model.Pane) error {
	_, err := s.db.Exec(`
INSERT INTO panes (id,workspace_id,title,cwd,startup_command,created_by,created_at,status,attention,is_dev,dormant,hosted_claude,cols,rows)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET title=excluded.title, cwd=excluded.cwd,
  startup_command=excluded.startup_command, status=excluded.status, attention=excluded.attention,
  is_dev=excluded.is_dev, dormant=excluded.dormant, hosted_claude=excluded.hosted_claude`,
		p.ID, p.WorkspaceID, p.Title, p.CWD, p.StartupCommand, p.CreatedBy, p.CreatedAt, p.Status, p.Attention, p.DevServer, p.Dormant, p.HostedClaude, p.Cols, p.Rows)
	return err
}

// UpdatePaneSize writes only the size, and only for a pane that still has a row.
// An UPDATE rather than an upsert on purpose: a resize can be in flight while the
// pane is closed, and an upsert would reinsert the row DeletePane just removed,
// leaving a phantom pane to be loaded on the next restart.
//
// Matching no row is reported as an error rather than passed off as success. Two
// different things land here — a pane closed mid-resize (expected) and a pane
// whose INSERT never happened (a bug) — and swallowing both means the second one
// persists nothing, ever, with no trace. The caller decides which it is.
func (s *SQLite) UpdatePaneSize(paneID string, cols, rows int) error {
	res, err := s.db.Exec(`UPDATE panes SET cols=?, rows=? WHERE id=?`, cols, rows, paneID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("pane %s has no registry row (closed, or its insert failed)", paneID)
	}
	return nil
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

// SetView upserts one login's window for a workspace; an empty window deletes
// the row (the login put the workspace away).
func (s *SQLite) SetView(login, wsID, window string) error {
	if window == "" {
		_, err := s.db.Exec(`DELETE FROM views WHERE login=? AND ws_id=?`, login, wsID)
		return err
	}
	_, err := s.db.Exec(`
INSERT INTO views (login, ws_id, window, updated_at) VALUES (?,?,?,?)
ON CONFLICT(login, ws_id) DO UPDATE SET window=excluded.window, updated_at=excluded.updated_at`,
		login, wsID, window, time.Now().UnixMilli())
	return err
}

// AllViews returns every view row, keyed workspace → login → window. One query:
// callers stamp whole workspace lists per request, so per-row lookups would be
// N queries on the list path.
func (s *SQLite) AllViews() (map[string]map[string]string, error) {
	rows, err := s.db.Query(`SELECT ws_id, login, window FROM views`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]string{}
	for rows.Next() {
		var ws, login, window string
		if err := rows.Scan(&ws, &login, &window); err != nil {
			return nil, err
		}
		if out[ws] == nil {
			out[ws] = map[string]string{}
		}
		out[ws][login] = window
	}
	return out, rows.Err()
}

// DeleteWorkspaceViews drops every login's row for a workspace (it was deleted),
// and its import marker with it — a future workspace can reuse nothing here.
func (s *SQLite) DeleteWorkspaceViews(wsID string) error {
	if _, err := s.db.Exec(`DELETE FROM view_imports WHERE ws_id=?`, wsID); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM views WHERE ws_id=?`, wsID)
	return err
}

// MarkViewImported records that a workspace's legacy group has been imported.
func (s *SQLite) MarkViewImported(wsID string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO view_imports (ws_id) VALUES (?)`, wsID)
	return err
}

// ViewImports returns the set of workspaces whose legacy group was imported.
func (s *SQLite) ViewImports() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT ws_id FROM view_imports`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			return nil, err
		}
		out[ws] = true
	}
	return out, rows.Err()
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
	rows, err := s.db.Query(`SELECT id,workspace_id,title,cwd,startup_command,created_by,created_at,status,attention,is_dev,dormant,hosted_claude,cols,rows FROM panes ORDER BY created_at`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		p := &model.Pane{}
		if err := rows.Scan(&p.ID, &p.WorkspaceID, &p.Title, &p.CWD, &p.StartupCommand, &p.CreatedBy, &p.CreatedAt, &p.Status, &p.Attention, &p.DevServer, &p.Dormant, &p.HostedClaude, &p.Cols, &p.Rows); err != nil {
			return err
		}
		if w := byID[p.WorkspaceID]; w != nil {
			w.Panes = append(w.Panes, p)
		}
	}
	return rows.Err()
}

func (s *SQLite) Close() error { return s.db.Close() }
