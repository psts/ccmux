// Package memtidy tidies each repo's Claude Code memory once a night: a
// Claude reads the day's conversations of the repo (its own sessions, its
// task builders and helpers in their worktrees) and the repo's memory, and
// merges, fixes and adds notes in a copy. The daemon checks the copy (the
// index still loads, nothing looks like a secret, not half the notes gone
// at once) and puts it in place only if no session wrote the memory
// meanwhile, after keeping the old one as a backup.
//
// Claude Code has the same thing built in (auto-dream), behind a switch on
// Anthropic's side that is off for this account; when it is on, this one
// stands down (DreamOn), so the two never tidy one memory at once.
package memtidy

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// Tidier runs the nightly tidy-up.
type Tidier struct {
	// Sessions lists the session rows, which say which repos had work and
	// in which folders.
	Sessions func() ([]store.RunnerSession, error)
	// Projects is Claude Code's projects folder (~/.claude/projects).
	Projects string
	// StateDir keeps the bookmarks, the log, the backups and the work.
	StateDir string
	// Hour is the local hour from which the night's pass is due.
	Hour int
	// Claude runs the tidy-up's Claude in dir with the prompt and returns
	// its answer and what it cost.
	Claude func(ctx context.Context, dir, prompt string) (Answer, error)
	// Enabled is a repo's switch (Switches.Enabled): whether it is tidied,
	// and when it was switched on again (zero: never switched off). A repo
	// that is off is not read at all. nil is on for every repo.
	Enabled func(repoPath string) (bool, time.Time)
	// Todo files a problem the night found coming back on the repo's
	// to-do list; nil files none.
	Todo func(repo, text, urgency string) error
	// DreamOn says Claude Code's own auto-dream is live; then this skips.
	DreamOn func() bool
	// Secrets are values never to be written into memory.
	Secrets func() []string
	now     func() time.Time
}

const (
	checkEvery = 15 * time.Minute
	// firstLook is how far back a repo's first night reads.
	firstLook  = 48 * time.Hour
	keepNights = 7
)

// Run checks every quarter hour whether tonight's pass is due. The day of
// the last pass is in a file, so a daemon restarted at night still runs it
// once.
func (t *Tidier) Run(ctx context.Context) {
	t.startTomorrow()
	tick := time.NewTicker(checkEvery)
	defer tick.Stop()
	for {
		if t.due() {
			t.Once(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// startTomorrow marks today done on a host that never ran a pass, so the
// first one comes at night, not at the deploy that brought it.
func (t *Tidier) startTomorrow() {
	if _, err := os.Stat(t.stateFile()); !errors.Is(err, os.ErrNotExist) {
		return
	}
	st := state{LastDay: t.clock().Format(time.DateOnly), Bookmarks: map[string]int64{}}
	if err := t.saveState(st); err != nil {
		log.Printf("memory tidy: %v", err)
	}
}

func (t *Tidier) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func (t *Tidier) due() bool {
	now := t.clock()
	if now.Hour() < t.Hour {
		return false
	}
	st, err := t.loadState()
	if err != nil {
		log.Printf("memory tidy: %v", err)
		return false
	}
	return st.LastDay != now.Format(time.DateOnly)
}

// Once is one night's pass over every repo. A pass cut off by the daemon
// stopping is not marked done, so it runs again; each repo's bookmark
// moves only when its memory was tidied (or there was nothing to do).
func (t *Tidier) Once(ctx context.Context) {
	if t.DreamOn != nil && t.DreamOn() {
		log.Printf("memory tidy: Claude Code's own auto-dream is on; skipping")
		t.finishNight(ctx)
		return
	}
	rows, err := t.Sessions()
	if err != nil {
		log.Printf("memory tidy: the sessions could not be read: %v", err)
		return
	}
	for _, r := range reposOf(rows) {
		if ctx.Err() != nil {
			return
		}
		if !isRepo(r.Path) {
			continue
		}
		if on, onAt := t.switchOf(r.Path); on {
			t.tidyRepo(ctx, r, onAt)
		}
	}
	t.finishNight(ctx)
}

// switchOf is a repo's switch; every repo is on without one.
func (t *Tidier) switchOf(path string) (bool, time.Time) {
	if t.Enabled == nil {
		return true, time.Time{}
	}
	return t.Enabled(path)
}

func (t *Tidier) finishNight(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	st, err := t.loadState()
	if err == nil {
		st.LastDay = t.clock().Format(time.DateOnly)
		err = t.saveState(st)
	}
	if err != nil {
		log.Printf("memory tidy: %v", err)
	}
}
