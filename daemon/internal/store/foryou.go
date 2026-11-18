package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// migrateForYou adds a part's For-you lines (task_parts.for_you, a JSON
// list) and whether a person has read a task's (tasks.for_you_read).
// Strict: every read selects them. Rows from before have neither.
func migrateForYou(db *sql.DB) error {
	for _, col := range []string{
		`task_parts ADD COLUMN for_you TEXT DEFAULT ''`,
		`tasks ADD COLUMN for_you_read INTEGER DEFAULT 0`,
	} {
		_, err := db.Exec(`ALTER TABLE ` + col)
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate %s: %w", strings.Fields(col)[0], err)
		}
	}
	return nil
}

// linesText is a list of lines as its column holds it ("" for none);
// linesOf reads it back.
func linesText(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	b, _ := json.Marshal(lines) // a list of strings always marshals
	return string(b)
}

func linesOf(text string) ([]string, error) {
	if text == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("task_parts.for_you holds %q, not a list of lines", text)
	}
	return out, nil
}
