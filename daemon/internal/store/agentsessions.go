// Role agents as sessions. Since task #177 a role agent on the claude
// harness is a runner session (store/sessions.go), not a sidecar typed into
// a tmux pane. Its home is still its workspace row (the one whose Agent
// names its base), now with no tmux session behind it; the home has one
// session that has not ended, of kind agent, kept for as long as the agent
// is in its window. It sleeps and wakes like any session, so its pane id,
// its peer on the bus and its mailbox stay the same across sleeps.
//
// Three things a session row did not need before are kept here:
//
//   - Conversation is the Claude conversation the session runs ("" is the
//     row's own id, as for every other session). An agent whose base says
//     start "fresh" opens a new conversation at each start after a sleep,
//     and its row must stay the same row.
//   - FreshNext says the next start opens a new conversation. The sleep sets
//     it (runner.Supervisor.Sleep), the start that follows clears it. A
//     crash, a restart or a reboot never sets it: those resume.
//   - AgentVersion is the base version the session last started with, which
//     is how a lens says the base has moved on since.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SessionAgent is the kind of a role agent's session.
const SessionAgent = "agent"

// ErrAgentExists is a second session for an agent's home whose session has
// not ended.
var ErrAgentExists = errors.New("this agent already has a session here")

// migrateAgentSessions gives a session row what a role agent's needs, and
// the rule that an agent's home has one session that has not ended: a
// click, a message and a schedule can all start the same agent at once, and
// the database is the one place all three go through. Strict, as
// migrateSessionPanes is: sessionCols names the columns. Rows from before
// have none of it.
func migrateAgentSessions(db *sql.DB) error {
	for _, col := range []string{"conversation TEXT DEFAULT ''", "fresh_next INTEGER DEFAULT 0", "agent_version TEXT DEFAULT ''"} {
		_, err := db.Exec(`ALTER TABLE sessions ADD COLUMN ` + col)
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate sessions.%s: %w", strings.Fields(col)[0], err)
		}
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS sessions_one_agent ON sessions(workspace_id)
WHERE kind='agent' AND wanted<>'done' AND workspace_id<>''`); err != nil {
		return fmt.Errorf("migrate sessions: one session per agent: %w", err)
	}
	return nil
}

// ConversationID is the Claude conversation a session runs: its own id,
// unless it is an agent that has opened a new one since.
func (r RunnerSession) ConversationID() string {
	if r.Conversation != "" {
		return r.Conversation
	}
	return r.ID
}

// AgentSession is the session of the agent whose home is workspaceID, when
// it has one that has not ended, or nil.
func (s *SQLite) AgentSession(workspaceID string) (*RunnerSession, error) {
	list, err := s.querySessions(`SELECT `+sessionCols+` FROM sessions WHERE workspace_id=? AND kind=? AND wanted<>'done'`, workspaceID, SessionAgent)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// AgentSessions lists every agent session that has not ended, oldest first.
func (s *SQLite) AgentSessions() ([]RunnerSession, error) {
	return s.querySessions(`SELECT `+sessionCols+` FROM sessions WHERE kind=? AND wanted<>'done' ORDER BY created_at, id`, SessionAgent)
}

// SetRunnerSessionFresh says whether a session's next start opens a new
// conversation.
func (s *SQLite) SetRunnerSessionFresh(id string, fresh bool, now int64) error {
	return s.updateSession(`UPDATE sessions SET fresh_next=?, updated_at=? WHERE id=?`, fresh, now, id)
}

// SleepRunnerSession puts a session to sleep and says, in the same write,
// whether its next start opens a new conversation: a wake that read one
// without the other would start it on the wrong one.
func (s *SQLite) SleepRunnerSession(id string, fresh bool, now int64) error {
	return s.updateSession(`UPDATE sessions SET wanted=?, fresh_next=?, stuck=0, updated_at=? WHERE id=?`, SessionAsleep, fresh, now, id)
}

// RotateRunnerConversation gives a session a new conversation and clears
// the mark that asked for one.
func (s *SQLite) RotateRunnerConversation(id, conversation string, now int64) error {
	return s.updateSession(`UPDATE sessions SET conversation=?, fresh_next=0, updated_at=? WHERE id=?`, conversation, now, id)
}

// SetRunnerSessionAgentVersion records the base version an agent's session
// starts with.
func (s *SQLite) SetRunnerSessionAgentVersion(id, version string) error {
	return s.updateSession(`UPDATE sessions SET agent_version=? WHERE id=?`, version, id)
}
