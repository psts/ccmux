package taskq

import (
	"context"
	"log"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// Store is the slice of the registry the runner needs.
type Store interface {
	Tasks() ([]store.Task, error)
	SaveTask(store.Task) (bool, error)
}

// Host is the daemon the workers run on: it finds a repo's session, clears
// a worktree of past workers, starts a worker's pane, reads it back, and
// tells the lenses something changed.
type Host interface {
	TaskWorkspace(repo string) (string, error)
	ClearTaskFolder(dir string) error
	StartTaskPane(wsID, cwd, cmd string) (string, error)
	TaskPane(paneID string) (PaneState, bool)
	TasksChanged()
}

// Runner starts waiting tasks while slots are free and keeps the running
// ones' status true to their panes. It decides under Lock, the lock the
// lens and tool routes take, so a person's edit and the runner never
// interleave on one task; the slow part of a start (git, a new pane) runs
// outside it, so a worker's report never waits on another task's checkout.
type Runner struct {
	Store Store
	Host  Host
	Git   Git
	Lock  sync.Locker
	Slots int    // tasks running at once, across every repo
	Root  string // where worktrees and briefs go
	Now   func() time.Time
}

// Start ticks the runner every `every` for the life of ctx.
func (r *Runner) Start(ctx context.Context, every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.Tick()
			}
		}
	}()
}

// Tick is one pass: judge the running tasks and claim what fits, under the
// lock; then start each claimed task without it.
func (r *Runner) Tick() {
	claimed, changed := r.decide()
	for _, t := range claimed {
		r.start(t)
	}
	if changed {
		r.Host.TasksChanged()
	}
}

// decide judges every running task and claims the ready ones that fit the
// free slots. A claimed task is saved as running with no pane yet: running
// is a status no person can edit or move, so nothing touches it while it
// starts, and a daemon that dies mid-start leaves a task the next pass
// fails ("it lost its pane before starting") instead of one started twice.
func (r *Runner) decide() ([]store.Task, bool) {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	all, err := r.Store.Tasks()
	if err != nil {
		log.Printf("tasks: list unreadable, runner pass skipped: %v", err)
		return nil, false
	}
	changed := r.watch(all)
	claimed := r.claim(all)
	return claimed, changed || len(claimed) > 0
}

// watch judges every running task against its pane, updating all in place.
func (r *Runner) watch(all []store.Task) bool {
	changed := false
	now := r.Now()
	for i, t := range all {
		if t.Status != Running && t.Status != NeedsYou {
			continue
		}
		st, ok := r.Host.TaskPane(t.PaneID)
		next, moved := Judge(t, st, ok, now)
		if moved && r.save(next) {
			all[i] = next
			changed = true
		}
	}
	return changed
}

// claim marks the ready tasks that fit the free slots as running.
func (r *Runner) claim(all []store.Task) []store.Task {
	free := r.Slots - CountRunning(all)
	var out []store.Task
	for _, t := range Ready(all) {
		if free <= 0 {
			break
		}
		t.Status, t.Note, t.PaneID = Running, "", ""
		t.StartedAt, t.FinishedAt = r.Now().UnixMilli(), 0
		if !r.save(t) {
			continue
		}
		out = append(out, t)
		free--
	}
	return out
}

// start builds a claimed task's worktree and starts its Claude, then saves
// where it runs, or fails it with the reason.
func (r *Runner) start(t store.Task) {
	l := layoutOf(r.Root, t)
	paneID, err := r.prepareAndStart(t, l)
	r.Lock.Lock()
	defer r.Lock.Unlock()
	if err != nil {
		log.Printf("tasks: #%d could not start: %v", t.ID, err)
		r.save(finish(t, Failed, "could not start: "+err.Error(), r.Now()))
		return
	}
	t.PaneID, t.Branch, t.Worktree = paneID, l.branch, l.worktree
	if r.save(t) {
		log.Printf("tasks: #%d started on %s in %s", t.ID, l.branch, l.worktree)
	}
}

func (r *Runner) prepareAndStart(t store.Task, l layout) (string, error) {
	wsID, err := r.Host.TaskWorkspace(t.RepoPath)
	if err != nil {
		return "", err
	}
	// One Claude per worktree: a past run's worker may still be open there,
	// tracked or not (a start whose pane was never recorded).
	if err := r.Host.ClearTaskFolder(l.worktree); err != nil {
		return "", err
	}
	trunk, err := r.Git.Trunk(t.RepoPath)
	if err != nil {
		return "", err
	}
	if err := r.Git.AddWorktree(t.RepoPath, l.worktree, l.branch, trunk); err != nil {
		return "", err
	}
	if err := copyLocalSettings(t.RepoPath, l.worktree); err != nil {
		return "", err
	}
	if err := writeBrief(t, l, trunk); err != nil {
		return "", err
	}
	return r.Host.StartTaskPane(wsID, l.worktree, launchCommand(l))
}

func (r *Runner) save(t store.Task) bool {
	t.UpdatedAt = r.Now().UnixMilli()
	if _, err := r.Store.SaveTask(t); err != nil {
		log.Printf("tasks: #%d not saved (%s): %v", t.ID, t.Status, err)
		return false
	}
	return true
}
