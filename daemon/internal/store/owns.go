// What each repo takes care of, as the bus tells every session in a window
// ("backend: API, database, auth"). Keyed by the repo's folder, not the
// workspace: it describes the code. Auto is the small model's summary of
// the repo's CLAUDE.md and README, with the hash of what it read, so it is
// written again only when those change; manual is a human's line, which
// wins whenever it is set.
package store

import (
	"database/sql"
	"errors"
)

// RepoOwns is one repo's row. Times are unix milliseconds.
type RepoOwns struct {
	Auto      string `json:"auto"`
	AutoHash  string `json:"-"`
	Manual    string `json:"manual"`
	UpdatedAt int64  `json:"updatedAt"`
}

// Line is what the bus shows: the human's line, else the summary.
func (o RepoOwns) Line() string {
	if o.Manual != "" {
		return o.Manual
	}
	return o.Auto
}

// RepoOwns reads one repo's row; a repo never summarized is the zero row.
func (s *SQLite) RepoOwns(repoPath string) (RepoOwns, error) {
	var o RepoOwns
	err := s.db.QueryRow(`SELECT auto,auto_hash,manual,updated_at FROM repo_owns WHERE repo_path=?`, repoPath).
		Scan(&o.Auto, &o.AutoHash, &o.Manual, &o.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RepoOwns{}, nil
	}
	return o, err
}

// SetRepoOwnsAuto stores a fresh summary and the hash of what it read.
func (s *SQLite) SetRepoOwnsAuto(repoPath, auto, hash string, now int64) error {
	_, err := s.db.Exec(`
INSERT INTO repo_owns (repo_path,auto,auto_hash,updated_at) VALUES (?,?,?,?)
ON CONFLICT(repo_path) DO UPDATE SET auto=excluded.auto, auto_hash=excluded.auto_hash, updated_at=excluded.updated_at`,
		repoPath, auto, hash, now)
	return err
}

// SetRepoOwnsManual stores a human's line; "" hands the repo back to the
// summary.
func (s *SQLite) SetRepoOwnsManual(repoPath, manual string, now int64) error {
	_, err := s.db.Exec(`
INSERT INTO repo_owns (repo_path,manual,updated_at) VALUES (?,?,?)
ON CONFLICT(repo_path) DO UPDATE SET manual=excluded.manual, updated_at=excluded.updated_at`,
		repoPath, manual, now)
	return err
}
