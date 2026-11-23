// Package llmserve runs the LLM routing proxy (internal/llmproxy) as its own
// process: `ccmuxd llm-proxy`, under the systemd user unit ccmux-llm-proxy.
// The daemon restarts on every deploy, and a reply being streamed through it
// was cut each time. This service restarts only when its own code changed.
// It also runs the Meridian sidecars (sidecars.go), for the same reason.
//
// It reads what it needs straight from the daemon's database, read only: the
// accounts and routes (settings), and which harness a pane runs (panes and
// sessions). So it answers whether or not the daemon is up, and the daemon
// stays the only writer.
//
// KEEP THE IMPORTS SMALL. The deploy script hashes this package and every
// package of this module it imports (scripts/llm-proxy-hash.sh), and the
// service restarts when that hash moves. Importing store, manager or api
// here would restart it on nearly every deploy, which is the thing this
// package exists to stop.
package llmserve

import (
	"database/sql"
	"errors"
	"net/url"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"

	"ccmux.dev/ccmuxd/internal/harness"
)

// Reader is the daemon's database, opened read only. It is llmproxy's Store
// and harness's Store, with the write refused, plus the one pane question
// the proxy asks.
type Reader struct {
	db *sql.DB
}

// OpenReader prepares a read-only handle on the daemon's database. Nothing
// is opened yet: the service starts at boot, maybe before the daemon has
// ever made the file, and it must stay up and answer "unreadable" per
// request rather than exit (a fast exit loop runs into systemd's start limit
// and then stays down). Each query opens the file when it needs to.
func OpenReader(path string) (*Reader, error) {
	// mode=ro is SQLite's own read-only open: this connection cannot write
	// the database even by mistake. It still reads a WAL database the daemon
	// is writing, and one the daemon left behind without closing.
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	return &Reader{db: db}, nil
}

func (r *Reader) Close() error { return r.db.Close() }

// GetSetting reads one settings row, "" when there is none (store.GetSetting's
// own contract, which llmproxy and harness are written against).
func (r *Reader) GetSetting(key string) (string, error) {
	var v sql.NullString
	err := r.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v.String, err
}

// ErrReadOnly is every write's answer here.
var ErrReadOnly = errors.New("the llm proxy service reads the daemon's database and never writes it")

func (r *Reader) SetSetting(string, string) error { return ErrReadOnly }

// HarnessForPane is the harness a pane runs, "" for none or an unknown pane:
// the manager's HarnessForPane, answered from the rows the manager's memory
// is built from.
//
// A tmux pane has a row in panes and its harness is a column there. A runner
// session (internal/runner) has no such row: its pane exists while the
// session has not ended and its workspace still does, and it always runs
// claude (manager/runnerpanes.go, newRunnerPane and SyncRunnerPane).
func (r *Reader) HarnessForPane(paneID string) (string, error) {
	var name sql.NullString
	err := r.db.QueryRow(`SELECT harness FROM panes WHERE id=?`, paneID).Scan(&name)
	if err == nil {
		return name.String, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var one int
	err = r.db.QueryRow(`SELECT 1 FROM sessions s JOIN workspaces w ON w.id = s.workspace_id
WHERE s.pane_id=? AND s.pane_id<>'' AND s.wanted<>'done' LIMIT 1`, paneID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return harness.Builtin, nil
}
