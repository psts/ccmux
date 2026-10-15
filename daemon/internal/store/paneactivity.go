package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// PaneActivity is an agent pane's last lifecycle reading: what the harness
// was doing ("busy", "idle", "waiting") and since when, in unix milliseconds.
// It is what the idle exit counts from, so it has to outlive the daemon: kept
// only in memory, a restart forgot every idle clock and the lifecycle fell
// back to guessing from the pane's attention, which a human's look rewrites.
type PaneActivity struct {
	State string
	Since int64
}

// migratePaneActivity adds the two activity columns. Strict like the other
// pane columns: PaneActivities reads both. An existing pane reads as no
// record, which the lifecycle treats as idle from the moment it first looks.
func migratePaneActivity(db *sql.DB) error {
	for _, col := range []string{"agent_state TEXT DEFAULT ''", "agent_state_since INTEGER DEFAULT 0"} {
		_, err := db.Exec(`ALTER TABLE panes ADD COLUMN ` + col)
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate panes.%s: %w", strings.Fields(col)[0], err)
		}
	}
	return nil
}

// UpdatePaneActivity writes only the activity, for a pane that still has a
// row. Its own writer and not a SavePane field, for the reason UpdatePaneSize
// gives: SavePane callers write a snapshot taken earlier, and a stale one
// would put an old busy reading back under a newer idle one. An UPDATE, never
// an upsert, so a reading racing a close cannot resurrect the row. "" clears.
func (s *SQLite) UpdatePaneActivity(paneID, state string, since int64) error {
	res, err := s.db.Exec(`UPDATE panes SET agent_state=?, agent_state_since=? WHERE id=?`, state, since, paneID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("pane %s has no registry row (closed, or its insert failed)", paneID)
	}
	return nil
}

// PaneActivities returns every stored reading, keyed by pane id. Panes with
// none are absent.
func (s *SQLite) PaneActivities() (map[string]PaneActivity, error) {
	rows, err := s.db.Query(`SELECT id, agent_state, agent_state_since FROM panes WHERE agent_state != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]PaneActivity{}
	for rows.Next() {
		var id string
		var a PaneActivity
		if err := rows.Scan(&id, &a.State, &a.Since); err != nil {
			return nil, err
		}
		out[id] = a
	}
	return out, rows.Err()
}
