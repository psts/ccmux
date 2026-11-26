package taskq

import (
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"ccmux.dev/ccmuxd/internal/store"
)

// What the session that filed a task hears of it (task #215). When a
// task's merge lands its filer gets a quiet notice: it wakes nobody, and is
// handed over with the filer's next message, prompt or start. When a
// window's queue then has nothing open, every filer of a task that landed
// since it was last empty is woken once, with what landed, how far each
// repo's trunk is ahead of its remote, and the rule for pushing. The daemon
// never pushes anything itself.
//
// Both are said under a fixed id, which the bus keeps one message for, so
// a merge's end that is acted on again after a crash says nothing twice.
// "Since it was last empty" is a mark per window in the settings
// (drainKey): the finish time of the newest task the last drain message
// named. No migration: one setting per window that ever landed a task.

// drainKey is the setting that holds a window's mark: unix ms.
func drainKey(window string) string { return "task-drain:" + window }

// openStatuses is a task the queue still owes something: a window with one
// of these has not drained. Planning is a draft, not in the queue.
var openStatuses = map[string]bool{Waiting: true, Running: true, NeedsYou: true, ReadyToMerge: true, Merging: true, MergeFailed: true}

// builderFiler is a task builder's bus name, <repo>-task-<n>: it ends with
// its task, so there is nobody to tell.
var builderFiler = regexp.MustCompile(`-task-\d+$`)

func landedMsgID(id int64) string { return fmt.Sprintf("task-landed-%d", id) }

func drainMsgID(window string, top int64, filer string) string {
	return fmt.Sprintf("queue-drained-%s-%d-%s", window, top, filer)
}

// openIn counts the open tasks in window.
func openIn(tasks []store.Task, window string) int {
	n := 0
	for _, t := range tasks {
		if t.WindowID == window && openStatuses[t.Status] {
			n++
		}
	}
	return n
}

// repoNames is the folder names of the repos parts work in, sorted, once each.
func repoNames(parts []store.Part) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range parts {
		if name := filepath.Base(p.RepoPath); !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func landedText(t store.Task, parts []store.Part, open int) string {
	return fmt.Sprintf("Task #%d %q merged: %s. %d task(s) still open in this window's queue.",
		t.ID, t.Title, strings.Join(repoNames(parts), ", "), open)
}

// told says a merged task to its filer, and then whether its window has
// drained. Called after mergeLanded saved the row, outside the lock, every
// time that merge's end is acted on: the row decides (done, or nothing is
// said), not whether this call was the one that saved it. Nothing here
// fails the merge.
func (r *Runner) told(id int64) {
	tasks, err := r.Store.Tasks()
	if err != nil {
		log.Printf("tasks: telling filer of #%d: the tasks were not read: %v", id, err)
		return
	}
	parts, err := r.Store.Parts()
	if err != nil {
		log.Printf("tasks: telling filer of #%d: the parts were not read: %v", id, err)
		return
	}
	for _, t := range tasks {
		if t.ID != id || t.Status != Done {
			continue
		}
		r.tellFiler(t.WindowID, t.FiledBy, id, landedMsgID(id), landedText(t, PartsOf(parts, id), openIn(tasks, t.WindowID)), true)
		r.checkDrain(t.WindowID, &t)
	}
}

// tellFiler sends one message to a task's filer and logs what went wrong.
// FiledBy is the filing session's workspace name, which the host turns
// into the repo's main session on the bus.
func (r *Runner) tellFiler(window, filer string, id int64, msgID, text string, quiet bool) {
	if filer == "" || builderFiler.MatchString(filer) {
		log.Printf("tasks: telling filer of #%d: %q is no session to tell; skipped", id, filer)
		return
	}
	if err := r.Host.TellFiler(window, filer, msgID, text, quiet); err != nil {
		log.Printf("tasks: telling filer %q of #%d: %v", filer, id, err)
	}
}

// drainPass looks for a drain in every window: a cancel, a task pulled
// back to a draft or one removed empties a queue as well as a landing
// does. Outside the lock, after a pass.
func (r *Runner) drainPass() {
	r.drainMu.Lock()
	defer r.drainMu.Unlock()
	tasks, err := r.Store.Tasks()
	if err != nil {
		return // the pass said so already
	}
	seen := map[string]bool{}
	for _, t := range tasks {
		if t.Status == Done && !seen[t.WindowID] {
			seen[t.WindowID] = true
			r.drainLocked(t.WindowID, nil, tasks)
		}
	}
}

// checkDrain wakes the filers of what landed in window since its last
// drain, when nothing is open there now. landing is the task whose merge
// just landed, nil from a pass: a window with no mark yet gets one at its
// first landing (just before it), so the history from before this feature
// is never told.
func (r *Runner) checkDrain(window string, landing *store.Task) {
	r.drainMu.Lock()
	defer r.drainMu.Unlock()
	tasks, err := r.Store.Tasks()
	if err != nil {
		log.Printf("tasks: whether %q drained was not read: %v", window, err)
		return
	}
	r.drainLocked(window, landing, tasks)
}

// drainLocked is checkDrain over tasks read under drainMu.
func (r *Runner) drainLocked(window string, landing *store.Task, tasks []store.Task) {
	mark, ok := r.drainMark(window, landing)
	if !ok || openIn(tasks, window) > 0 {
		return
	}
	batch, newest := landedSince(tasks, window, mark)
	if len(batch) == 0 {
		return
	}
	parts, err := r.Store.Parts()
	if err != nil {
		log.Printf("tasks: telling filers that %s drained: the parts were not read: %v", window, err)
		return
	}
	text := r.drainText(batch, parts)
	top := batch[len(batch)-1].ID
	for _, filer := range filersOf(batch) {
		r.tellFiler(window, filer, top, drainMsgID(window, top, filer), text, false)
	}
	// Moved on whatever the sends did: each was tried once and logged, and
	// a filer that can never be told must not be tried on every pass.
	r.setDrainMark(window, newest)
}

// drainMark is window's mark, seeding it from landing when it has none.
// ok is false with no mark to go by. Under drainMu.
func (r *Runner) drainMark(window string, landing *store.Task) (int64, bool) {
	if at, ok := r.marks[window]; ok {
		return at, true
	}
	if r.unmarked[window] && landing == nil {
		return 0, false
	}
	v, err := r.Store.GetSetting(drainKey(window))
	if err != nil {
		log.Printf("tasks: the drain mark of %q was not read: %v", window, err)
		return 0, false
	}
	if v != "" {
		at, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			log.Printf("tasks: the drain mark of %q is not a time (%q)", window, v)
			return 0, false
		}
		r.rememberMark(window, at)
		return at, true
	}
	if landing == nil {
		r.unmarked = setOf(r.unmarked, window)
		return 0, false
	}
	at := landing.FinishedAt - 1
	r.setDrainMark(window, at)
	return at, true
}

