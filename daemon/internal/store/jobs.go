// Jobs. A job is one piece of long work the daemon handed to its own
// one-shot service (internal/jobs): a thread's merge, say. The work runs in
// that service and writes files; this row is the daemon's record of it, and
// the daemon is the only one that writes it.
package store

import (
	"database/sql"
	"errors"
	"strings"
)

// jobsSchema is the jobs table. subject says whose job it is ("thread:3");
// state is running, stopping (a stop was asked for and its service has not
// ended yet) or done; outcome says how a done job ended (JobApplied,
// JobLost, JobStopped) and note why; ctx is what the one who started it
// wants back when it ends, as JSON the job itself never reads; attempts
// counts the starts of its service.
//
// The index is the rule that a subject has one job at a time, whatever
// raced: a thread cannot be merged twice at once.
const jobsSchema = `
CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, subject TEXT NOT NULL DEFAULT '', state TEXT NOT NULL,
  outcome TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '', ctx TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0, started_at INTEGER NOT NULL, ended_at INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS jobs_one_open ON jobs (subject) WHERE state != 'done' AND subject != '';
`

// The states of a job, and how a done one ended.
const (
	JobRunning  = "running"
	JobStopping = "stopping"
	JobDone     = "done"

	JobApplied = "applied" // its result was read and acted on
	JobLost    = "lost"    // it ended with no result the daemon could read
	JobStopped = "stopped" // it was stopped on request
)

// Job is one row of jobs. Times are unix milliseconds.
type Job struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Subject   string `json:"subject,omitempty"`
	State     string `json:"state"`
	Outcome   string `json:"outcome,omitempty"`
	Note      string `json:"note,omitempty"`
	Ctx       string `json:"-"`
	Attempts  int    `json:"attempts"`
	StartedAt int64  `json:"startedAt"`
	EndedAt   int64  `json:"endedAt,omitempty"`
}

const jobCols = "id,kind,subject,state,outcome,note,ctx,attempts,started_at,ended_at"

// ErrJobOpen is a second job for a subject whose first has not ended.
var ErrJobOpen = errors.New("a job for this is running already")

// ErrNoJob is an update of a job with no row.
var ErrNoJob = errors.New("no such job")

// AddJob inserts a job. A subject that has a job that is not done is
// ErrJobOpen.
func (s *SQLite) AddJob(j Job) error {
	_, err := s.db.Exec(`INSERT INTO jobs (`+jobCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.Kind, j.Subject, j.State, j.Outcome, j.Note, j.Ctx, j.Attempts, j.StartedAt, j.EndedAt)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: jobs.subject") {
		return ErrJobOpen
	}
	return err
}

// SaveJob writes a job's changeable fields: its state, how it ended, and
// how often it was started. A job with no row is ErrNoJob.
func (s *SQLite) SaveJob(j Job) error {
	res, err := s.db.Exec(`UPDATE jobs SET state=?, outcome=?, note=?, attempts=?, ended_at=? WHERE id=?`,
		j.State, j.Outcome, j.Note, j.Attempts, j.EndedAt, j.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoJob
	}
	return nil
}

// Job is one job by id, or nil when there is none.
func (s *SQLite) Job(id string) (*Job, error) {
	return s.oneJob(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id)
}

// OpenJob is the job of subject that is not done, or nil.
func (s *SQLite) OpenJob(subject string) (*Job, error) {
	return s.oneJob(`SELECT `+jobCols+` FROM jobs WHERE subject=? AND state != 'done'`, subject)
}

// LastJob is the newest job of subject, done or not, or nil.
func (s *SQLite) LastJob(subject string) (*Job, error) {
	return s.oneJob(`SELECT `+jobCols+` FROM jobs WHERE subject=? ORDER BY started_at DESC, rowid DESC LIMIT 1`, subject)
}

// OpenJobs lists every job that is not done, oldest first.
func (s *SQLite) OpenJobs() ([]Job, error) {
	return s.queryJobs(`SELECT ` + jobCols + ` FROM jobs WHERE state != 'done' ORDER BY started_at, rowid`)
}

// JobsEndedBefore lists the done jobs that ended before the time given:
// the ones whose files can go.
func (s *SQLite) JobsEndedBefore(millis int64) ([]Job, error) {
	return s.queryJobs(`SELECT `+jobCols+` FROM jobs WHERE state = 'done' AND ended_at < ? ORDER BY ended_at`, millis)
}

func (s *SQLite) oneJob(q string, args ...any) (*Job, error) {
	list, err := s.queryJobs(q, args...)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

func (s *SQLite) queryJobs(q string, args ...any) ([]Job, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func scanJob(rows *sql.Rows) (Job, error) {
	var j Job
	err := rows.Scan(&j.ID, &j.Kind, &j.Subject, &j.State, &j.Outcome, &j.Note, &j.Ctx, &j.Attempts, &j.StartedAt, &j.EndedAt)
	return j, err
}
