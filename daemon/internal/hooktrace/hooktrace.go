// Package hooktrace appends one JSON object per line to a shared trace file so a
// Claude Code hook can be followed from the moment it fires to the notification
// it did or didn't produce.
//
// Four processes write the same file: ccmux-notify.sh (every hook, before any
// filtering), the daemon's hook listener (what the event routed to), the daemon's
// push notifier (who got buzzed and who was suppressed), and the native app's
// listener (the local macOS alert). Each line carries a stage so the log reads as
// one story per hook rather than four disjoint logs.
//
// Correlation is by trace_id where the id survives the hop (script → socket →
// router) and by timestamp + workspace where it doesn't: the push notifier
// consumes the manager firehose, which carries a workspace and an attention state
// but no hook identity. That's a deliberate limit — threading an id through the
// whole event pipeline costs more than reading two adjacent lines.
package hooktrace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Stage names — the shared vocabulary of the trace file. Go writes two of them;
// the other two name what the shell script and the native app write, so the whole
// format is documented in one place even though this package never emits them.
const (
	StageHook  = "hook"  // ccmux-notify.sh: every hook Claude Code fired
	StageRoute = "route" // daemon hook listener: hook → pane + attention
	StagePush  = "push"  // daemon notifier: one line per subscription considered
	StageLocal = "local" // native app listener: the macOS alert it posted or suppressed
)

// maxBytes caps the trace file. Past it the file is truncated and started fresh
// rather than rotated: this is a debugging aid you tail, not an audit log worth
// keeping history for.
//
// Truncation can't corrupt the file — an O_APPEND write always seeks to the
// current end, so nobody writes into a hole — but with four processes appending
// it is not only old lines that go. Whatever another writer appended between this
// process's size check and its truncate is discarded too. Losing a few lines at
// the 8 MB mark is the accepted cost of not rotating.
const maxBytes = 8 << 20

// Line is one traced event. Everything is omitempty because each stage fills a
// different subset; a line is meant to be read, not schema-validated.
type Line struct {
	TS      string `json:"ts"`
	Stage   string `json:"stage"`
	TraceID string `json:"trace_id,omitempty"`
	Event   string `json:"event,omitempty"`

	// Routing (StageRoute).
	CWD       string `json:"cwd,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	PaneID    string `json:"pane_id,omitempty"`
	Resolved  string `json:"resolved_pane,omitempty"`
	Attention string `json:"attention,omitempty"`
	Session   string `json:"session_signal,omitempty"`

	// Push (StagePush).
	WorkspaceID string `json:"workspace_id,omitempty"`
	Login       string `json:"login,omitempty"`
	Suppressed  string `json:"suppressed_by,omitempty"`

	// Outcome, in every stage: what actually happened, in one word.
	Decision string `json:"decision"`
	Detail   string `json:"detail,omitempty"`
}

var (
	mu   sync.Mutex
	path = defaultPath()
)

// DefaultPath is where the trace lands. It sits beside the daemon's own log so
// one directory holds everything you'd tail while debugging notifications.
func DefaultPath() string {
	mu.Lock()
	defer mu.Unlock()
	return path
}

// SetPath redirects the trace, for tests.
func SetPath(p string) {
	mu.Lock()
	defer mu.Unlock()
	path = p
}

func defaultPath() string {
	if p := os.Getenv("CCMUX_HOOK_TRACE"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "ccmux-hooks.jsonl")
	}
	return filepath.Join(home, "Library", "Logs", "ccmux-hooks.jsonl")
}

// Write appends one line. Every failure is swallowed: a trace that can't be
// written must never change what the daemon does with a hook.
func Write(l Line) {
	l.TS = time.Now().Format(time.RFC3339Nano)
	data, err := json.Marshal(l)
	if err != nil {
		return
	}
	data = append(data, '\n')

	mu.Lock()
	defer mu.Unlock()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > maxBytes {
		_ = f.Truncate(0)
	}
	_, _ = f.Write(data) // one write per line: interleaves atomically with the shell's
}