// setDrainMark saves window's mark. Under drainMu.
func (r *Runner) setDrainMark(window string, at int64) {
	if err := r.Store.SetSetting(drainKey(window), strconv.FormatInt(at, 10)); err != nil {
		log.Printf("tasks: the drain mark of %q was not saved, its next drain may name these tasks again: %v", window, err)
	}
	r.rememberMark(window, at)
}

func setOf(m map[string]bool, key string) map[string]bool {
	if m == nil {
		m = map[string]bool{}
	}
	m[key] = true
	return m
}

func (r *Runner) rememberMark(window string, at int64) {
	if r.marks == nil {
		r.marks = map[string]int64{}
	}
	r.marks[window] = at
}

// landedSince is the tasks in window done after mark, by id, and the
// newest finish time among them. Where merge jobs run, done is reached
// only by a merge that landed (Rules), and only such a daemon sets marks.
func landedSince(tasks []store.Task, window string, mark int64) ([]store.Task, int64) {
	var out []store.Task
	newest := mark
	for _, t := range tasks {
		if t.WindowID == window && t.Status == Done && t.FinishedAt > mark {
			out = append(out, t)
			newest = max(newest, t.FinishedAt)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, newest
}

// filersOf is who filed the tasks, sorted, once each.
func filersOf(batch []store.Task) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range batch {
		if !seen[t.FiledBy] {
			seen[t.FiledBy] = true
			out = append(out, t.FiledBy)
		}
	}
	sort.Strings(out)
	return out
}

// pushRule is what a filer may do with what landed.
const pushRule = "Nothing has been pushed. If a repo has a `dev` branch, you may push it with /push (it runs the gates). " +
	"If a repo has only `main`, pushing is a deploy: do not push, tell the person it is ready for /ship. " +
	"The merge already ran each repo's checks on the new trunk, so you do not need to run the tests again unless something looks wrong."

// drainText is the waking message: what landed, each repo's trunk against
// its remote, and the rule for pushing.
func (r *Runner) drainText(batch []store.Task, parts []store.Part) string {
	var b strings.Builder
	b.WriteString("This window's task queue is empty. Merged since it was last empty:\n")
	var touched []store.Part
	for _, t := range batch {
		mine := PartsOf(parts, t.ID)
		touched = append(touched, mine...)
		fmt.Fprintf(&b, "- #%d %q (%s)\n", t.ID, t.Title, strings.Join(repoNames(mine), ", "))
	}
	b.WriteString("Each repo's trunk against its remote:\n")
	for _, p := range uniqueRepos(touched) {
		fmt.Fprintf(&b, "- %s: %s\n", filepath.Base(p), r.aheadLine(p))
	}
	b.WriteString(pushRule)
	return b.String()
}

// uniqueRepos is the repo paths of parts, sorted by folder name, once each.
func uniqueRepos(parts []store.Part) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range parts {
		if !seen[p.RepoPath] {
			seen[p.RepoPath] = true
			out = append(out, p.RepoPath)
		}
	}
	sort.Slice(out, func(i, j int) bool { return filepath.Base(out[i]) < filepath.Base(out[j]) })
	return out
}

// aheadLine says how far repo's trunk is ahead of its upstream, as far as
// the repo last fetched: git is asked, nothing is fetched or pushed.
func (r *Runner) aheadLine(repo string) string {
	trunk, err := r.Git.Trunk(repo)
	if err != nil {
		return "unknown (its trunk could not be read: " + err.Error() + ")"
	}
	n, upstream, err := r.Git.Unpushed(repo, trunk)
	switch {
	case err != nil:
		return fmt.Sprintf("%s, unknown (%v)", trunk, err)
	case upstream == "":
		return fmt.Sprintf("unknown (%s has no upstream)", trunk)
	}
	return fmt.Sprintf("%s is %d commit(s) ahead of %s", trunk, n, upstream)
}
