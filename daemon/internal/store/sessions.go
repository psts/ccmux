// Session runners (internal/runner): Claude sessions that run through the
// Agent SDK in their own systemd service instead of a tmux pane. The daemon
// keeps one row per session saying what should run, an inbox of the prompts
// meant for it, and the cards (questions, approvals) it raised, so all of it
// outlives the daemon, the runner and the machine.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const sessionsSchema = `
-- One row per runner session. wanted is what the daemon keeps it at:
-- running, asleep or done. The id is also the Claude session id. note says
-- why the daemon changed it on its own (a start that failed, a session put
-- to sleep after crashing too often), empty otherwise.
CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY, name TEXT DEFAULT '', cwd TEXT NOT NULL, model TEXT DEFAULT '',
  wanted TEXT NOT NULL, note TEXT DEFAULT '', created_at INTEGER, updated_at INTEGER
);
-- Prompts for a session, in the order they came. delivered_at is set once
-- the prompt's id is in the session's transcript; until then it is sent
-- again after every reconnect, and the runner drops one it already has.
CREATE TABLE IF NOT EXISTS session_inbox (
  id TEXT PRIMARY KEY, session_id TEXT NOT NULL, seq INTEGER NOT NULL, text TEXT NOT NULL,
  sender TEXT DEFAULT '', created_at INTEGER, delivered_at INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS session_inbox_open ON session_inbox(session_id, delivered_at, seq);
-- Cards a session raised. state is open, answered or expired: a card still
-- open when its runner restarts is expired, since the question it asked
-- died with that process.
CREATE TABLE IF NOT EXISTS session_cards (
  id TEXT NOT NULL, session_id TEXT NOT NULL, kind TEXT NOT NULL, body TEXT NOT NULL,
  state TEXT NOT NULL, boot TEXT DEFAULT '', created_at INTEGER, updated_at INTEGER,
  PRIMARY KEY (session_id, id)
);
`

// The states a session row can want.
const (
	SessionRunning = "running"
	SessionAsleep  = "asleep"
	SessionDone    = "done"
)

// SessionLead is the kind of a repo's own session on the thread page: the
// one a repo row opens. Any other session of the repo is a thread ("").
const SessionLead = "lead"

// The states of a card.
const (
	CardOpen     = "open"
	CardAnswered = "answered"
	CardExpired  = "expired"
)

// RunnerSession is one row of sessions. Times are unix milliseconds.
//
// PaneID and WorkspaceID are the session's place in ccmux: the pane id the
// bus, the alerts, the hooks and the LLM proxy know it by, and the repo
// workspace it belongs to (manager/runnerpanes.go). Both are "" on a session
// with no workspace, which runs but is on no bus and raises no alert.
// Attention, AttentionReason and AttentionSince are that pane's attention
// (model.Pane's), kept here so it outlives a daemon restart; the manager
// writes them (SaveRunnerAttention), nothing else does.
type RunnerSession struct {
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	Cwd             string `json:"cwd"`
	Model           string `json:"model,omitempty"`
	Wanted          string `json:"wanted"`
	Note            string `json:"note,omitempty"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
	PaneID          string `json:"paneId,omitempty"`
	WorkspaceID     string `json:"workspaceId,omitempty"`
	Attention       string `json:"attention,omitempty"`
	AttentionReason string `json:"attentionReason,omitempty"`
	AttentionSince  int64  `json:"attentionSince,omitempty"`
	// Kind, Title and Goal are the session's place on the thread page: Kind
	// is SessionLead for a repo's own session (one per workspace that has
	// not ended, kept so by an index) and "" for a thread filed under it;
	// Title is what its row and header show; Goal is the note the session
	// keeps of what it is doing and what comes next, which is the text it
	// gives the bus with set_summary, kept here so it outlives the daemon.
	Kind  string `json:"kind,omitempty"`
	Title string `json:"title,omitempty"`
	Goal  string `json:"goal,omitempty"`
	// ThreadID, Role, Repo, Worktree, Branch and ReadOnly are the session's
	// place in a thread with helpers (store/threads.go); all empty on a
	// session in none. Repo is the repo folder the member works for, which
	// stays the same when Cwd moves into its Worktree on Branch. ReadOnly is
	// a member still in the repo's own folder: it may read there and nothing
	// else, and its first change moves it into a worktree.
	ThreadID int64  `json:"threadId,omitempty"`
	Role     string `json:"role,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Branch   string `json:"branch,omitempty"`
	ReadOnly bool   `json:"readOnly,omitempty"`
	// Effort and PermissionMode are what a session's Claude starts with on
	// top of its model: Claude Code's --effort and --permission-mode. Both
	// "" on a person's own session, which keeps what their settings choose;
	// a task builder (store/builders.go) carries its part's effort and
	// "auto", since nobody is there to answer a permission prompt.
	Effort         string `json:"effort,omitempty"`
	PermissionMode string `json:"permissionMode,omitempty"`
	// Advisor is the advisor model a session starts with (Claude Code's
	// advisorModel setting, store/advisor.go); "" keeps what the person's
	// settings choose.
	Advisor string `json:"advisor,omitempty"`
	// Stuck says the daemon gave up starting the session (store/stuck.go):
	// asleep after failed starts, its note says why. Whatever wants it next
	// clears it.
	Stuck bool `json:"stuck,omitempty"`
	// Loadout is what the session loaded from the capability catalog at its
	// last start (internal/loadout's record, JSON; "" before its first start
	// with one). The routes serve it parsed (api.sessionView), never raw.
	Loadout string `json:"-"`
	// Conversation, FreshNext and AgentVersion are a role agent's
	// (store/agentsessions.go). Conversation also moves on any session a
	// person gave a clean start or rewound; FreshNext and AgentVersion stay
	// empty on any session but an agent's.
	Conversation string `json:"conversation,omitempty"`
	FreshNext    bool   `json:"freshNext,omitempty"`
	AgentVersion string `json:"agentVersion,omitempty"`
	// Left is the conversations its clean starts left, oldest first
	// (store/cleanstart.go), which the thread page shows above the one it
	// runs now.
	Left []LeftConversation `json:"leftConversations,omitempty"`
}

// InboxPrompt is one prompt waiting for, or delivered to, a session.
// Attachments are the images that go with it, as names in the session's
// media folder (runner.Paths.MediaDir): the prompt carries the names, the
// runner reads the files, so a prompt sent again after a crash takes its
// pictures with it.
type InboxPrompt struct {
	ID          string   `json:"id"`
	SessionID   string   `json:"sessionId"`
	Seq         int64    `json:"seq"`
	Text        string   `json:"text"`
	Sender      string   `json:"sender,omitempty"`
	CreatedAt   int64    `json:"createdAt"`
	DeliveredAt int64    `json:"deliveredAt,omitempty"`
	Attachments []string `json:"attachments,omitempty"`
}

// migrateInboxAttachments gives an inbox prompt its attachments column (a
// JSON list of names, ” for none). Strict, as migrateSessionPanes is: the
// inbox's SELECT names the column.
func migrateInboxAttachments(db *sql.DB) error {
	_, err := db.Exec(`ALTER TABLE session_inbox ADD COLUMN attachments TEXT DEFAULT ''`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("migrate session_inbox.attachments: %w", err)
	}
	return nil
}

// attachmentsText is the column's value for a list of names.
func attachmentsText(names []string) (string, error) {
	if len(names) == 0 {
		return "", nil
	}
	b, err := json.Marshal(names)
	return string(b), err
}

// attachmentsOf reads the column back. A value that is not a list is an
// error: a prompt must not go out as if it had no pictures.
func attachmentsOf(text string) ([]string, error) {
	if text == "" {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal([]byte(text), &names); err != nil {
		return nil, fmt.Errorf("session_inbox.attachments %q: %w", text, err)
	}
	return names, nil
}

// SessionCard is one card a session raised; body is the runner's JSON.
type SessionCard struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Kind      string `json:"kind"`
	Body      string `json:"body"`
	State     string `json:"state"`
	Boot      string `json:"boot,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

const sessionCols = "id,name,cwd,model,wanted,note,created_at,updated_at,pane_id,workspace_id,attention,attention_reason,attention_since,kind,title,goal,thread_id,role,repo,worktree,branch,read_only,loadout,effort,permission_mode,conversation,fresh_next,agent_version,advisor,left_conversations,stuck"

// migrateSessionThreads gives a session row its kind, title and goal, and
// the rule that a workspace has one lead that has not ended. Strict, as
// migrateSessionPanes is. Rows from before are threads with no title.
func migrateSessionThreads(db *sql.DB) error {
	for _, col := range []string{"kind TEXT DEFAULT ''", "title TEXT DEFAULT ''", "goal TEXT DEFAULT ''"} {
		_, err := db.Exec(`ALTER TABLE sessions ADD COLUMN ` + col)
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate sessions.%s: %w", strings.Fields(col)[0], err)
		}
	}
	// Two browser windows opening the same repo row at once must end with
	// one lead, whichever insert came second; the database is the one place
	// both go through.
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS sessions_one_lead ON sessions(workspace_id)
WHERE kind='lead' AND wanted<>'done' AND workspace_id<>''`); err != nil {
		return fmt.Errorf("migrate sessions: one lead per workspace: %w", err)
	}
	return nil
}

// ErrLeadExists is a second lead for a workspace whose lead has not ended.
var ErrLeadExists = errors.New("this repo already has a lead session")

// migrateSessionPanes gives a session row its pane: the pane id and the
// workspace it belongs to, and that pane's attention. Strict, like the
// panes columns in Open: the widened SELECT (sessionCols) needs every one,
// and a column missing for any other reason (a locked or read-only
// database) must say so here, not as "no such column" from a later read.
// Rows from before this have no pane and stay off the bus.
func migrateSessionPanes(db *sql.DB) error {
	for _, col := range []string{
		"pane_id TEXT DEFAULT ''", "workspace_id TEXT DEFAULT ''", "attention TEXT DEFAULT ''",
		"attention_reason TEXT DEFAULT ''", "attention_since INTEGER DEFAULT 0",
	} {
		_, err := db.Exec(`ALTER TABLE sessions ADD COLUMN ` + col)
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate sessions.%s: %w", strings.Fields(col)[0], err)
		}
	}
	return nil
}

// AddRunnerSession inserts a new session row. A lead for a workspace that
// has one is ErrLeadExists; a second member of a thread for a repo that has
// one is ErrMemberExists; a second session for a role agent's home that has
// one is ErrAgentExists.
func (s *SQLite) AddRunnerSession(r RunnerSession) error {
	_, err := s.db.Exec(`INSERT INTO sessions (`+sessionCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Name, r.Cwd, r.Model, r.Wanted, r.Note, r.CreatedAt, r.UpdatedAt,
		r.PaneID, r.WorkspaceID, r.Attention, r.AttentionReason, r.AttentionSince, r.Kind, r.Title, r.Goal,
		r.ThreadID, r.Role, r.Repo, r.Worktree, r.Branch, r.ReadOnly, r.Loadout, r.Effort, r.PermissionMode,
		r.Conversation, r.FreshNext, r.AgentVersion, r.Advisor, leftColumn(r.Left), r.Stuck)
	if err != nil && r.Kind == SessionAgent && strings.Contains(err.Error(), "UNIQUE constraint failed: sessions.workspace_id") {
		return ErrAgentExists
	}
	if err != nil && r.Kind == SessionLead && strings.Contains(err.Error(), "UNIQUE constraint failed: sessions.workspace_id") {
		return ErrLeadExists
	}
	if err != nil && r.ThreadID != 0 && strings.Contains(err.Error(), "UNIQUE constraint failed: sessions.thread_id") {
		return ErrMemberExists
	}
	return err
}

// RunnerLead is a workspace's lead session that has not ended, or nil.
func (s *SQLite) RunnerLead(workspaceID string) (*RunnerSession, error) {
	list, err := s.querySessions(`SELECT `+sessionCols+` FROM sessions WHERE workspace_id=? AND kind=? AND wanted<>'done'`, workspaceID, SessionLead)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// SetRunnerSessionTitle sets what a session's row and header show.
func (s *SQLite) SetRunnerSessionTitle(id, title string, now int64) error {
	return s.updateSession(`UPDATE sessions SET title=?, updated_at=? WHERE id=?`, title, now, id)
}

// SetRunnerSessionGoal records the note a session keeps of what it is doing.
func (s *SQLite) SetRunnerSessionGoal(id, goal string, now int64) error {
	return s.updateSession(`UPDATE sessions SET goal=?, updated_at=? WHERE id=?`, goal, now, id)
}

// RunnerPaneIDs is every pane id a session ever had, ended ones included:
// a hook from a session that has ended still names its pane, and must not
// be pinned on another pane in the same folder (manager.ResolvePane).
func (s *SQLite) RunnerPaneIDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT pane_id FROM sessions WHERE pane_id<>''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SaveRunnerAttention records a session pane's attention. A pane with no
// row (the session was deleted under it) is ErrNoSession.
func (s *SQLite) SaveRunnerAttention(paneID, attention, reason string, since int64) error {
	return s.updateSession(`UPDATE sessions SET attention=?, attention_reason=?, attention_since=? WHERE pane_id=?`,
		attention, reason, since, paneID)
}

// RunnerSession is one session by id, or nil when there is none.
func (s *SQLite) RunnerSession(id string) (*RunnerSession, error) {
	list, err := s.querySessions(`SELECT `+sessionCols+` FROM sessions WHERE id=?`, id)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// RunnerSessions lists every session that is not done, oldest first.
func (s *SQLite) RunnerSessions() ([]RunnerSession, error) {
	return s.querySessions(`SELECT ` + sessionCols + ` FROM sessions WHERE wanted<>'done' ORDER BY created_at, id`)
}

func (s *SQLite) querySessions(q string, args ...any) ([]RunnerSession, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunnerSession
	for rows.Next() {
		var r RunnerSession
		var left string
		if err := rows.Scan(&r.ID, &r.Name, &r.Cwd, &r.Model, &r.Wanted, &r.Note, &r.CreatedAt, &r.UpdatedAt,
			&r.PaneID, &r.WorkspaceID, &r.Attention, &r.AttentionReason, &r.AttentionSince, &r.Kind, &r.Title, &r.Goal,
			&r.ThreadID, &r.Role, &r.Repo, &r.Worktree, &r.Branch, &r.ReadOnly, &r.Loadout, &r.Effort, &r.PermissionMode,
			&r.Conversation, &r.FreshNext, &r.AgentVersion, &r.Advisor, &left, &r.Stuck); err != nil {
			return nil, err
		}
		r.Left = parseLeft(left)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ErrNoSession is an update of a session with no row.
var ErrNoSession = errors.New("no such session")

// SetRunnerSessionWanted sets what the daemon keeps a session at.
func (s *SQLite) SetRunnerSessionWanted(id, wanted string, now int64) error {
	return s.updateSession(`UPDATE sessions SET wanted=?, stuck=0, updated_at=? WHERE id=?`, wanted, now, id)
}

// SetRunnerSessionNote records why the daemon acted on a session ("" clears it).
func (s *SQLite) SetRunnerSessionNote(id, note string, now int64) error {
	return s.updateSession(`UPDATE sessions SET note=?, updated_at=? WHERE id=?`, note, now, id)
}

// SetRunnerSessionModel records a session's model, for its later starts.
func (s *SQLite) SetRunnerSessionModel(id, model string, now int64) error {
	return s.updateSession(`UPDATE sessions SET model=?, updated_at=? WHERE id=?`, model, now, id)
}

func (s *SQLite) updateSession(q string, args ...any) error {
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSession
	}
	return nil
}

// AddInboxPromptAndWake queues a prompt and, when wake is set, makes an
// asleep session want to run again with its note cleared, in one
// transaction: either the prompt is queued and the session woken, or
// neither, so a caller that sees an error can send again without the
// prompt running twice.
func (s *SQLite) AddInboxPromptAndWake(p InboxPrompt, wake bool, now int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	attached, err := attachmentsText(p.Attachments)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`
INSERT INTO session_inbox (id,session_id,seq,text,sender,created_at,delivered_at,attachments)
VALUES (?,?,(SELECT COALESCE(MAX(seq),0)+1 FROM session_inbox WHERE session_id=?),?,?,?,0,?)`,
		p.ID, p.SessionID, p.SessionID, p.Text, p.Sender, p.CreatedAt, attached); err != nil {
		return err
	}
	if wake {
		if _, err := tx.Exec(`UPDATE sessions SET wanted=?, note='', stuck=0, updated_at=? WHERE id=? AND wanted=?`,
			SessionRunning, now, p.SessionID, SessionAsleep); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// HasInboxPrompt reports whether a prompt with this id was ever queued,
// delivered or not. The bus names each message's prompt after the message
// (peers/runnerpeer.go) and hands it over again after a crash between its
// two writes; the supervisor asks this first, so the second hand-over
// queues nothing.
func (s *SQLite) HasInboxPrompt(id string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM session_inbox WHERE id=?`, id).Scan(&n)
	return n > 0, err
}

