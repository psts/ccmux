package jobs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Journal is what a run has finished, step by step, kept in a file so the
// next run of the same work skips it. A step is named by a key that says
// exactly what was done ("checked:<worktree>@<commit>"): work that changed
// since has another key and is done again.
//
// The file is one JSON line per entry, appended and synced. A crash can cut
// the last line short; such a line is not an entry, and the step it was
// about is simply done again. So a step must be safe to repeat, and the
// journal only saves the time (a check that takes minutes) and remembers
// what a repeat cannot find out (what was said, what was left).
type Journal struct {
	path string
	mu   sync.Mutex
	done map[string]json.RawMessage
	keys []string        // the done steps, in the order they were first recorded
	open map[string]bool // begun and not done
}

// Entry is one line of a journal: a step that began (State "started", with
// Label saying what is going on, for whoever watches) or ended ("done",
// with Data it wants remembered).
type Entry struct {
	Key   string          `json:"key"`
	State string          `json:"state"`
	Label string          `json:"label,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
	At    int64           `json:"at"`
}

const (
	stepStarted = "started"
	stepDone    = "done"
)

// OpenJournal reads the journal at path, or starts an empty one.
func OpenJournal(path string) (*Journal, error) {
	j := &Journal{path: path, done: map[string]json.RawMessage{}, open: map[string]bool{}}
	entries, err := ReadJournal(path)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		j.note(e)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return j, endLastLine(path)
}

func (j *Journal) note(e Entry) {
	if e.State == stepDone {
		if _, had := j.done[e.Key]; !had {
			j.keys = append(j.keys, e.Key)
		}
		j.done[e.Key] = e.Data
		delete(j.open, e.Key)
		return
	}
	j.open[e.Key] = true
}

// ReadJournal lists a journal's entries in order. No file is no entries;
// a line that is not an entry (cut short by a crash) is skipped.
func ReadJournal(path string) ([]Entry, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Key != "" {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// endLastLine ends a last line a crash cut short, so the next entry starts
// on a line of its own.
func endLastLine(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 || raw[len(raw)-1] == '\n' {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return appendLine(path, nil)
}

// Done reports whether the step is finished, and reads what it recorded
// into the value given (nil reads nothing).
func (j *Journal) Done(key string, into any) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	data, ok := j.done[key]
	if !ok {
		return false
	}
	if into != nil && len(data) > 0 && json.Unmarshal(data, into) != nil {
		return false // not what this reader expects: do the step again
	}
	return true
}

// Recorded lists what the finished steps whose key starts with prefix
// recorded, in the order they were first finished.
func (j *Journal) Recorded(prefix string) []json.RawMessage {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []json.RawMessage
	for _, k := range j.keys {
		if strings.HasPrefix(k, prefix) {
			out = append(out, j.done[k])
		}
	}
	return out
}

// Cut reports whether the step began and never ended: a run stopped in the
// middle of it.
func (j *Journal) Cut(key string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.open[key]
}

// Begin records that a step starts, with what to show of it meanwhile.
func (j *Journal) Begin(key, label string) error {
	return j.add(Entry{Key: key, State: stepStarted, Label: label})
}

// Record records that a step is finished, with what it wants remembered.
func (j *Journal) Record(key string, data any) error {
	e := Entry{Key: key, State: stepDone}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		e.Data = raw
	}
	return j.add(e)
}

func (j *Journal) add(e Entry) error {
	e.At = time.Now().UnixMilli()
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := appendLine(j.path, line); err != nil {
		return err
	}
	j.note(e)
	return nil
}

// appendLine appends one line and syncs it: an entry that was recorded is
// there after a crash.
func appendLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(line, '\n'))
	serr := f.Sync()
	return errors.Join(werr, serr, f.Close())
}

// Doing is the label of the step a journal's last entry began, "" when its
// last entry ended one or it has none: what a job is in the middle of.
func Doing(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}
	last := entries[len(entries)-1]
	if last.State != stepStarted {
		return ""
	}
	return last.Label
}
