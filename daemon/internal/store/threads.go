// Threads with helpers. A thread is one piece of work with a team: the
// session that leads it, in one repo, and a helper session for every other
// repo it needs. The thread has a row here; its members are session rows
// (store/sessions.go) that carry the thread's id, their role, the repo they
// work for and, once they changed something, their worktree and branch.
// internal/threads owns the rules.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const threadsSchema = `
-- One row per thread. ws_group is the ccmux window it was started in ('' for
-- a repo in none); lead_session its lead's session id; state is open,
-- merging, merged or ended; model is what its members start on ('' for the
-- default); note says how its last merge went.
CREATE TABLE IF NOT EXISTS threads (
  id INTEGER PRIMARY KEY AUTOINCREMENT, ws_group TEXT DEFAULT '', lead_session TEXT DEFAULT '',
  title TEXT DEFAULT '', goal TEXT DEFAULT '', state TEXT NOT NULL, model TEXT DEFAULT '',
  note TEXT DEFAULT '', created_at INTEGER, updated_at INTEGER
);
`

// The states of a thread.
const (
	ThreadOpen    = "open"
	ThreadMerging = "merging"
	ThreadMerged  = "merged"
	ThreadEnded   = "ended"
)

// The roles of a thread's members.
const (
	RoleLead   = "lead"
	RoleHelper = "helper"
)

// Thread is one row of threads. Times are unix milliseconds.
type Thread struct {
	ID          int64  `json:"id"`
	Group       string `json:"group,omitempty"`
	LeadSession string `json:"leadSession"`
	Title       string `json:"title,omitempty"`
	Goal        string `json:"goal,omitempty"`
	State       string `json:"state"`
	Model       string `json:"model,omitempty"`
	Note        string `json:"note,omitempty"`
	CreatedAt   int64  `json:"createdAt"`
	UpdatedAt   int64  `json:"updatedAt"`
}

const threadCols = "id,ws_group,lead_session,title,goal,state,model,note,created_at,updated_at"

// ErrMemberExists is a second member of a thread for a repo that already
// has one that has not ended.
var ErrMemberExists = errors.New("this thread already has a session for that repo")

// ErrNoThread is an update of a thread with no row.
var ErrNoThread = errors.New("no such thread")

// migrateSessionMembers gives a session row its place in a thread, and the
// rule that a thread has one member per repo that has not ended. Strict, as
// migrateSessionPanes is: sessionCols names every column. Rows from before
// are in no thread.
func migrateSessionMembers(db *sql.DB) error {
	for _, col := range []string{
		"thread_id INTEGER DEFAULT 0", "role TEXT DEFAULT ''", "repo TEXT DEFAULT ''",
		"worktree TEXT DEFAULT ''", "branch TEXT DEFAULT ''", "read_only INTEGER DEFAULT 0",
	} {
		_, err := db.Exec(`ALTER TABLE sessions ADD COLUMN ` + col)
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate sessions.%s: %w", strings.Fields(col)[0], err)
		}
	}
	// Two messages to the same repo at once must end with one helper,
	// whichever insert came second; the database is where both meet.
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS sessions_one_member ON sessions(thread_id, repo)
WHERE thread_id<>0 AND wanted<>'done'`); err != nil {
		return fmt.Errorf("migrate sessions: one member per thread and repo: %w", err)
	}
	return nil
}

// AddThread inserts a thread and returns it with its id.
func (s *SQLite) AddThread(t Thread) (Thread, error) {
	res, err := s.db.Exec(`INSERT INTO threads (ws_group,lead_session,title,goal,state,model,note,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		t.Group, t.LeadSession, t.Title, t.Goal, t.State, t.Model, t.Note, t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return t, err
	}
	t.ID, err = res.LastInsertId()
	return t, err
}

// SaveThread writes a thread's changeable fields. A thread with no row is
// ErrNoThread.
func (s *SQLite) SaveThread(t Thread) error {
	res, err := s.db.Exec(`UPDATE threads SET lead_session=?, title=?, goal=?, state=?, model=?, note=?, updated_at=? WHERE id=?`,
		t.LeadSession, t.Title, t.Goal, t.State, t.Model, t.Note, t.UpdatedAt, t.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoThread
	}
	return nil
}

// Thread is one thread by id, or nil when there is none.
func (s *SQLite) Thread(id int64) (*Thread, error) {
	list, err := s.queryThreads(`SELECT `+threadCols+` FROM threads WHERE id=?`, id)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// Threads lists every thread that has not ended, oldest first.
func (s *SQLite) Threads() ([]Thread, error) {
	return s.queryThreads(`SELECT `+threadCols+` FROM threads WHERE state<>? ORDER BY id`, ThreadEnded)
}

func (s *SQLite) queryThreads(q string, args ...any) ([]Thread, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Thread
	for rows.Next() {
		var t Thread
		if err := rows.Scan(&t.ID, &t.Group, &t.LeadSession, &t.Title, &t.Goal, &t.State, &t.Model, &t.Note, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ThreadMembers lists a thread's sessions that have not ended, the lead
// first, then its helpers oldest first.
func (s *SQLite) ThreadMembers(threadID int64) ([]RunnerSession, error) {
	return s.querySessions(`SELECT `+sessionCols+` FROM sessions WHERE thread_id=? AND wanted<>'done'
ORDER BY role=? DESC, created_at, id`, threadID, RoleLead)
}

// SessionPlace is where a thread member works: its folder, and the worktree
// and branch it has there once it changed something.
type SessionPlace struct {
	Cwd, Worktree, Branch string
	ReadOnly              bool
}

// SetRunnerSessionPlace moves a session: its folder, its worktree and branch
// ("" for none) and whether it only reads there.
func (s *SQLite) SetRunnerSessionPlace(id string, p SessionPlace, now int64) error {
	return s.updateSession(`UPDATE sessions SET cwd=?, worktree=?, branch=?, read_only=?, updated_at=? WHERE id=?`,
		p.Cwd, p.Worktree, p.Branch, p.ReadOnly, now, id)
}
