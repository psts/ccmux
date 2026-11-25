package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/api"
	"ccmux.dev/ccmuxd/internal/diskcheck"
	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/todos"
	"ccmux.dev/ccmuxd/internal/wtsweep"
)

// The weekly sweep of Claude Code's leftover worktrees (internal/wtsweep)
// and the disk check (internal/diskcheck), wired from here so runDaemon
// stays as it is. The flags are this file's own: runDaemon's flag.Parse
// reads them.
var (
	sweepDayFlag  = flag.String("sweep-day", "sunday", "the day of the week the weekly sweep of Claude Code's leftover worktrees (<repo>/.claude/worktrees) runs")
	sweepHourFlag = flag.Int("sweep-hour", 4, "the local hour from which the weekly sweep runs: it removes old worktrees that are clean and merged and lists the rest as to-dos (-1 never; a scratch daemon sweeps only when this is given)")
)

// sweepCheckEvery is how often the sweep looks whether it is due; the
// disks are looked at once an hour.
const (
	sweepCheckEvery = 15 * time.Minute
	diskCheckEvery  = time.Hour
	// sweepCatchUp is how long after its hour a sweep that did not run
	// (the daemon was down) still runs.
	sweepCatchUp = 24 * time.Hour
)

// startSweep starts the hourly disk check and the weekly sweep, and gives
// the sweep's jobs their handler. Called before wireJobs starts the job
// loop; the handler is there even where no sweep runs, so a sweep's job
// from before is settled.
func startSweep(ctx context.Context, w jobWiring, srv *api.Server, mgr *manager.Manager, worktreesDir string) {
	keys := &todos.Keys{}
	repos := func() []wtsweep.Repo { return knownRepos(mgr.List(), keys.Of) }
	disks := &diskcheck.Watcher{Paths: diskPaths(worktreesDir, repos), Todo: diskTodo(srv, repos)}
	srv.SetDiskHealth(func() any { return disks.Last() })
	go disks.Run(ctx, diskCheckEvery)
	if w.mgr != nil {
		w.mgr.Handle(wtsweep.Kind, wtsweep.Handler{Todo: sweepTodo(srv), Applied: w.mgr.Applied})
	}
	day, hour, err := sweepSchedule(*sweepDayFlag, *sweepHourFlag)
	switch {
	case err != nil:
		log.Printf("sweep: off: %v", err)
		return
	case hour < 0:
		log.Printf("sweep: off")
		return
	case w.mgr == nil:
		log.Printf("sweep: off, this daemon runs no jobs: %s", w.off)
		return
	}
	wk := &weekly{day: day, hour: hour, now: time.Now, last: lastSweep(w.mgr), start: func() { startSweepJob(w.mgr, repos(), disks) }}
	log.Printf("sweep: weekly on %s from %02d:00", day, hour)
	go wk.run(ctx)
}

// sweepSchedule reads the flags. A scratch daemon sweeps only when it was
// given -sweep-hour: a test must not remove the person's worktrees.
func sweepSchedule(dayFlag string, hour int) (time.Weekday, int, error) {
	if scratchRoot() != "" && !flagGiven("sweep-hour") {
		return 0, -1, nil
	}
	if hour > 23 {
		return 0, -1, fmt.Errorf("-sweep-hour %d is not an hour", hour)
	}
	day, err := parseWeekday(dayFlag)
	return day, hour, err
}

// parseWeekday takes a day's name ("sunday", "sun") or its number (0 is
// Sunday).
func parseWeekday(s string) (time.Weekday, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= 6 {
		return time.Weekday(n), nil
	}
	for d := time.Sunday; d <= time.Saturday; d++ {
		name := strings.ToLower(d.String())
		if s == name || (len(s) >= 3 && strings.HasPrefix(name, s)) {
			return d, nil
		}
	}
	return 0, fmt.Errorf("-sweep-day %q is not a day of the week", s)
}

// weekly starts the sweep once a week.
type weekly struct {
	day   time.Weekday
	hour  int
	now   func() time.Time
	last  func() (time.Time, error)
	start func()
	ran   time.Time // the slot this daemon last started a sweep for
}

