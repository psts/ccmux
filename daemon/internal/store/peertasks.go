// Delegation-task state that has to outlive the daemon and both sessions.
//
// A delegation is a message whose sender needs to know what happened to it.
// Holding that only in memory (or only in the delegator's context window)
// reproduces the failure the transcripts document: worker restarts eat the
// thread, the delegator chases with pings, and the human ends up hand-carrying
// status. One durable row per delegation is the fix.
package store

// PeerTask is one delegation: from_id asked to_id to do text; status is the
// worker's last report. ToID may be "" while a spawn_if_missing worker is
// still coming up — the first update_task claims the row.
type PeerTask struct {
	TaskID        string `json:"task_id"`
	FromID        string `json:"from_id"`
	ToID          string `json:"to_id"`
	Group         string `json:"group"`
	Text          string `json:"text"`
	Status        string `json:"status"`
	StatusMessage string `json:"status_message,omitempty"`
	Result        string `json:"result,omitempty"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

// SavePeerTask inserts or fully replaces a task row.
func (s *SQLite) SavePeerTask(t PeerTask) error {
	_, err := s.db.Exec(`
INSERT INTO peer_tasks (task_id,from_id,to_id,grp,text,status,status_message,result,created_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(task_id) DO UPDATE SET from_id=excluded.from_id, to_id=excluded.to_id,
  grp=excluded.grp, text=excluded.text, status=excluded.status,
  status_message=excluded.status_message, result=excluded.result,
  created_at=excluded.created_at, updated_at=excluded.updated_at`,
		t.TaskID, t.FromID, t.ToID, t.Group, t.Text, t.Status, t.StatusMessage,
		t.Result, t.CreatedAt, t.UpdatedAt)
	return err
}

// PeerTask returns one task by id, or nil when it does not exist.
func (s *SQLite) PeerTask(taskID string) (*PeerTask, error) {
	row := s.db.QueryRow(`
SELECT task_id,from_id,to_id,grp,text,status,status_message,result,created_at,updated_at
FROM peer_tasks WHERE task_id=?`, taskID)
	var t PeerTask
	err := row.Scan(&t.TaskID, &t.FromID, &t.ToID, &t.Group, &t.Text, &t.Status,
		&t.StatusMessage, &t.Result, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

// OpenPeerTasksFor lists non-terminal tasks the peer is on either end of,
// most recently updated first, capped at limit.
func (s *SQLite) OpenPeerTasksFor(peerID string, limit int) ([]PeerTask, error) {
	rows, err := s.db.Query(`
SELECT task_id,from_id,to_id,grp,text,status,status_message,result,created_at,updated_at
FROM peer_tasks WHERE (from_id=? OR to_id=?) AND status NOT IN ('completed','failed')
ORDER BY updated_at DESC LIMIT ?`, peerID, peerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PeerTask
	for rows.Next() {
		var t PeerTask
		if err := rows.Scan(&t.TaskID, &t.FromID, &t.ToID, &t.Group, &t.Text, &t.Status,
			&t.StatusMessage, &t.Result, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeletePeerTask removes one task (delegation rollback when delivery failed —
// no message went out, so there is nothing to report against).
func (s *SQLite) DeletePeerTask(taskID string) error {
	_, err := s.db.Exec(`DELETE FROM peer_tasks WHERE task_id=?`, taskID)
	return err
}

// PrunePeerTasks drops terminal tasks not updated since cutoff. Open tasks are
// never pruned — an unanswered delegation staying visible is the point.
func (s *SQLite) PrunePeerTasks(beforeMillis int64) error {
	_, err := s.db.Exec(`
DELETE FROM peer_tasks WHERE status IN ('completed','failed') AND updated_at<?`, beforeMillis)
	return err
}
