package store

import (
	"database/sql"
	"strings"
)

// A stuck session: one the daemon gave up starting (runner.Supervisor
// park: its start limit hit, its starts failing before systemd saw them,
// or its walls refused). It sleeps with a note that says why, and the
// lenses draw its dot red, so a person sees it without opening it. Every
// write that wants it again (a wake, a prompt's wake, a sleep) clears the
// mark.

// migrateStuck adds the column. Strict, as migrateAdvisor is: sessionCols
// names it. Rows parked before it existed are marked from their note,
// which every park starts with "put to sleep:".
func migrateStuck(db *sql.DB) error {
	_, err := db.Exec(`ALTER TABLE sessions ADD COLUMN stuck INTEGER DEFAULT 0`)
	if err != nil && strings.Contains(err.Error(), "duplicate column name") {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE sessions SET stuck=1 WHERE wanted=? AND note LIKE 'put to sleep:%'`, SessionAsleep)
	return err
}

// ParkRunnerSession puts a session to sleep as one the daemon gave up
// starting; the caller writes why in its note.
func (s *SQLite) ParkRunnerSession(id string, now int64) error {
	return s.updateSession(`UPDATE sessions SET wanted=?, stuck=1, updated_at=? WHERE id=?`, SessionAsleep, now, id)
}
