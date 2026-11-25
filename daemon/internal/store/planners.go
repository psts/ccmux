// Planners. "Make task" on a repo's to-do list does not prompt the repo's
// main session: it puts the item in the repo's planning line (a to-do of
// status planning, store/todos.go), and one planner session per repo works
// through that line: it plans each item and files it as a task. A planner
// is a runner session of kind planner, in the repo's own folder and in no
// thread; internal/api (planner.go) starts, feeds and ends it. It is never
// the repo's main session: it does not answer to the repo's plain name on
// the bus, and it asks other repos through side helpers of its own
// (internal/threads side.go).
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SessionPlanner is the kind of a repo's planner session.
const SessionPlanner = "planner"

// ErrPlannerExists is a second planner for a workspace whose planner has
// not ended.
var ErrPlannerExists = errors.New("this repo already has a planner session")

// migratePlanners adds the rule that a workspace has one planner that has
// not ended: five clicks on Make task, and the pass that restarts a planner
// whose line is not empty, must end with one, whichever insert came second.
func migratePlanners(db *sql.DB) error {
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS sessions_one_planner ON sessions(workspace_id)
WHERE kind='planner' AND wanted<>'done' AND workspace_id<>''`); err != nil {
		return fmt.Errorf("migrate sessions: one planner per workspace: %w", err)
	}
	return nil
}

// oneOfAKind is the kinds a workspace holds one session of at a time, each
// with what a second one is refused as.
var oneOfAKind = map[string]error{SessionLead: ErrLeadExists, SessionAgent: ErrAgentExists, SessionPlanner: ErrPlannerExists}

// sessionClash names an insert the indexes refused: a second lead, agent or
// planner for a workspace, or a second member of a thread for a repo. Any
// other error, and none, come back as they are.
func sessionClash(r RunnerSession, err error) error {
	if err == nil {
		return nil
	}
	if r.ThreadID != 0 && strings.Contains(err.Error(), "UNIQUE constraint failed: sessions.thread_id") {
		return ErrMemberExists
	}
	if one, ok := oneOfAKind[r.Kind]; ok && strings.Contains(err.Error(), "UNIQUE constraint failed: sessions.workspace_id") {
		return one
	}
	return err
}

// RunnerPlanners lists the planner sessions that have not ended, oldest
// first.
func (s *SQLite) RunnerPlanners() ([]RunnerSession, error) {
	return s.querySessions(`SELECT `+sessionCols+` FROM sessions WHERE kind=? AND wanted<>'done' ORDER BY created_at, id`, SessionPlanner)
}

// LastPlannerModel is the model the newest planner of a workspace ran on,
// ended ones included: "" when it never had one, or that one ran on the
// default. A planner started again for a line that was left (its planner
// ended with items still in it) runs on what the person chose before.
func (s *SQLite) LastPlannerModel(workspaceID string) (string, error) {
	var model string
	err := s.db.QueryRow(`SELECT model FROM sessions WHERE kind=? AND workspace_id=? ORDER BY created_at DESC, id DESC LIMIT 1`,
		SessionPlanner, workspaceID).Scan(&model)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return model, err
}
