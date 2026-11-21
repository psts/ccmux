package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// A clean start: a person who sends a message on a cold prompt cache can
// choose to have it open a new conversation rather than re-read the old one
// at full price (the thread page asks). The session row stays the same; its
// conversation moves on, and the one it left is kept in a list so the page
// can still show it, above a line that says a clean one began.
//
// Only a clean start adds to the list. A rewind (runner/rewind.go) and an
// agent's fresh wake (runner/agentsleep.go) rotate the conversation too, but
// they are not this: a rewind's fork carries the history, and an agent's
// past conversations have their own list (api/pastconv.go).

// LeftConversation is one conversation a clean start left: its id, when it
// was left (ms), and the key of the message that asked, so a retry of that
// message does not leave a second one.
type LeftConversation struct {
	ID  string `json:"id"`
	At  int64  `json:"at"`
	Key string `json:"key,omitempty"`
}

// leftCap bounds the list: the page shows only the newest few anyway.
const leftCap = 20

// migrateLeftConversations adds the list's column. Strict, as
// migrateAdvisor is: sessionCols names it. Rows from before have none.
func migrateLeftConversations(db *sql.DB) error {
	_, err := db.Exec(`ALTER TABLE sessions ADD COLUMN left_conversations TEXT DEFAULT ''`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	return nil
}

// leftColumn is the list as the column holds it: JSON, "" for none.
func leftColumn(list []LeftConversation) string {
	if len(list) == 0 {
		return ""
	}
	b, _ := json.Marshal(list) // plain strings and numbers: cannot fail
	return string(b)
}

// parseLeft reads the column back. One that is not a list is dropped with
// the row still readable: it only feeds what the page shows above.
func parseLeft(text string) []LeftConversation {
	if text == "" {
		return nil
	}
	var list []LeftConversation
	if json.Unmarshal([]byte(text), &list) != nil {
		return nil
	}
	return list
}

// HasLeft says whether conv is one this session's clean starts left.
func (r RunnerSession) HasLeft(conv string) bool {
	for _, c := range r.Left {
		if c.ID == conv {
			return true
		}
	}
	return false
}

// LeftFor says whether the newest clean start was asked for by key, so the
// message that asked is not given a second one when it is sent again.
func (r RunnerSession) LeftFor(key string) bool {
	return key != "" && len(r.Left) > 0 && r.Left[len(r.Left)-1].Key == key
}

// StartCleanConversation moves session id onto conversation, and records
// the one it ran until now (left) in the same write: a reader never sees
// the new conversation without the old one listed.
func (s *SQLite) StartCleanConversation(id string, left LeftConversation, conversation string, now int64) error {
	sess, err := s.RunnerSession(id)
	if err != nil {
		return err
	}
	if sess == nil {
		return ErrNoSession
	}
	list := append(append([]LeftConversation(nil), sess.Left...), left)
	if len(list) > leftCap {
		list = list[len(list)-leftCap:]
	}
	if err := s.updateSession(`UPDATE sessions SET conversation=?, fresh_next=0, left_conversations=?, updated_at=? WHERE id=?`,
		conversation, leftColumn(list), now, id); err != nil {
		return fmt.Errorf("start a clean conversation: %w", err)
	}
	return nil
}
