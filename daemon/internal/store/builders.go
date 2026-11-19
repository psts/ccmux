// Task builders. Since task #176 the Claude that builds one part of a
// queued task is a runner session (store/sessions.go), not a Claude typed
// into a tmux pane: a member, with the role builder, of a thread that has
// no lead (threads.task_id names its task; the task queue leads it). It
// starts in its own worktree, with its part's effort and in auto mode, and
// its last signal to the queue's watchdog is kept on its own row, since a
// runner session has no row in panes.
package store

import (
	"database/sql"
	"fmt"
	"strings"

	"ccmux.dev/ccmuxd/internal/model"
)

// migrateBuilders gives a session row what a builder starts with (effort,
// permission mode) and its last signal to the task queue, and a thread row
// the task it builds. Strict, as migrateSessionPanes is: sessionCols and
// threadCols name the columns. Rows from before have none of it.
func migrateBuilders(db *sql.DB) error {
	for _, col := range []string{
		"sessions ADD COLUMN effort TEXT DEFAULT ''", "sessions ADD COLUMN permission_mode TEXT DEFAULT ''",
		"sessions ADD COLUMN task_sig TEXT DEFAULT ''", "sessions ADD COLUMN task_sig_reason TEXT DEFAULT ''",
		"sessions ADD COLUMN task_sig_at INTEGER DEFAULT 0", "threads ADD COLUMN task_id INTEGER DEFAULT 0",
	} {
		_, err := db.Exec(`ALTER TABLE ` + col)
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate %s: %w", strings.Fields(col)[0]+"."+strings.Fields(col)[3], err)
		}
	}
	return nil
}

// TaskThread is the thread of queued task taskID, the newest when a task
// was built more than once, or nil when it has none yet.
func (s *SQLite) TaskThread(taskID int64) (*Thread, error) {
	list, err := s.queryThreads(`SELECT `+threadCols+` FROM threads WHERE task_id=? ORDER BY id DESC LIMIT 1`, taskID)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// BuildersIn lists the builders that have not ended and work in worktree.
// Read from the rows, never from the daemon's list of panes: a builder
// whose workspace is gone has no pane, and still holds the worktree.
func (s *SQLite) BuildersIn(worktree string) ([]RunnerSession, error) {
	return s.querySessions(`SELECT `+sessionCols+` FROM sessions WHERE role=? AND worktree=? AND wanted<>'done'
ORDER BY created_at, id`, RoleBuilder, worktree)
}

// UpdateRunnerTaskSignal writes a builder's last signal on its session's
// row (UpdateTaskSignal's twin: that one writes a tmux worker's pane row).
func (s *SQLite) UpdateRunnerTaskSignal(paneID string, sig TaskSignal) error {
	return s.updateSession(`UPDATE sessions SET task_sig=?, task_sig_reason=?, task_sig_at=? WHERE pane_id=?`,
		string(sig.Attention), string(sig.Reason), sig.At, paneID)
}

// RunnerTaskSignals returns every builder's stored signal, keyed by pane
// id. Sessions with none, and ended ones, are absent.
func (s *SQLite) RunnerTaskSignals() (map[string]TaskSignal, error) {
	rows, err := s.db.Query(`SELECT pane_id, task_sig, task_sig_reason, task_sig_at FROM sessions
WHERE task_sig_at != 0 AND pane_id<>'' AND wanted<>'done'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]TaskSignal{}
	for rows.Next() {
		var id, att, reason string
		var sig TaskSignal
		if err := rows.Scan(&id, &att, &reason, &sig.At); err != nil {
			return nil, err
		}
		sig.Attention, sig.Reason = model.Attention(att), model.AttentionReason(reason)
		out[id] = sig
	}
	return out, rows.Err()
}