// UndeliveredPrompts is a session's prompts Claude does not have yet, in
// the order they came.
func (s *SQLite) UndeliveredPrompts(sessionID string) ([]InboxPrompt, error) {
	rows, err := s.db.Query(`SELECT id,session_id,seq,text,sender,created_at,delivered_at,attachments FROM session_inbox
WHERE session_id=? AND delivered_at=0 ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InboxPrompt
	for rows.Next() {
		var p InboxPrompt
		var attached string
		if err := rows.Scan(&p.ID, &p.SessionID, &p.Seq, &p.Text, &p.Sender, &p.CreatedAt, &p.DeliveredAt, &attached); err != nil {
			return nil, err
		}
		if p.Attachments, err = attachmentsOf(attached); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkPromptsDelivered stamps the prompts Claude has.
func (s *SQLite) MarkPromptsDelivered(ids []string, now int64) error {
	if len(ids) == 0 {
		return nil
	}
	args := []any{now}
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.Exec(`UPDATE session_inbox SET delivered_at=? WHERE delivered_at=0 AND id IN (?`+
		strings.Repeat(",?", len(ids)-1)+`)`, args...)
	return err
}

// UpsertSessionCard records a card the runner shows as pending. A card it
// already knows keeps its state and first time; a new one opens.
func (s *SQLite) UpsertSessionCard(c SessionCard) error {
	_, err := s.db.Exec(`
INSERT INTO session_cards (id,session_id,kind,body,state,boot,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT(session_id,id) DO NOTHING`,
		c.ID, c.SessionID, c.Kind, c.Body, CardOpen, c.Boot, c.CreatedAt, c.CreatedAt)
	return err
}

// OpenSessionCards lists a session's open cards.
func (s *SQLite) OpenSessionCards(sessionID string) ([]SessionCard, error) {
	rows, err := s.db.Query(`SELECT id,session_id,kind,body,state,boot,created_at,updated_at FROM session_cards
WHERE session_id=? AND state=? ORDER BY created_at, id`, sessionID, CardOpen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionCard
	for rows.Next() {
		var c SessionCard
		if err := rows.Scan(&c.ID, &c.SessionID, &c.Kind, &c.Body, &c.State, &c.Boot, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetSessionCardState moves an open card to answered or expired.
func (s *SQLite) SetSessionCardState(sessionID, id, state string, now int64) error {
	_, err := s.db.Exec(`UPDATE session_cards SET state=?, updated_at=? WHERE session_id=? AND id=? AND state=?`,
		state, now, sessionID, id, CardOpen)
	return err
}
