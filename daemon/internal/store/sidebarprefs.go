package store

import "database/sql"

// SidebarPref is one person's choice about one group (a window) on the
// thread page's sidebar: folded into its header line, and where it sits.
// Position is nil for a group the person never placed; it then keeps the
// page's default order, after the placed ones.
type SidebarPref struct {
	WindowID  string `json:"windowId"`
	Collapsed bool   `json:"collapsed"`
	Position  *int   `json:"position"`
}

// SetSidebarCollapsed folds or unfolds a group for one login, only while
// the window exists: the check and the write are one statement, and
// DeleteWindow removes the window before these rows, as with SetWindowSize.
// The group's position is kept.
func (s *SQLite) SetSidebarCollapsed(login, windowID string, collapsed bool) (bool, error) {
	res, err := s.db.Exec(`
INSERT INTO sidebar_prefs (login, window_id, collapsed)
SELECT ?, ?, ? WHERE EXISTS (SELECT 1 FROM windows WHERE id=?)
ON CONFLICT(login, window_id) DO UPDATE SET collapsed=excluded.collapsed`,
		login, windowID, boolInt(collapsed), windowID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetSidebarOrder places one login's groups in the order given, 0 first, in
// one transaction: every earlier place of that login is cleared, so a group
// left out of the list falls back to the default order. A window that does
// not exist (deleted by someone else since the page drew) is skipped. Fold
// state is kept.
func (s *SQLite) SetSidebarOrder(login string, windowIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE sidebar_prefs SET position=NULL WHERE login=?`, login); err != nil {
		return err
	}
	for i, id := range windowIDs {
		if err := placeGroup(tx, login, id, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func placeGroup(tx *sql.Tx, login, windowID string, position int) error {
	_, err := tx.Exec(`
INSERT INTO sidebar_prefs (login, window_id, position)
SELECT ?, ?, ? WHERE EXISTS (SELECT 1 FROM windows WHERE id=?)
ON CONFLICT(login, window_id) DO UPDATE SET position=excluded.position`,
		login, windowID, position, windowID)
	return err
}

// SidebarPrefs returns one login's choices, placed groups first in order.
func (s *SQLite) SidebarPrefs(login string) ([]SidebarPref, error) {
	rows, err := s.db.Query(`
SELECT window_id, collapsed, position FROM sidebar_prefs WHERE login=?
ORDER BY position IS NULL, position, window_id`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SidebarPref{}
	for rows.Next() {
		var p SidebarPref
		var pos sql.NullInt64
		if err := rows.Scan(&p.WindowID, &p.Collapsed, &pos); err != nil {
			return nil, err
		}
		if pos.Valid {
			n := int(pos.Int64)
			p.Position = &n
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
