package store

import (
	"database/sql"
	"errors"

	"ccmux.dev/ccmuxd/internal/model"
)

// Scratchpad returns a workspace's scratchpad, or an empty one when nobody
// has written it yet.
func (s *SQLite) Scratchpad(wsID string) (*model.Scratchpad, error) {
	pad := &model.Scratchpad{}
	err := s.db.QueryRow(`SELECT content, updated_at, updated_by FROM scratchpads WHERE ws_id=?`, wsID).
		Scan(&pad.Content, &pad.UpdatedAt, &pad.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return &model.Scratchpad{}, nil
	}
	if err != nil {
		return nil, err
	}
	return pad, nil
}

// SaveScratchpad replaces a workspace's scratchpad.
func (s *SQLite) SaveScratchpad(wsID string, pad *model.Scratchpad) error {
	_, err := s.db.Exec(`
INSERT INTO scratchpads (ws_id, content, updated_at, updated_by) VALUES (?,?,?,?)
ON CONFLICT(ws_id) DO UPDATE SET content=excluded.content,
  updated_at=excluded.updated_at, updated_by=excluded.updated_by`,
		wsID, pad.Content, pad.UpdatedAt, pad.UpdatedBy)
	return err
}

// DeleteScratchpad drops a deleted workspace's scratchpad.
func (s *SQLite) DeleteScratchpad(wsID string) error {
	_, err := s.db.Exec(`DELETE FROM scratchpads WHERE ws_id=?`, wsID)
	return err
}
