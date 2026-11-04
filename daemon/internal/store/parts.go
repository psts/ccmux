package store

import "database/sql"

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
	return execSaved(s.db, savePartSQL, partArgs(p)...)
}

const savePartSQL = `
UPDATE task_parts SET plan=?, status=?, note=?, branch=?, worktree=?, pane_id=?,
  updated_at=?, started_at=?, finished_at=? WHERE id=?`

func partArgs(p Part) []any {
	return []any{p.Plan, p.Status, p.Note, p.Branch, p.Worktree, p.PaneID, p.UpdatedAt, p.StartedAt, p.FinishedAt, p.ID}
}

// execer is what a save runs on: the database, or a transaction.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// execSaved runs one UPDATE and reports whether it matched a row.
func execSaved(ex execer, query string, args ...any) (bool, error) {
	res, err := ex.Exec(query, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SaveTaskAndPart writes a task and one of its parts together, or neither:
// a part sent back to its worker must never run again under a task still
// saved as failed, where its fix would be accepted and never merged. False
// (and nothing written) when either row no longer exists.
func (s *SQLite) SaveTaskAndPart(t Task, p Part) (bool, error) {
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
	return true, tx.Commit()
}
