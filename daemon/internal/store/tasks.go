package store

// Task is one feature in the task queue, built by its Parts (one per repo).
// internal/taskq owns the rules: which statuses exist, which moves are
// allowed. Times are unix milliseconds.
type Task struct {
	ID         int64  `json:"id"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	AfterID    int64  `json:"afterId,omitempty"`
	Position   int64  `json:"position"`
	Note       string `json:"note,omitempty"`
	FiledBy    string `json:"filedBy,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
	UpdatedAt  int64  `json:"updatedAt"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
}

const taskCols = "id,title,status,after_id,position,note,filed_by,created_at,updated_at,started_at,finished_at"

// AddTaskWithPart files a task at the back of the queue together with its
// first part, in one step: a task left without parts could never start.
func (s *SQLite) AddTaskWithPart(t Task, p Part) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(addTaskSQL, t.Title, t.Status, t.AfterID, t.Note, t.FiledBy, t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(addPartSQL, id, p.RepoPath, p.Plan, p.Status, p.Note, p.FiledBy, p.CreatedAt, p.UpdatedAt); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

const addTaskSQL = `
INSERT INTO tasks (title,status,after_id,position,note,filed_by,created_at,updated_at)
VALUES (?,?,?,(SELECT COALESCE(MAX(position),0)+1 FROM tasks),?,?,?,?)`

// Task returns one task, or nil when there is none with that id.
func (s *SQLite) Task(id int64) (*Task, error) {
	list, err := s.queryTasks(`SELECT `+taskCols+` FROM tasks WHERE id=?`, id)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// Tasks lists every task in queue order.
func (s *SQLite) Tasks() ([]Task, error) {
	return s.queryTasks(`SELECT ` + taskCols + ` FROM tasks ORDER BY position, id`)
}

// SaveTask writes back the fields a lens or the runner may change. It
// reports false when the task no longer exists. Position is not one of them:
// only SwapTaskPositions moves a task, so saving a copy read before a move
// cannot undo it.
func (s *SQLite) SaveTask(t Task) (bool, error) {
	res, err := s.db.Exec(`
UPDATE tasks SET title=?, status=?, after_id=?, note=?,
  updated_at=?, started_at=?, finished_at=? WHERE id=?`,
		t.Title, t.Status, t.AfterID, t.Note, t.UpdatedAt, t.StartedAt, t.FinishedAt, t.ID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SwapTaskPositions trades two tasks' places in the queue in one step, so a
// failure halfway never leaves two tasks on the same position.
func (s *SQLite) SwapTaskPositions(a, b Task) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE tasks SET position=? WHERE id=?`, b.Position, a.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE tasks SET position=? WHERE id=?`, a.Position, b.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteTask removes one task and its parts; false when there was none.
func (s *SQLite) DeleteTask(id int64) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM task_parts WHERE task_id=?`, id); err != nil {
		return false, err
	}
	res, err := tx.Exec(`DELETE FROM tasks WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, tx.Commit()
}

func (s *SQLite) queryTasks(query string, args ...any) ([]Task, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.Title, &t.Status, &t.AfterID, &t.Position, &t.Note, &t.FiledBy,
			&t.CreatedAt, &t.UpdatedAt, &t.StartedAt, &t.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
