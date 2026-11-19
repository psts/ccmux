// Task notices: the outbox of what the task queue has to tell a worker (why
// its merge failed and that its part is back with it). The notice is saved
// in the same step as the rows that send the part back, and delivered from
// here afterwards, as often as it takes. So a daemon that stops between the
// two neither loses the word (the row is there) nor says it twice (its id
// is fixed, and the bus stores one event per id).
package store

import "database/sql"

// taskNoticesSchema is the outbox. id is fixed by whoever made the notice
// (the merge job and the part it is about); sent_at is 0 while it waits;
// failed says why a notice that was given up on never arrived.
const taskNoticesSchema = `
CREATE TABLE IF NOT EXISTS task_notices (
  id TEXT PRIMARY KEY, task_id INTEGER NOT NULL, part_id INTEGER NOT NULL,
  pane_id TEXT NOT NULL, text TEXT NOT NULL, created_at INTEGER NOT NULL,
  sent_at INTEGER NOT NULL DEFAULT 0, failed TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS task_notices_waiting ON task_notices (created_at) WHERE sent_at = 0 AND failed = '';
`

// TaskNotice is one row of the outbox. Times are unix milliseconds.
type TaskNotice struct {
	ID        string
	TaskID    int64
	PartID    int64
	PaneID    string
	Text      string
	CreatedAt int64
	SentAt    int64
	Failed    string
}

// Waiting reports whether the notice is still to be delivered.
func (n TaskNotice) Waiting() bool { return n.SentAt == 0 && n.Failed == "" }

// SaveSendBack writes a task, one of its parts and the notice that tells
// the part's worker about it, all three or none: a part running again that
// nobody is going to tell would only sit, and a notice for a part that was
// not saved would send a worker to fix what the queue then refuses. False
// (and nothing written) when the task or the part no longer exists. A
// notice whose id is there already is an error and nothing is written: that
// send-back was made before.
func (s *SQLite) SaveSendBack(t Task, p Part, n TaskNotice) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, step := range []struct {
		query string
		args  []any
	}{{saveTaskSQL, taskArgs(t)}, {savePartSQL, partArgs(p)}} {
		ok, err := execSaved(tx, step.query, step.args...)
		if err != nil || !ok {
			return false, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO task_notices (id,task_id,part_id,pane_id,text,created_at) VALUES (?,?,?,?,?,?)`,
		n.ID, n.TaskID, n.PartID, n.PaneID, n.Text, n.CreatedAt); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// WaitingTaskNotices lists the notices still to be delivered, oldest first.
func (s *SQLite) WaitingTaskNotices() ([]TaskNotice, error) {
	rows, err := s.db.Query(`SELECT id,task_id,part_id,pane_id,text,created_at,sent_at,failed FROM task_notices
WHERE sent_at = 0 AND failed = '' ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	return scanTaskNotices(rows)
}

// EndTaskNotice records that a notice is no longer waiting: delivered at
// sentAt, or given up on with why in failed (sentAt 0).
func (s *SQLite) EndTaskNotice(id string, sentAt int64, failed string) error {
	_, err := s.db.Exec(`UPDATE task_notices SET sent_at=?, failed=? WHERE id=?`, sentAt, failed, id)
	return err
}

// PruneTaskNotices removes the notices that ended (delivered or given up
// on) and were made before the time given. One still waiting stays.
func (s *SQLite) PruneTaskNotices(beforeMillis int64) error {
	_, err := s.db.Exec(`DELETE FROM task_notices WHERE created_at < ? AND NOT (sent_at = 0 AND failed = '')`, beforeMillis)
	return err
}

func scanTaskNotices(rows *sql.Rows) ([]TaskNotice, error) {
	defer rows.Close()
	var out []TaskNotice
	for rows.Next() {
		var n TaskNotice
		if err := rows.Scan(&n.ID, &n.TaskID, &n.PartID, &n.PaneID, &n.Text, &n.CreatedAt, &n.SentAt, &n.Failed); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
