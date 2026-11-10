package taskq

import (
	"context"
	"log"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

// Store is the slice of the registry the runner needs.
type Store interface {
	Tasks() ([]store.Task, error)
	Parts() ([]store.Part, error)
	SaveTask(store.Task) (bool, error)
	SavePart(store.Part) (bool, error)
	// SaveTaskAndPart writes both or neither (a part sent back, sendback.go).
	SaveTaskAndPart(store.Task, store.Part) (bool, error)
}

// Host is the daemon the workers run on: it finds a repo's session, clears
// a worktree of past workers, starts a worker's pane, reads it back, and
// tells the lenses something changed.
type Host interface {
	TaskWorkspace(repo, windowID string) (string, error)
	ClearTaskFolder(dir string) error
	StartTaskPane(wsID, cwd, cmd string) (string, error)
	TaskPane(paneID string) (PaneState, bool)
	TasksChanged()
	// TaskHostnames, SetTaskRoutes and DevDomain serve the workers' own dev
	// servers (serve.go).
	TaskHostnames(repo, windowID string) ([]model.Hostname, error)
	SetTaskRoutes(routes map[string]int)
	DevDomain() string
	// TellWorker sends text to the Claude in a worker's pane over the bus,
	// and errors when no Claude there can hear it (sendback.go).
	TellWorker(paneID, text string) error
}

// Runner starts waiting tasks while slots are free (a slot is a feature:
// every part of it starts at once), keeps each part's status true to its
// pane, and each task's status true to its parts. It decides under Lock,
// the lock the lens and tool routes take, so a person's edit and the runner
// never interleave; the slow part of a start (git, a new pane) runs outside
// it, so a worker's report never waits on another part's checkout.
type Runner struct {
	Store    Store
	Host     Host
	Git      Git
	MergeGit MergeGit
	Checks   Checks
	Lock     sync.Locker
	Slots    int // features running at once in a window with no number of its own
	// WindowSlots returns the windows that have their own number, window id
	// -> features at once (0 holds that window's queue). Nil means every
	// window runs Slots. An error starts nothing new that pass: falling back
	// to Slots would start a task in a window someone paused.
	WindowSlots func() (map[string]int, error)
	// TotalSlots returns the most features at once across every window, 0
	// for no cap. Nil means no cap. An error starts nothing new that pass.
	TotalSlots func() (int, error)
	Root       string // where worktrees and briefs go
	Now        func() time.Time

	merging bool           // a merge is in flight; guarded by Lock
	merges  sync.WaitGroup // the merge in flight, for tests to wait on
}

// job is one claimed part to start, with its teammates' parts and bus names.
type job struct {
	task  store.Task
	part  store.Part
	mates []store.Part
	team  []string
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
// without it, and hand a feature ready to merge to its own goroutine. (A
// cancel closes its task's Claudes itself, at once.)
func (r *Runner) Tick() {
	jobs, mj, changed := r.decide()
	for _, j := range jobs {
		r.start(j)
	}
	if mj != nil {
		r.merges.Add(1)
		go func() {
			defer r.merges.Done()
			r.merge(*mj)
		}()
	}
	if changed {
		r.Host.TasksChanged()
	}
}

// decide judges every working part, rolls each started task up from its
// parts, claims what may start, and picks a feature to merge. A claimed
// part is saved as running with no pane yet: a daemon that dies mid-start
// leaves a part the next pass fails ("it lost its pane before starting")
// instead of one started twice.
func (r *Runner) decide() ([]job, *mergeJob, bool) {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	tasks, err := r.Store.Tasks()
	if err != nil {
		log.Printf("tasks: list unreadable, runner pass skipped: %v", err)
		return nil, nil, false
	}
	parts, err := r.Store.Parts()
	if err != nil {
		log.Printf("tasks: parts unreadable, runner pass skipped: %v", err)
		return nil, nil, false
	}
	changed := r.watch(parts)
	r.publishRoutes(tasks, parts)
	if r.rollup(tasks, parts) {
		changed = true
	}
	jobs, sent := r.claim(tasks, parts)
	mj, picked := r.mergeStep(tasks, parts)
	return jobs, mj, changed || sent || picked || len(jobs) > 0
}

// mergeStep recovers a merge the daemon dropped and picks the next one,
// unless one is in flight.
func (r *Runner) mergeStep(tasks []store.Task, parts []store.Part) (*mergeJob, bool) {
	if r.merging {
		return nil, false
	}
	recovered := r.recoverMerges(tasks)
	mj, picked := r.pickMerge(tasks, parts)
	return mj, recovered || picked
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
// a person retried) and, while its window has a slot free, each next ready
// task with every one of its parts. A started task already at work holds its
// slot, so its new part starts at once; one with nothing at work (every part
// failed or retried) needs a free slot like a new task, and gets it first.
// Ahead of both, a failed merge waiting for a slot goes back to its worker
// (claimFixes). It reports whether such a send-back went out.
func (r *Runner) claim(tasks []store.Task, parts []store.Part) ([]job, bool) {
	book := r.slotBook(tasks, parts)
	sent := r.claimFixes(tasks, parts, book)
	jobs := r.claimStarted(tasks, parts, book)
	if book == nil {
		return jobs, sent
	}
	return append(jobs, r.claimNew(tasks, parts, book)...), sent
}

// claimStarted starts the pending parts of running and needs-you tasks: at
// once for a task already at work, else only into a free slot (none while
// book is nil, the numbers being unreadable). It counts what it takes.
func (r *Runner) claimStarted(tasks []store.Task, parts []store.Part, book *slotBook) []job {
	working := Working(parts)
	var jobs []job
	for _, t := range tasks {
		if t.Status != Running && t.Status != NeedsYou {
			continue
		}
		if working[t.ID] {
			jobs = append(jobs, r.claimParts(t, parts)...)
			continue
		}
		if !book.free(t.WindowID) {
			continue
		}
		if mine := r.claimParts(t, parts); len(mine) > 0 {
			jobs = append(jobs, mine...)
			book.take(t.WindowID)
		}
	}
	return jobs
}

// claimNew starts each next ready task, with every one of its parts, while
// its window and the daemon have a slot free.
func (r *Runner) claimNew(tasks []store.Task, parts []store.Part, book *slotBook) []job {
	var jobs []job
	for _, t := range Startable(tasks, parts) {
		if !book.free(t.WindowID) {
			continue
		}
		t.Status, t.Note, t.StartedAt, t.FinishedAt = Running, "", r.Now().UnixMilli(), 0
		if !r.saveTask(t) {
			continue
		}
		jobs = append(jobs, r.claimParts(t, parts)...)
		book.take(t.WindowID)
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
			mates := matesOf(p, mine)
			jobs = append(jobs, job{task: t, part: p, mates: mates, team: teamOf(t.ID, mates)})
		}
	}
	return jobs
}

// matesOf is the other parts of p's task.
func matesOf(p store.Part, mine []store.Part) []store.Part {
	var out []store.Part
	for _, o := range mine {
		if o.ID != p.ID {
			out = append(out, o)
		}
	}
	return out
}

// teamOf is the bus names of a part's teammates.
func teamOf(taskID int64, mates []store.Part) []string {
	var out []string
	for _, o := range mates {
		out = append(out, BusName(o.RepoPath, taskID))
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
	// The start-up grace and the silence limit count from the pane, not
	// the claim: a slow pass of starts must not use up a later part's grace.
	p.PaneID, p.Branch, p.Worktree, p.StartedAt = paneID, l.branch, l.worktree, r.Now().UnixMilli()
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
	wsID, err := r.Host.TaskWorkspace(repo, j.task.WindowID)
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
	servers, err := r.serversBrief(j)
	if err != nil {
		return "", err
	}
	if err := writeBrief(j.task, j.part, l, trunk, j.team, servers); err != nil {
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
