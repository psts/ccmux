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
	Parts() ([]store.Part, error)
	SaveTask(store.Task) (bool, error)
	SavePart(store.Part) (bool, error)
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

// Runner starts waiting tasks while slots are free (a slot is a feature:
// every part of it starts at once), keeps each part's status true to its
// pane, and each task's status true to its parts. It decides under Lock,
// the lock the lens and tool routes take, so a person's edit and the runner
// never interleave; the slow part of a start (git, a new pane) runs outside
// it, so a worker's report never waits on another part's checkout.
type Runner struct {
	Store Store
	Host  Host
	Git   Git
	Lock  sync.Locker
	Slots int    // features running at once, across every repo
	Root  string // where worktrees and briefs go
	Now   func() time.Time
}

// job is one claimed part to start, with the bus names of its teammates.
type job struct {
	task store.Task
	part store.Part
	team []string
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

// Tick is one pass: decide under the lock, then start the claimed parts
// without it. (A cancel closes its task's Claudes itself, at once.)
func (r *Runner) Tick() {
	jobs, changed := r.decide()
	for _, j := range jobs {
		r.start(j)
	}
	if changed {
		r.Host.TasksChanged()
	}
}

// decide judges every working part, rolls each started task up from its
// parts, and claims what may start. A claimed
// part is saved as running with no pane yet: a daemon that dies mid-start
// leaves a part the next pass fails ("it lost its pane before starting")
// instead of one started twice.
func (r *Runner) decide() ([]job, bool) {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	tasks, err := r.Store.Tasks()
	if err != nil {
		log.Printf("tasks: list unreadable, runner pass skipped: %v", err)
		return nil, false
	}
	parts, err := r.Store.Parts()
	if err != nil {
		log.Printf("tasks: parts unreadable, runner pass skipped: %v", err)
		return nil, false
	}
	changed := r.watch(parts)
	if r.rollup(tasks, parts) {
		changed = true
	}
	jobs := r.claim(tasks, parts)
	return jobs, changed || len(jobs) > 0
}

// watch judges every working part against its pane, updating parts in place.
func (r *Runner) watch(parts []store.Part) bool {
	changed := false
	now := r.Now()
	for i, p := range parts {
		if p.Status != Running && p.Status != NeedsYou {
			continue
		}
		st, ok := r.Host.TaskPane(p.PaneID)
		if next, moved := Judge(p, st, ok, now); moved && r.savePart(next) {
			parts[i] = next
			changed = true
		}
	}
	return changed
}

// rollup sets each started task's status from its parts.
func (r *Runner) rollup(tasks []store.Task, parts []store.Part) bool {
	changed := false
	for i, t := range tasks {
		if t.Status != Running && t.Status != NeedsYou && t.Status != ReadyToMerge {
			continue
		}
		s := Rollup(PartsOf(parts, t.ID))
		if s == t.Status {
			continue
		}
		t.Status = s
		if s == ReadyToMerge {
			t.FinishedAt = r.Now().UnixMilli()
		}
		if r.saveTask(t) {
			tasks[i] = t
			changed = true
		}
	}
	return changed
}

// claim starts the pending parts of started tasks (a part a worker added, or
// a person retried) and, while slots are free, the next ready tasks with
// every one of their parts.
func (r *Runner) claim(tasks []store.Task, parts []store.Part) []job {
	var jobs []job
	for _, t := range tasks {
		if t.Status == Running || t.Status == NeedsYou {
			jobs = append(jobs, r.claimParts(t, parts)...)
		}
	}
	free := r.Slots - SlotsHeld(tasks, parts)
	for _, t := range Startable(tasks, parts) {
		if free <= 0 {
			break
		}
		t.Status, t.Note, t.StartedAt, t.FinishedAt = Running, "", r.Now().UnixMilli(), 0
		if !r.saveTask(t) {
			continue
		}
		jobs = append(jobs, r.claimParts(t, parts)...)
		free--
	}
	return jobs
}

// claimParts marks a task's pending parts running and returns them as jobs.
func (r *Runner) claimParts(t store.Task, parts []store.Part) []job {
	mine := PartsOf(parts, t.ID)
	var jobs []job
	for _, p := range mine {
		if p.Status != Pending {
			continue
		}
		p.Status, p.Note, p.PaneID = Running, "", ""
		p.StartedAt, p.FinishedAt = r.Now().UnixMilli(), 0
		if r.savePart(p) {
			jobs = append(jobs, job{task: t, part: p, team: teamOf(t.ID, p, mine)})
		}
	}
	return jobs
}

// teamOf is the bus names of a part's teammates.
func teamOf(taskID int64, p store.Part, mine []store.Part) []string {
	var out []string
	for _, o := range mine {
		if o.ID != p.ID {
			out = append(out, BusName(o.RepoPath, taskID))
		}
	}
	return out
}

// start builds a claimed part's worktree and starts its Claude, then saves
// where it runs, or fails the part with the reason.
func (r *Runner) start(j job) {
	l := layoutOf(r.Root, j.task, j.part)
	paneID, err := r.prepareAndStart(j, l)
	r.Lock.Lock()
	defer r.Lock.Unlock()
	p := j.part
	if err != nil {
		log.Printf("tasks: #%d part in %s could not start: %v", j.task.ID, p.RepoPath, err)
		r.savePart(finish(p, Failed, "could not start: "+err.Error(), r.Now()))
		return
	}
	if r.cancelled(j.task.ID) {
		// Cancelled while it was starting: the cancel found no pane to close.
		if err := r.Host.ClearTaskFolder(l.worktree); err != nil {
			log.Printf("tasks: #%d was cancelled while starting; its new worker did not close: %v", j.task.ID, err)
		}
		return
	}
	p.PaneID, p.Branch, p.Worktree = paneID, l.branch, l.worktree
	if r.savePart(p) {
		log.Printf("tasks: #%d part started on %s in %s", j.task.ID, l.branch, l.worktree)
	}
}

// cancelled reports whether task id is cancelled now. When that cannot be
// read it answers yes, like a task that is gone: a start that closes its own
// worker shows on the next pass ("it lost its pane before starting"), while
// one that guessed wrong would leave a Claude running on a cancelled task.
func (r *Runner) cancelled(id int64) bool {
	tasks, err := r.Store.Tasks()
	if err != nil {
		log.Printf("tasks: #%d: list unreadable after a start, closing the new worker to be safe: %v", id, err)
		return true
	}
	for _, t := range tasks {
		if t.ID == id {
			return t.Status == Cancelled
		}
	}
	return true // removed
}

func (r *Runner) prepareAndStart(j job, l layout) (string, error) {
	repo := j.part.RepoPath
	wsID, err := r.Host.TaskWorkspace(repo)
	if err != nil {
		return "", err
	}
	// One Claude per worktree: a past run's worker may still be open there,
	// tracked or not (a start whose pane was never recorded).
	if err := r.Host.ClearTaskFolder(l.worktree); err != nil {
		return "", err
	}
	trunk, err := r.Git.Trunk(repo)
	if err != nil {
		return "", err
	}
	if err := r.Git.AddWorktree(repo, l.worktree, l.branch, trunk); err != nil {
		return "", err
	}
	if err := copyLocalSettings(repo, l.worktree); err != nil {
		return "", err
	}
	if err := writeBrief(j.task, j.part, l, trunk, j.team); err != nil {
		return "", err
	}
	return r.Host.StartTaskPane(wsID, l.worktree, launchCommand(l))
}

func (r *Runner) saveTask(t store.Task) bool {
	t.UpdatedAt = r.Now().UnixMilli()
	if _, err := r.Store.SaveTask(t); err != nil {
		log.Printf("tasks: #%d not saved (%s): %v", t.ID, t.Status, err)
		return false
	}
	return true
}

func (r *Runner) savePart(p store.Part) bool {
	p.UpdatedAt = r.Now().UnixMilli()
	if _, err := r.Store.SavePart(p); err != nil {
		log.Printf("tasks: #%d part in %s not saved (%s): %v", p.TaskID, p.RepoPath, p.Status, err)
		return false
	}
	return true
}
