// Package jobs runs long work outside the daemon: each job is one run of a
// one-shot systemd user service (ccmux-job@<id>, the daemon's own binary as
// `ccmuxd job-exec <spec>`), so a deploy, a crash of the daemon or a reboot
// never cuts the work off, runs it twice or leaves it half tidied.
//
// The rules a job lives by:
//   - The daemon is the only writer of the database. A job writes files: a
//     journal of the steps it finished and, at its end, one result file. The
//     daemon reads the result and acts on it (Handler).
//   - A job may be run again at any time, after a kill or a reboot. It
//     skips the steps its journal says are done (Journal).
//   - One job at a time works on a repo: it holds the repo's lock file,
//     which the kernel releases when the job ends, however it ends (Lock).
//   - The daemon asks systemd whether a job is still running before it calls
//     it dead, and only a plain "dead" counts (decide, in manager.go).
//   - Spec and result carry a version. One that this binary does not read
//     is said loudly and never guessed at (VersionError).
//   - The service kills its whole process group when it stops, and has a
//     time limit (unit.go).
//
// This package knows no kind of job. A kind is a Runner (what the service
// does) and a Handler (what the daemon does with how it ended), registered
// by the package that owns the work: a thread's merge is internal/taskq's
// landing and internal/threads' handler.
package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Version is the format of the spec and result files. Raise it when either
// changes in a way an older or newer binary would misread.
const Version = 1

// VersionError is a spec or result file in a format this binary does not
// read. A job started by one daemon can end under another (a deploy in
// between), so this happens, and nothing is ever read out of such a file.
type VersionError struct {
	File       string
	Have, Want int
}

func (e VersionError) Error() string {
	return fmt.Sprintf("%s is format %d and this ccmuxd reads format %d: it is not read at all, look at it by hand", e.File, e.Have, e.Want)
}

// ErrGarbled is a spec or result file that is not what such a file looks
// like at all. The files are written whole or not at all, so this is not a
// crash's doing, and the file is never read around.
var ErrGarbled = errors.New("the file is garbled")

// Paths are where the job files live. State holds one folder per job (it
// outlives a reboot); Locks the per-repo lock files.
type Paths struct {
	State  string // e.g. ~/.local/state/ccmux/jobs
	Locks  string // e.g. ~/.local/state/ccmux/locks
	Prefix string // the unit template's name before "@"; "" is DefaultPrefix
}

// DefaultPrefix names the systemd template every job runs under, one
// instance per job id. A second daemon on the host passes its own.
const DefaultPrefix = "ccmux-job"

func (p Paths) prefix() string {
	if p.Prefix == "" {
		return DefaultPrefix
	}
	return p.Prefix
}

// Template is the unit template's file name.
func (p Paths) Template() string { return p.prefix() + "@.service" }

// Unit is the service of one job.
func (p Paths) Unit(id string) string { return p.prefix() + "@" + id + ".service" }

// Dir is one job's folder.
func (p Paths) Dir(id string) string { return filepath.Join(p.State, id) }

func (p Paths) SpecFile(id string) string    { return filepath.Join(p.Dir(id), "spec.json") }
func (p Paths) ResultFile(id string) string  { return filepath.Join(p.Dir(id), "result.json") }
func (p Paths) JournalFile(id string) string { return filepath.Join(p.Dir(id), "journal.jsonl") }

// AppliedFile is the daemon's own journal of what it did with the job's
// result: the job's journal is the job's to write, this one the daemon's,
// so no file ever has two writers.
func (p Paths) AppliedFile(id string) string { return filepath.Join(p.Dir(id), "applied.jsonl") }

// Spec is what a job is started from. The daemon writes it once, before the
// job's first start, and every run of the job reads it.
type Spec struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	// Subject is whose job it is ("thread:3"), for the log.
	Subject string `json:"subject,omitempty"`
	// Input is the kind's own.
	Input json.RawMessage `json:"input"`
	// Env is the environment the work runs in: the daemon's own as it was
	// when the job was made, so a check finds the tools it finds today.
	Env []string `json:"env"`
	// MaxSeconds is how long one run may take; 0 is no limit but the unit's.
	MaxSeconds int `json:"maxSeconds,omitempty"`
	// Locks is the folder of the per-repo lock files.
	Locks     string `json:"locks"`
	CreatedAt int64  `json:"createdAt"`
}

// Result is how a job ended, written once by the job as its last act.
type Result struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	// Error is why the job could not do its work at all (its spec was not
	// readable, it ran out of time). Work that was done and came out badly
	// (a merge whose checks failed) is not an error: it is in Output.
	Error string `json:"error,omitempty"`
	// Output is the kind's own.
	Output  json.RawMessage `json:"output,omitempty"`
	EndedAt int64           `json:"endedAt"`
}

// WriteSpec writes a spec for its owner alone (it holds an environment),
// whole or not at all.
func WriteSpec(path string, sp Spec) error {
	sp.Version = Version
	return writeWhole(path, sp)
}

// ReadSpec reads a spec. One in another format is a VersionError.
func ReadSpec(path string) (Spec, error) {
	var sp Spec
	if err := readVersioned(path, &sp); err != nil {
		return Spec{}, err
	}
	if sp.ID == "" || sp.Kind == "" {
		return Spec{}, fmt.Errorf("spec %s names no job or no kind", path)
	}
	return sp, nil
}

// WriteResult writes a result whole or not at all: a reader sees the
// finished file or none.
func WriteResult(path string, r Result) error {
	r.Version = Version
	return writeWhole(path, r)
}

// ReadResult reads a result. None yet is os.ErrNotExist (errors.Is); one in
// another format is a VersionError.
func ReadResult(path string) (Result, error) {
	var r Result
	if err := readVersioned(path, &r); err != nil {
		return Result{}, err
	}
	return r, nil
}

// readVersioned reads a file's version first, and the rest only when this
// binary reads that format.
func readVersioned(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return fmt.Errorf("%s: %w: %v", path, ErrGarbled, err)
	}
	if head.Version != Version {
		return VersionError{File: path, Have: head.Version, Want: Version}
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("%s: %w: %v", path, ErrGarbled, err)
	}
	return nil
}

// writeWhole writes v as JSON to path through a temporary file and a
// rename, synced first: after a crash the file is the old one, the new one,
// or missing, never half of either.
func writeWhole(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
