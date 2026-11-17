// Skills and plugins per group and repo: what a thread session loads from
// the capability catalog (agent/capabilities.go). A group (a shared window)
// switches a catalog entry on; a repo (a workspace) follows its group, or
// switches one entry on or off for itself. No row is "off" for a group and
// "as the group" for a repo, so a new group or repo starts with nothing on.
// internal/loadout owns the rules.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const capChoicesSchema = `
-- One row per choice. scope is window or workspace, scope_id its id, cap_id
-- the catalog entry's id, state on or off (a window keeps on rows only).
CREATE TABLE IF NOT EXISTS capability_choices (
  scope TEXT NOT NULL, scope_id TEXT NOT NULL, cap_id TEXT NOT NULL, state TEXT NOT NULL,
  PRIMARY KEY (scope, scope_id, cap_id)
);
CREATE INDEX IF NOT EXISTS capability_choices_by_cap ON capability_choices(cap_id);
`

// The scopes and states of a choice.
const (
	ChoiceWindow    = "window"
	ChoiceWorkspace = "workspace"
	ChoiceOn        = "on"
	ChoiceOff       = "off"
)

// capChoicesSeededKey is the settings row that says the day-one choices
// were written (SeedCapabilityChoices), holding when.
const capChoicesSeededKey = "capability_choices_seeded"

// CapChoice is one row of capability_choices.
type CapChoice struct {
	Scope   string `json:"scope"`
	ScopeID string `json:"scopeId"`
	CapID   string `json:"capId"`
	State   string `json:"state"`
}

// migrateSessionLoadout gives a session row its loadout: what it loaded
// from the catalog at its last start, as internal/loadout's record (JSON,
// ” for a session that never started with one). Strict, as
// migrateSessionPanes is: sessionCols names the column.
func migrateSessionLoadout(db *sql.DB) error {
	_, err := db.Exec(`ALTER TABLE sessions ADD COLUMN loadout TEXT DEFAULT ''`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("migrate sessions.loadout: %w", err)
	}
	return nil
}

// SetRunnerSessionLoadout records what a session loaded at the start being
// made now. It is not a change a lens shows as activity, so updated_at
// stays.
func (s *SQLite) SetRunnerSessionLoadout(id, loadout string) error {
	return s.updateSession(`UPDATE sessions SET loadout=? WHERE id=?`, loadout, id)
}

// SetCapabilityChoice writes one choice; state "" removes it (off for a
// window, "as the group" for a workspace).
func (s *SQLite) SetCapabilityChoice(c CapChoice) error {
	if c.State == "" {
		_, err := s.db.Exec(`DELETE FROM capability_choices WHERE scope=? AND scope_id=? AND cap_id=?`, c.Scope, c.ScopeID, c.CapID)
		return err
	}
	_, err := s.db.Exec(`
INSERT INTO capability_choices (scope, scope_id, cap_id, state) VALUES (?,?,?,?)
ON CONFLICT(scope, scope_id, cap_id) DO UPDATE SET state=excluded.state`, c.Scope, c.ScopeID, c.CapID, c.State)
	return err
}

// CapabilityChoices is every choice, ordered so a reader gets the same list
// each time.
func (s *SQLite) CapabilityChoices() ([]CapChoice, error) {
	return s.queryChoices(`SELECT scope, scope_id, cap_id, state FROM capability_choices ORDER BY scope, scope_id, cap_id`)
}

// CapabilityChoicesOf is one window's or workspace's choices, cap id →
// state.
func (s *SQLite) CapabilityChoicesOf(scope, scopeID string) (map[string]string, error) {
	list, err := s.queryChoices(`SELECT scope, scope_id, cap_id, state FROM capability_choices WHERE scope=? AND scope_id=?`, scope, scopeID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list))
	for _, c := range list {
		out[c.CapID] = c.State
	}
	return out, nil
}

func (s *SQLite) queryChoices(q string, args ...any) ([]CapChoice, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CapChoice{}
	for rows.Next() {
		var c CapChoice
		if err := rows.Scan(&c.Scope, &c.ScopeID, &c.CapID, &c.State); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CapabilityChoiceUsers names who has catalog entry capID switched on, one
// label per group or repo ("group ChartLabs", "repo app"), sorted. An "off"
// is not a use: it asks for nothing to be loaded.
func (s *SQLite) CapabilityChoiceUsers(capID string) ([]string, error) {
	rows, err := s.db.Query(`
SELECT CASE c.scope WHEN 'window' THEN 'group ' || COALESCE(w.name, c.scope_id)
                    ELSE 'repo ' || COALESCE(ws.name, c.scope_id) END AS label
FROM capability_choices c
LEFT JOIN windows w ON c.scope='window' AND w.id=c.scope_id
LEFT JOIN workspaces ws ON c.scope='workspace' AND ws.id=c.scope_id
WHERE c.cap_id=? AND c.state='on' ORDER BY label`, capID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, err
		}
		out = append(out, label)
	}
	return out, rows.Err()
}

// CapabilityChoicesSeeded says whether the day-one choices were written.
func (s *SQLite) CapabilityChoicesSeeded() (bool, error) {
	v, err := s.GetSetting(capChoicesSeededKey)
	return v != "", err
}

// SeedCapabilityChoices writes the day-one choices and the mark that they
// were written, in one transaction, and says whether it did. Once the mark
// is there it writes nothing: what people chose since is theirs. A row
// already there is kept too, for the same reason.
func (s *SQLite) SeedCapabilityChoices(rows []CapChoice, now int64) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var mark string
	err = tx.QueryRow(`SELECT value FROM settings WHERE key=?`, capChoicesSeededKey).Scan(&mark)
	if err == nil && mark != "" {
		return false, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	for _, c := range rows {
		if _, err := tx.Exec(`INSERT INTO capability_choices (scope, scope_id, cap_id, state) VALUES (?,?,?,?)
ON CONFLICT(scope, scope_id, cap_id) DO NOTHING`, c.Scope, c.ScopeID, c.CapID, c.State); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, capChoicesSeededKey, fmt.Sprint(now)); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// deleteChoicesOf drops a window's or workspace's choices with it, so a
// choice of something gone never keeps a catalog entry "in use".
func (s *SQLite) deleteChoicesOf(scope, scopeID string) error {
	_, err := s.db.Exec(`DELETE FROM capability_choices WHERE scope=? AND scope_id=?`, scope, scopeID)
	return err
}
