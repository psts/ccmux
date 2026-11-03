package store

// Part is one repo's share of a task: its own plan, written by that repo's
// session, and the runner's record of where it is being built.
type Part struct {
	ID         int64  `json:"id"`
	TaskID     int64  `json:"taskId"`
	RepoPath   string `json:"repoPath"`
	Plan       string `json:"plan"`
	Status     string `json:"status"`
	Note       string `json:"note,omitempty"`
	FiledBy    string `json:"filedBy,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Worktree   string `json:"worktree,omitempty"`
	PaneID     string `json:"paneId,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
	UpdatedAt  int64  `json:"updatedAt"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
}

const addPartSQL = `
INSERT INTO task_parts (task_id,repo_path,plan,status,note,filed_by,created_at,updated_at)
VALUES (?,?,?,?,?,?,?,?)`

const partCols = "id,task_id,repo_path,plan,status,note,filed_by,branch,worktree,pane_id,created_at,updated_at,started_at,finished_at"

// AddPart inserts a part. A task has at most one part per repo, so a second
// one for the same repo is an error.
func (s *SQLite) AddPart(p Part) (int64, error) {
	res, err := s.db.Exec(addPartSQL,
		p.TaskID, p.RepoPath, p.Plan, p.Status, p.Note, p.FiledBy, p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Parts lists every part, by task and then in the order they were added.
func (s *SQLite) Parts() ([]Part, error) {
	rows, err := s.db.Query(`SELECT ` + partCols + ` FROM task_parts ORDER BY task_id, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Part{}
	for rows.Next() {
		var p Part
		if err := rows.Scan(&p.ID, &p.TaskID, &p.RepoPath, &p.Plan, &p.Status, &p.Note, &p.FiledBy,
			&p.Branch, &p.Worktree, &p.PaneID, &p.CreatedAt, &p.UpdatedAt, &p.StartedAt, &p.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SavePart writes back a part's plan and the runner's fields; false when
// the part no longer exists.
func (s *SQLite) SavePart(p Part) (bool, error) {
	res, err := s.db.Exec(`
UPDATE task_parts SET plan=?, status=?, note=?, branch=?, worktree=?, pane_id=?,
  updated_at=?, started_at=?, finished_at=? WHERE id=?`,
		p.Plan, p.Status, p.Note, p.Branch, p.Worktree, p.PaneID, p.UpdatedAt, p.StartedAt, p.FinishedAt, p.ID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
