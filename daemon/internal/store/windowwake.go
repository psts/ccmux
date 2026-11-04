package store

// Wake marks (table session_wake): a session that was awake when its window
// was last closed carries a mark, and opening the window wakes the marked
// sessions that are asleep, so a window comes back as it was left. Waking a
// session clears its mark. Nothing is used up by reading, so an open that
// fails, or one nobody acts on, loses nothing.

// SetWakeMarks replaces the marks of a window's members with awake (only ids
// that are members), in one transaction.
func (s *SQLite) SetWakeMarks(members, awake []string) error {
	isMember := make(map[string]bool, len(members))
	for _, id := range members {
		isMember[id] = true
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	for _, id := range members {
		if _, err := tx.Exec(`DELETE FROM session_wake WHERE ws_id=?`, id); err != nil {
			return err
		}
	}
	for _, id := range awake {
		if !isMember[id] {
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO session_wake (ws_id) VALUES (?)`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// WakeMarks is the ids of ids that carry a mark, in the order given; an
// empty list, never nil, when none does.
func (s *SQLite) WakeMarks(ids []string) ([]string, error) {
	rows, err := s.db.Query(`SELECT ws_id FROM session_wake`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	marked := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		marked[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []string{}
	for _, id := range ids {
		if marked[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

// ClearWakeMark drops one session's mark: it has been woken.
func (s *SQLite) ClearWakeMark(wsID string) error {
	_, err := s.db.Exec(`DELETE FROM session_wake WHERE ws_id=?`, wsID)
	return err
}
