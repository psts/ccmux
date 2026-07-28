// Per-pane Claude session truth, persisted. The signal comes from hooks that
// fire once — a session ends, and nothing about that pane ever speaks again —
// so holding it only in memory means a daemon restart silently re-admits a pane
// with no session as if it were live. Restarts are routine; the knowledge has
// to outlive them.
package store

import (
	"encoding/json"
)

// PaneSessionState is one pane's session truth: the sessions known to be
// running there, and when it last showed any sign of life.
type PaneSessionState struct {
	LiveIDs      []string
	LastActivity int64
}

// SavePaneSessions writes one pane's session truth, replacing what was there.
func (s *SQLite) SavePaneSessions(paneID string, liveIDs []string, lastActivity int64) error {
	if liveIDs == nil {
		liveIDs = []string{}
	}
	blob, err := json.Marshal(liveIDs)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
INSERT INTO pane_sessions (pane_id, live_ids, last_activity) VALUES (?,?,?)
ON CONFLICT(pane_id) DO UPDATE SET live_ids=excluded.live_ids, last_activity=excluded.last_activity`,
		paneID, string(blob), lastActivity)
	return err
}

// LoadPaneSessions returns every pane's stored session truth, for rebuilding
// the in-memory view at startup.
func (s *SQLite) LoadPaneSessions() (map[string]PaneSessionState, error) {
	rows, err := s.db.Query(`SELECT pane_id, live_ids, last_activity FROM pane_sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]PaneSessionState{}
	for rows.Next() {
		var paneID, blob string
		var last int64
		if err := rows.Scan(&paneID, &blob, &last); err != nil {
			return nil, err
		}
		var ids []string
		if json.Unmarshal([]byte(blob), &ids) != nil {
			ids = nil // unreadable row degrades to "no known session", never to a crash
		}
		out[paneID] = PaneSessionState{LiveIDs: ids, LastActivity: last}
	}
	return out, rows.Err()
}

// DeletePaneSessions forgets a pane entirely, called when the pane itself is
// gone so this table tracks live panes rather than growing forever.
func (s *SQLite) DeletePaneSessions(paneID string) error {
	_, err := s.db.Exec(`DELETE FROM pane_sessions WHERE pane_id=?`, paneID)
	return err
}
