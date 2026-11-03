package store

import (
	"database/sql"
	"fmt"
	"strings"

	"ccmux.dev/ccmuxd/internal/model"
)

// TaskSignal is a task worker's last hook signal: what its Claude last said
// and when, in unix milliseconds. The task runner judges a worker by it, so
// it has to outlive the daemon: kept only in memory, a restart forgot it and
// a worker deep in one long turn read as never having started.
type TaskSignal struct {
	Attention model.Attention
	Reason    model.AttentionReason
	At        int64
}

// migrateTaskSignal adds the three signal columns. Strict like the other
// pane columns: TaskSignals reads them all. An existing pane reads as no
// signal, which is what it had in memory after a restart anyway.
func migrateTaskSignal(db *sql.DB) error {
	for _, col := range []string{"task_sig TEXT DEFAULT ''", "task_sig_reason TEXT DEFAULT ''", "task_sig_at INTEGER DEFAULT 0"} {
		_, err := db.Exec(`ALTER TABLE panes ADD COLUMN ` + col)
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate panes.%s: %w", strings.Fields(col)[0], err)
		}
	}
	return nil
}

// UpdateTaskSignal writes only the signal, for a pane that still has a row:
// an UPDATE, never an upsert, for the reasons UpdatePaneActivity gives.
func (s *SQLite) UpdateTaskSignal(paneID string, sig TaskSignal) error {
	res, err := s.db.Exec(`UPDATE panes SET task_sig=?, task_sig_reason=?, task_sig_at=? WHERE id=?`,
		string(sig.Attention), string(sig.Reason), sig.At, paneID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("pane %s has no registry row (closed, or its insert failed)", paneID)
	}
	return nil
}

// TaskSignals returns every stored signal, keyed by pane id. Panes with none
// are absent.
func (s *SQLite) TaskSignals() (map[string]TaskSignal, error) {
	rows, err := s.db.Query(`SELECT id, task_sig, task_sig_reason, task_sig_at FROM panes WHERE task_sig_at != 0`)
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
