package memtidy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// outcome is one repo's night, as the log keeps it.
type outcome struct {
	At            string   `json:"at"`
	Repo          string   `json:"repo"`
	Conversations int      `json:"conversations"`        // read tonight
	Unreadable    []string `json:"unreadable,omitempty"` // "path: why"
	Deferred      int      `json:"deferred,omitempty"`   // left for the next night
	Result        string   `json:"result"`               // applied, unchanged, refused, skipped, failed
	Why           string   `json:"why,omitempty"`
	Added         []string `json:"added,omitempty"`
	Changed       []string `json:"changed,omitempty"`
	Removed       []string `json:"removed,omitempty"`
	Answer        string   `json:"answer,omitempty"`
	CostUSD       float64  `json:"costUSD,omitempty"` // list price, as Claude Code says
	Todos         int      `json:"todos,omitempty"`   // problems filed on the to-do list
}

// ranToTheEnd is a night whose Claude finished and whose result stands:
// its bookmark moves, and what it proposed is filed. A failed, skipped or
// partial night is read again next time.
func (o outcome) ranToTheEnd() bool {
	switch o.Result {
	case "applied", "unchanged", "refused":
		return true
	}
	return false
}

// tidyRepo is one repo's night: read what is new, let Claude tidy a copy,
// check it and put it in place. The bookmark moves up to what was read,
// unless the work failed or a session wrote the memory meanwhile: then the
// same conversations are read again next night. A repo switched on again
// at onAt reads no further back than firstLook before it.
func (t *Tidier) tidyRepo(ctx context.Context, r repo, onAt time.Time) {
	st, err := t.loadState()
	if err != nil {
		log.Printf("memory tidy: %v", err)
		return
	}
	until := t.clock()
	since := until.Add(-firstLook)
	if ms, ok := st.Bookmarks[r.Path]; ok {
		since = time.UnixMilli(ms)
	}
	if back := onAt.Add(-firstLook); !onAt.IsZero() && back.After(since) {
		since = back
	}
	convs := conversationsOf(t.Projects, r, since, until)
	if len(convs) == 0 {
		return
	}
	o, through := t.tidy(ctx, r, convs, until)
	o.At, o.Repo = until.Format(time.RFC3339), r.Path
	t.record(o)
	if ctx.Err() != nil || !o.ranToTheEnd() {
		return
	}
	if st, err = t.loadState(); err == nil {
		st.Bookmarks[r.Path] = through.UnixMilli()
		err = t.saveState(st)
	}
	if err != nil {
		log.Printf("memory tidy: %v", err)
	}
}

// tidy runs one repo's tidy-up, and says up to when its conversations
// were dealt with.
func (t *Tidier) tidy(ctx context.Context, r repo, convs []conversation, until time.Time) (outcome, time.Time) {
	live := filepath.Join(projectDir(t.Projects, r.Path), "memory")
	read, err := readNotes(live)
	if err != nil {
		return outcome{Result: "failed", Why: err.Error()}, until
	}
	work := filepath.Join(t.StateDir, "work", filepath.Base(projectDir(t.Projects, r.Path)))
	defer os.RemoveAll(work)
	b, err := prepare(work, read, convs, until)
	if err != nil {
		return outcome{Result: "failed", Why: err.Error()}, until
	}
	counts := outcome{Conversations: b.n, Unreadable: b.unreadable, Deferred: b.deferred}
	if b.n == 0 {
		counts.Result, counts.Why = "unchanged", "no conversation could be read"
		return counts, b.through
	}
	answer, err := t.Claude(ctx, work, prompt(r.Path, b.n))
	if err != nil {
		counts.Result, counts.Why, counts.CostUSD = "failed", err.Error(), answer.CostUSD
		return counts, until
	}
	o := t.settle(r, live, read, filepath.Join(work, "memory"))
	o.Conversations, o.Unreadable, o.Deferred = counts.Conversations, counts.Unreadable, counts.Deferred
	o.Answer, o.CostUSD = clip(strings.TrimSpace(answer.Text), 600), answer.CostUSD
	if o.ranToTheEnd() && ctx.Err() == nil {
		o.Todos = t.fileProposals(r.Path, readProposals(work, r.Path, t.secrets()))
	}
	return o, b.through
}

