package store

import (
	"database/sql"
	"strings"
)

// A session's advisor model: Claude Code's advisorModel setting, which the
// thread page's Advisor picker sets per session. It applies when the
// session next starts, as its model does: tried live on 2026-11-20 with the
// Agent SDK the sidecar pins (0.3.311), a running session switched with
// applyFlagSettings kept calling the advisor it started with, while one
// started with the setting called the one named.

// migrateAdvisor adds the advisor column. Strict, as migrateBuilders is:
// sessionCols names it. Rows from before have none, and keep the person's
// settings.
func migrateAdvisor(db *sql.DB) error {
	_, err := db.Exec(`ALTER TABLE sessions ADD COLUMN advisor TEXT DEFAULT ''`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	return nil
}

// SetRunnerSessionEffort records the effort a session starts with
// (--effort), set from its page; "" goes back to the model's own. The
// column came with builders (migrateBuilders).
func (s *SQLite) SetRunnerSessionEffort(id, effort string, now int64) error {
	return s.updateSession(`UPDATE sessions SET effort=?, updated_at=? WHERE id=?`, effort, now, id)
}

// SetRunnerSessionAdvisor records a session's advisor, for its later
// starts; "" goes back to the person's settings.
func (s *SQLite) SetRunnerSessionAdvisor(id, advisor string, now int64) error {
	return s.updateSession(`UPDATE sessions SET advisor=?, updated_at=? WHERE id=?`, advisor, now, id)
}