func (wk *weekly) run(ctx context.Context) {
	t := time.NewTicker(sweepCheckEvery)
	defer t.Stop()
	for {
		wk.pass()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pass starts the sweep when its hour this week has come, less than a day
// ago, and no sweep has started since.
func (wk *weekly) pass() {
	now := wk.now()
	slot := lastSlot(now, wk.day, wk.hour)
	if now.Sub(slot) >= sweepCatchUp || !wk.ran.Before(slot) {
		return
	}
	last, err := wk.last()
	if err != nil {
		log.Printf("sweep: when the last one ran could not be read, so none is started: %v", err)
		return
	}
	wk.ran = slot
	if last.Before(slot) {
		wk.start()
	}
}

// lastSlot is the newest day-and-hour of the week at or before now.
func lastSlot(now time.Time, day time.Weekday, hour int) time.Time {
	slot := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	slot = slot.AddDate(0, 0, -((int(now.Weekday()) - int(day) + 7) % 7))
	if slot.After(now) {
		slot = slot.AddDate(0, 0, -7)
	}
	return slot
}

// lastSweep is when the newest sweep job started (the jobs table says,
// not this daemon's memory); the zero time for none.
func lastSweep(mgr *jobs.Manager) func() (time.Time, error) {
	return func() (time.Time, error) {
		job, err := mgr.LastJob(wtsweep.Subject)
		if err != nil || job == nil {
			return time.Time{}, err
		}
		return time.UnixMilli(job.StartedAt), nil
	}
}

// startSweepJob looks at the disks and starts the sweep's job over the
// repos that have a .claude/worktrees folder.
func startSweepJob(mgr *jobs.Manager, repos []wtsweep.Repo, disks *diskcheck.Watcher) {
	disks.Check()
	var swept []wtsweep.Repo
	for _, r := range repos {
		if isDir(filepath.Join(r.Path, ".git")) && isDir(wtsweep.Folder(r.Path)) {
			swept = append(swept, r)
		}
	}
	if len(swept) == 0 {
		log.Printf("sweep: no repo has a .claude/worktrees folder; nothing to sweep")
		return
	}
	_, err := mgr.Start(jobs.New{
		Kind: wtsweep.Kind, Subject: wtsweep.Subject, Name: wtsweep.Name,
		Input: wtsweep.Input{Repos: swept, Before: time.Now().Add(-wtsweep.MinAge).UnixMilli()},
		Limit: wtsweep.Limit,
	})
	if errors.Is(err, store.ErrJobOpen) {
		log.Printf("sweep: the last sweep's job is still open, so none started this week: %v", err)
	} else if err != nil {
		log.Printf("sweep: its job did not start: %v", err)
	}
}

func isDir(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.IsDir()
}

// knownRepos is every workspace's repo (role agents' homes left out), by
// its main folder (todos.Keys), with every spelling of it the workspaces
// use: a merge locks a repo under the path it was given.
func knownRepos(list []*model.Workspace, keyOf func(string) (string, error)) []wtsweep.Repo {
	var out []wtsweep.Repo
	at := map[string]int{}
	for _, ws := range list {
		if ws.Agent != "" || ws.RepoPath == "" {
			continue
		}
		key, err := keyOf(ws.RepoPath)
		if err != nil {
			log.Printf("sweep: which repo %s is in could not be read: %v", ws.RepoPath, err)
			continue
		}
		i, ok := at[key]
		if !ok {
			i = len(out)
			at[key] = i
			out = append(out, wtsweep.Repo{Path: key})
		}
		if spelled := filepath.Clean(ws.RepoPath); spelled != key && !contains(out[i].Locks, spelled) {
			out[i].Locks = append(out[i].Locks, spelled)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// diskPaths is the folders whose disks are looked at: the task worktrees',
// home, and every repo's.
func diskPaths(worktreesDir string, repos func() []wtsweep.Repo) func() []string {
	return func() []string {
		home, _ := os.UserHomeDir()
		paths := []string{worktreesDir, home}
		for _, r := range repos() {
			paths = append(paths, r.Path)
		}
		return paths
	}
}

// diskTodo files a low disk on ccmux's own list: the known repo whose
// main folder is called ccmux. Without one it is only logged.
func diskTodo(srv *api.Server, repos func() []wtsweep.Repo) func(prefix, text, urgency string) error {
	return func(prefix, text, urgency string) error {
		for _, r := range repos() {
			if filepath.Base(r.Path) == "ccmux" {
				_, err := srv.AddTodoOnce(r.Path, prefix, text, urgency, "disk check")
				return err
			}
		}
		return errors.New("no ccmux repo is known to this daemon to file it on")
	}
}

// sweepTodo files a kept worktree on its repo's list, once.
func sweepTodo(srv *api.Server) wtsweep.Todo {
	return func(repo, prefix, text, urgency string) error {
		_, err := srv.AddTodoOnce(repo, prefix, text, urgency, wtsweep.By)
		return err
	}
}