func (t *Tidier) secrets() []string {
	if t.Secrets == nil {
		return nil
	}
	return t.Secrets()
}

// settle checks the tidied copy and puts it in place.
func (t *Tidier) settle(r repo, live string, read files, tidied string) outcome {
	new, err := readNotes(tidied)
	if err != nil {
		return outcome{Result: "failed", Why: err.Error()}
	}
	dropEmpty(new)
	c := diff(read, new)
	o := outcome{Added: c.Added, Changed: c.Changed, Removed: c.Removed}
	if c.empty() {
		o.Result = "unchanged"
		return o
	}
	if err := check(read, new, c, t.secrets()); err != nil {
		o.Result, o.Why = "refused", err.Error()
		return o
	}
	backup := filepath.Join(t.StateDir, "backups", filepath.Base(projectDir(t.Projects, r.Path)), t.clock().Format("2006-01-02T1504"))
	wrote, err := apply(live, read, new, c, backup)
	if errors.Is(err, errChanged) {
		o.Result, o.Why = "skipped", err.Error()
		return o
	}
	if err != nil && len(wrote) > 0 {
		// Some notes landed before the error: say which, and where the
		// old memory is, so a person can put it back.
		o.Result = "partial"
		o.Why = fmt.Sprintf("%v; wrote %s before it; the old memory is in %s", err, strings.Join(wrote, ", "), backup)
		return o
	}
	if err != nil {
		o.Result, o.Why = "failed", err.Error()
		return o
	}
	pruneBackups(filepath.Dir(backup), keepNights)
	o.Result = "applied"
	return o
}

// batch is what prepare put in the work folder: n conversations, the
// unreadable ones it left out, the ones deferred to the next night when
// the budget ran out, and the time up to which they were all dealt with.
type batch struct {
	n, deferred int
	unreadable  []string
	through     time.Time
}

// prepare lays out the work folder: ./memory, a copy of the notes, and
// ./conversations, the digests, oldest first up to the night's budget.
// What does not fit waits for the next night (through stops before it).
func prepare(work string, notes files, convs []conversation, until time.Time) (batch, error) {
	b := batch{through: until}
	if err := os.RemoveAll(work); err != nil {
		return b, err
	}
	if err := writeNotes(filepath.Join(work, "memory"), notes); err != nil {
		return b, err
	}
	if err := os.MkdirAll(filepath.Join(work, "memory"), 0o700); err != nil {
		return b, err
	}
	budget := repoBudget
	for i, c := range convs {
		text, err := digest(c.Path)
		if err != nil {
			b.unreadable = append(b.unreadable, c.Path+": "+err.Error())
			continue
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if len(text) > budget && b.n > 0 {
			b.deferred, b.through = len(convs)-i, c.MTime.Add(-time.Millisecond)
			break
		}
		text = clip(text, budget)
		budget -= len(text)
		name := fmt.Sprintf("%s-%s.md", c.MTime.Format("2006-01-02T1504"), strings.TrimSuffix(filepath.Base(c.Path), ".jsonl"))
		if err := writeAtomic(filepath.Join(work, "conversations", name), []byte(text)); err != nil {
			return b, err
		}
		b.n++
	}
	return b, nil
}

// pruneBackups keeps a repo's newest backups.
func pruneBackups(dir string, keep int) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > keep {
		_ = os.RemoveAll(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}

// dropEmpty takes out the notes Claude emptied: it has no tool that
// deletes, so an empty file is how it removes a note.
func dropEmpty(notes files) {
	for rel, b := range notes {
		if len(bytes.TrimSpace(b)) == 0 {
			delete(notes, rel)
		}
	}
}
