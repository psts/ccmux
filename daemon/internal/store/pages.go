// Pages: an agent paging its human (POST /v1/panes/{id}/page). One row per
// incident key per window, so a page is sent once however often an agent
// runs into the same incident.
package store

import (
	"database/sql"
	"errors"
)

// ClaimPage records the first page for incident in scope and reports true;
// when one was already sent it reports false with when and by whom.
func (s *SQLite) ClaimPage(scope, incident, byName string, now int64) (claimed bool, pagedAt int64, pagedBy string, err error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO agent_pages (scope, incident, paged_at, by_name) VALUES (?,?,?,?)`,
		scope, incident, now, byName)
	if err != nil {
		return false, 0, "", err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err == nil, now, byName, err
	}
	err = s.db.QueryRow(`SELECT paged_at, by_name FROM agent_pages WHERE scope=? AND incident=?`, scope, incident).
		Scan(&pagedAt, &pagedBy)
	if errors.Is(err, sql.ErrNoRows) {
		err = errors.New("page row vanished between insert and read")
	}
	return false, pagedAt, pagedBy, err
}

// ReleasePage forgets a claim, for a page that reached nobody: the next
// try for that incident is then a first page again.
func (s *SQLite) ReleasePage(scope, incident string) error {
	_, err := s.db.Exec(`DELETE FROM agent_pages WHERE scope=? AND incident=?`, scope, incident)
	return err
}
