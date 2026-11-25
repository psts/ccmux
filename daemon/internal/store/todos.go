package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Todo is one item on a repo's to-do list: something a session noticed and
// did not handle right away, kept until a person or an agent deals with it.
// It is not a session's own step list (Claude's TodoWrite), which lives in
// that session's stream and nowhere else. internal/todos owns the rules:
// which urgencies and statuses exist, and what counts as the same item.
// Times are unix milliseconds.
type Todo struct {
	ID int64 `json:"id"`
	// Repo is the list the item is on: the repo's main folder, as
	// todos.Keys resolves it, so a worktree's items land on its repo's list.
	Repo    string `json:"repo"`
	Text    string `json:"text"`
	Urgency string `json:"urgency"`
	Status  string `json:"status"`
	// TaskID is the task the item was filed as, once it has one.
	TaskID int64 `json:"taskId,omitempty"`
	// PlanningAt is when the item was put in its repo's planning line
	// (status planning: handed to the repo's planner, not filed yet), which
	// is its place in that line; 0 for an item that is not in it.
	PlanningAt int64  `json:"planningAt,omitempty"`
	AddedBy    string `json:"addedBy,omitempty"`
	DoneBy     string `json:"doneBy,omitempty"`
	Note       string `json:"note,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
	UpdatedAt  int64  `json:"updatedAt"`
	DoneAt     int64  `json:"doneAt,omitempty"`
}

// todosSchema is the to-do lists' table. norm is the item's text as two
// items are compared (todos.Normalize). The index is what keeps a repo from
// ever holding the same unfinished item twice, whatever raced: the rules
// look first and say so kindly, this refuses what slipped past them.
const todosSchema = `
CREATE TABLE IF NOT EXISTS repo_todos (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    repo       TEXT NOT NULL,
    text       TEXT NOT NULL,
    norm       TEXT NOT NULL,
    urgency    TEXT NOT NULL,
    status     TEXT NOT NULL,
    task_id    INTEGER NOT NULL DEFAULT 0,
    added_by   TEXT NOT NULL DEFAULT '',
    done_by    TEXT NOT NULL DEFAULT '',
    note       TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    done_at    INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS repo_todos_unfinished ON repo_todos (repo, norm) WHERE status != 'done';
`

const todoCols = "id,repo,text,urgency,status,task_id,added_by,done_by,note,created_at,updated_at,done_at,planning_at"

// migrateTodoPlanning gives an item the time it entered its repo's planning
// line. Strict, as migrateSessionPanes is: todoCols names the column. Rows
// from before are in no line.
func migrateTodoPlanning(db *sql.DB) error {
	_, err := db.Exec(`ALTER TABLE repo_todos ADD COLUMN planning_at INTEGER NOT NULL DEFAULT 0`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("migrate repo_todos.planning_at: %w", err)
	}
	return nil
}

// AddTodo adds an item, with norm as what it is compared by, and returns
// its number.
func (s *SQLite) AddTodo(t Todo, norm string) (int64, error) {
	res, err := s.db.Exec(`
INSERT INTO repo_todos (repo,text,norm,urgency,status,task_id,added_by,done_by,note,created_at,updated_at,done_at,planning_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.Repo, t.Text, norm, t.Urgency, t.Status, t.TaskID, t.AddedBy, t.DoneBy, t.Note, t.CreatedAt, t.UpdatedAt, t.DoneAt, t.PlanningAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Todo returns one item, or nil when there is none with that number.
func (s *SQLite) Todo(id int64) (*Todo, error) {
	return s.oneTodo(`SELECT `+todoCols+` FROM repo_todos WHERE id=?`, id)
}

// UnfinishedTodo returns the item of repo that is not done and compares as
// norm, or nil: the one a second sighting of the same thing lands on.
func (s *SQLite) UnfinishedTodo(repo, norm string) (*Todo, error) {
	return s.oneTodo(`SELECT `+todoCols+` FROM repo_todos WHERE repo=? AND norm=? AND status != 'done'`, repo, norm)
}

func (s *SQLite) oneTodo(query string, args ...any) (*Todo, error) {
	t, err := scanTodo(s.db.QueryRow(query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Todos lists a repo's items, every repo's when repo is "", oldest first.
func (s *SQLite) Todos(repo string) ([]Todo, error) {
	return s.queryTodos(`SELECT `+todoCols+` FROM repo_todos WHERE (?='' OR repo=?) ORDER BY id`, repo, repo)
}

// TodosWithTask lists the items filed as a task that are not done yet: the
// ones that turn done, or open again, with their task.
func (s *SQLite) TodosWithTask() ([]Todo, error) {
	return s.queryTodos(`SELECT ` + todoCols + ` FROM repo_todos WHERE task_id != 0 AND status != 'done' ORDER BY id`)
}

// PlanningTodos lists the items in a planning line, every repo's when repo
// is "", in the order they entered it: handed to a planner, not filed yet.
func (s *SQLite) PlanningTodos(repo string) ([]Todo, error) {
	return s.queryTodos(`SELECT `+todoCols+` FROM repo_todos WHERE status='planning' AND (?='' OR repo=?) ORDER BY planning_at, id`, repo, repo)
}

// SaveTodo writes an item back, with norm as what its text now compares
// by; false when the item no longer exists. Its repo, who added it and when
// are not written: they never change.
func (s *SQLite) SaveTodo(t Todo, norm string) (bool, error) {
	return execSaved(s.db, `
UPDATE repo_todos SET text=?, norm=?, urgency=?, status=?, task_id=?, done_by=?, note=?, updated_at=?, done_at=?, planning_at=? WHERE id=?`,
		t.Text, norm, t.Urgency, t.Status, t.TaskID, t.DoneBy, t.Note, t.UpdatedAt, t.DoneAt, t.PlanningAt, t.ID)
}

// DeleteTodo removes one item; false when there was none.
func (s *SQLite) DeleteTodo(id int64) (bool, error) {
	return execSaved(s.db, `DELETE FROM repo_todos WHERE id=?`, id)
}

func (s *SQLite) queryTodos(query string, args ...any) ([]Todo, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Todo{}
	for rows.Next() {
		t, err := scanTodo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func scanTodo(row interface{ Scan(...any) error }) (Todo, error) {
	var t Todo
	err := row.Scan(&t.ID, &t.Repo, &t.Text, &t.Urgency, &t.Status, &t.TaskID, &t.AddedBy, &t.DoneBy, &t.Note,
		&t.CreatedAt, &t.UpdatedAt, &t.DoneAt, &t.PlanningAt)
	return t, err
}
