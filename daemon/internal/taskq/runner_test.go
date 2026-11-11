package taskq

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

type memStore struct {
	tasks    []store.Task
	parts    []store.Part
	tasksErr error
	partErr  error // SavePart fails with it
}

func (m *memStore) Tasks() ([]store.Task, error) {
	if m.tasksErr != nil {
		return nil, m.tasksErr
	}
	return append([]store.Task{}, m.tasks...), nil
}
func (m *memStore) Parts() ([]store.Part, error) { return append([]store.Part{}, m.parts...), nil }
func (m *memStore) SaveTask(t store.Task) (bool, error) {
	for i := range m.tasks {
		if m.tasks[i].ID == t.ID {
			m.tasks[i] = t
			return true, nil
		}
	}
	return false, nil
}
func (m *memStore) SavePart(p store.Part) (bool, error) {
	if m.partErr != nil {
		return false, m.partErr
	}
	for i := range m.parts {
		if m.parts[i].ID == p.ID {
			m.parts[i] = p
			return true, nil
		}
	}
	return false, nil
}
func (m *memStore) SaveTaskAndPart(t store.Task, p store.Part) (bool, error) {
	if m.partErr != nil {
		return false, m.partErr
	}
	ti, pi := -1, -1
	for i := range m.tasks {
		if m.tasks[i].ID == t.ID {
			ti = i
		}
	}
	for i := range m.parts {
		if m.parts[i].ID == p.ID {
			pi = i
		}
	}
	if ti < 0 || pi < 0 {
		return false, nil
	}
	m.tasks[ti], m.parts[pi] = t, p
	return true, nil
}
func (m *memStore) task(id int64) store.Task {
	for _, t := range m.tasks {
		if t.ID == id {
			return t
		}
	}
	return store.Task{}
}
func (m *memStore) part(id int64) store.Part {
	for _, p := range m.parts {
		if p.ID == id {
			return p
		}
	}
	return store.Part{}
}

type fakeHost struct {
	mu        sync.Mutex // the merge goroutine reports while a pass runs
	noSession bool
	started   []string // commands typed, in order
	cleared   []string // worktrees cleared of past workers
	clearErr  error
	panes     map[string]PaneState
	changed   int
	onStart   func()
	windows   []string                    // the window each session lookup asked for
	hostnames map[string][]model.Hostname // by repo folder name
	nameErr   map[string]error            // by repo folder name
	routes    map[string]int              // the last routes sent
	told      map[string][]string         // pane → what TellWorker sent it
	deaf      bool                        // TellWorker finds no Claude
	domain    string
}

func (h *fakeHost) TaskWorkspace(repo, windowID string) (string, error) {
	h.mu.Lock()
	h.windows = append(h.windows, windowID)
	h.mu.Unlock()
	if h.noSession {
		return "", errors.New("no ccmux session for " + repo)
	}
	return "ws-" + filepath.Base(repo), nil
}
func (h *fakeHost) ClearTaskFolder(dir string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cleared = append(h.cleared, dir)
	return h.clearErr
}
func (h *fakeHost) StartTaskPane(wsID, cwd, cmd string) (string, error) {
	if h.onStart != nil {
		h.onStart()
	}
	h.started = append(h.started, cmd)
	id := fmt.Sprintf("pane-%d", len(h.started))
	h.panes[id] = PaneState{}
	return id, nil
}
func (h *fakeHost) TaskPane(id string) (PaneState, bool) { st, ok := h.panes[id]; return st, ok }
func (h *fakeHost) TaskHostnames(repo, _ string) ([]model.Hostname, error) {
	return h.hostnames[filepath.Base(repo)], h.nameErr[filepath.Base(repo)]
}
func (h *fakeHost) SetTaskRoutes(routes map[string]int) { h.routes = routes }
func (h *fakeHost) DevDomain() string                   { return h.domain }
func (h *fakeHost) TellWorker(paneID, text string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.deaf {
		return errors.New("no Claude is on the bus in that pane")
	}
	if h.told == nil {
		h.told = map[string][]string{}
	}
	h.told[paneID] = append(h.told[paneID], text)
	return nil
}
func (h *fakeHost) TasksChanged() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.changed++
}

type fakeGit struct {
	mu          sync.Mutex // the merge goroutine calls in while a pass runs
	added       []string
	blocked     map[string]string   // repo -> why its trunk cannot move
	clash       map[string]bool     // repo -> its rebase clashes
	ffErr       map[string]error    // repo -> its fast-forward fails
	rmErr       map[string]error    // repo -> its worktree will not go
	gateChanges map[string][]string // repo -> files its branch changes under .claude
	dirty       map[string]string   // repo -> uncommitted work in its worktree
	moved       map[string]string   // repo -> how its worktree changed after the checks
	ffs         []string            // repos fast-forwarded, in order
	removed     []string            // worktrees removed after a merge
	rebased     int
	onRemove    func() // runs as a merged part's worktree is removed
}

func (g *fakeGit) TrunkBlocked(repo, trunk string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.blocked[repo], nil
}
func (g *fakeGit) ChangedUnder(repo, trunk, rev, dir string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gateChanges[repo], nil
}
func (g *fakeGit) Uncommitted(worktree string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for repo, left := range g.dirty {
		if strings.Contains(worktree, filepath.Base(repo)+"-") {
			return left, nil
		}
	}
	return "", nil
}
func (g *fakeGit) Rebase(worktree, trunk string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rebased++
	for repo, bad := range g.clash {
		if bad && strings.Contains(worktree, filepath.Base(repo)+"-") {
			return "", rebaseClash{trunk: trunk, err: errors.New("CONFLICT")}
		}
	}
	return "rev-" + filepath.Base(filepath.Dir(worktree)), nil
}
func (g *fakeGit) Moved(worktree, rev string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for repo, why := range g.moved {
		if strings.Contains(worktree, filepath.Base(repo)+"-") {
			return why, nil
		}
	}
	return "", nil
}
func (g *fakeGit) FastForward(repo, rev, trunk string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ffErr[repo]; err != nil {
		return err
	}
	g.ffs = append(g.ffs, filepath.Base(repo)+"@"+rev)
	return nil
}
func (g *fakeGit) RemoveWorktree(repo, path, branch, trunk string) error {
	if g.onRemove != nil {
		g.onRemove()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.rmErr[repo]; err != nil {
		return err
	}
	g.removed = append(g.removed, path)
	return nil
}

// fakeChecks answers per repo; a repo not listed passes. hold, when set,
// keeps every run waiting until it is closed.
type fakeChecks struct {
	results map[string]CheckResult
	hold    chan struct{}
	during  func() // runs while the checks run
}

func (c *fakeChecks) Run(repo, worktree, trunk string) CheckResult {
	if c.hold != nil {
		<-c.hold
	}
	if c.during != nil {
		c.during()
	}
	if res, ok := c.results[repo]; ok {
		return res
	}
	return CheckResult{Configured: true, OK: true}
}

func (g *fakeGit) Trunk(string) (string, error) { return "dev", nil }
func (g *fakeGit) AddWorktree(repo, path, branch, base string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.added = append(g.added, filepath.Base(repo)+":"+branch+"<-"+base)
	return os.MkdirAll(path, 0o755)
}

type rig struct {
	r      *Runner
	st     *memStore
	host   *fakeHost
	git    *fakeGit
	checks *fakeChecks
	now    time.Time
}

func newRig(t *testing.T) *rig {
	g := &rig{st: &memStore{}, host: &fakeHost{panes: map[string]PaneState{}},
		git: &fakeGit{blocked: map[string]string{}, clash: map[string]bool{}, dirty: map[string]string{}, moved: map[string]string{}, ffErr: map[string]error{}, rmErr: map[string]error{},
			gateChanges: map[string][]string{}},
		checks: &fakeChecks{results: map[string]CheckResult{}},
		now:    time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC)}
	g.r = &Runner{Store: g.st, Host: g.host, Git: g.git, MergeGit: g.git, Checks: g.checks,
		Lock: &sync.Mutex{}, Slots: 2, Root: t.TempDir(), Now: func() time.Time { return g.now }}
	return g
}

// tick runs a pass and waits for a merge it started to finish.
func (g *rig) tick() {
	g.r.Tick()
	g.r.merges.Wait()
}

// feature adds a waiting task with one pending part per repo.
func (g *rig) feature(id, after int64, repos ...string) {
	g.st.tasks = append(g.st.tasks, store.Task{ID: id, Title: fmt.Sprintf("Feature %d", id), Status: Waiting, AfterID: after})
	for _, r := range repos {
		g.st.parts = append(g.st.parts, store.Part{ID: int64(len(g.st.parts) + 1), TaskID: id,
			RepoPath: "/p/" + r, Plan: r + " plan", Status: Pending})
	}
}

func (g *rig) partsOf(id int64) []store.Part { return PartsOf(g.st.parts, id) }

// inWindow files task id in window w.
func (g *rig) inWindow(id int64, w string) {
	for i := range g.st.tasks {
		if g.st.tasks[i].ID == id {
			g.st.tasks[i].WindowID = w
		}
	}
}

// Each window has its own slots: a full window's next task waits while
// another window's starts, and its worker is looked for in its own window.
func TestRunner_SlotsArePerWindow(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.feature(1, 0, "app")
	g.feature(2, 0, "backend")
	g.feature(3, 0, "admin")
	g.inWindow(1, "w1")
	g.inWindow(2, "w1")
	g.inWindow(3, "w2")
	g.tick()
	if g.st.task(1).Status != Running || g.st.task(2).Status != Waiting || g.st.task(3).Status != Running {
		t.Fatalf("statuses = %s %s %s, want running waiting running",
			g.st.task(1).Status, g.st.task(2).Status, g.st.task(3).Status)
	}
	if strings.Join(g.host.windows, ",") != "w1,w2" {
		t.Fatalf("sessions looked for in %v, want w1 then w2", g.host.windows)
	}
}

// A window's own number beats the default: w1 set 2 runs two, w2 set 0 is
// paused and starts nothing, and w3 with no number of its own runs the
// default one.
func TestRunner_WindowsOwnSlotsBeatTheDefault(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.r.WindowSlots = func() (map[string]int, error) { return map[string]int{"w1": 2, "w2": 0}, nil }
	for i, w := range []string{"w1", "w1", "w1", "w2", "w3", "w3"} {
		g.feature(int64(i+1), 0, "app")
		g.inWindow(int64(i+1), w)
	}
	g.tick()
	want := map[int64]string{1: Running, 2: Running, 3: Waiting, 4: Waiting, 5: Running, 6: Waiting}
	for id, st := range want {
		if got := g.st.task(id).Status; got != st {
			t.Errorf("task %d = %s, want %s", id, got, st)
		}
	}
}

// Unreadable window numbers start nothing new: not the paused window's task,
// and not a task in a window with no number either. A part a running
// feature already holds a slot for still starts.
func TestRunner_UnreadableWindowSlotsStartNothingNew(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.r.WindowSlots = func() (map[string]int, error) { return nil, errors.New("database is locked") }
	g.feature(1, 0, "app")
	g.feature(2, 0, "app")
	g.feature(3, 0, "backend", "admin")
	g.inWindow(1, "w1")
	g.inWindow(2, "w2")
	g.inWindow(3, "w3")
	g.st.tasks[2].Status = Running
	g.st.parts[2].Status, g.st.parts[2].PaneID = Running, "busy" // #3's backend part is at work, so #3 holds its slot
	g.host.panes["busy"] = PaneState{Attention: model.AttentionRunning, At: 1}
	g.tick()
	if g.st.task(1).Status != Waiting || g.st.task(2).Status != Waiting {
		t.Fatalf("statuses = %s %s, want both still waiting", g.st.task(1).Status, g.st.task(2).Status)
	}
	if len(g.host.started) != 1 || g.partsOf(3)[1].Status != Running {
		t.Fatalf("started %v, want only running #3's pending part", g.host.started)
	}
}

// The slots count features: every part of a started feature starts at once,
// each told who its teammates are, and a third feature waits.
func TestRunner_StartsWholeFeatures(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app", "backend")
	g.feature(2, 0, "admin")
	g.feature(3, 0, "app")
	g.r.Tick()
	if g.st.task(1).Status != Running || g.st.task(2).Status != Running || g.st.task(3).Status != Waiting {
		t.Fatalf("tasks = %s %s %s", g.st.task(1).Status, g.st.task(2).Status, g.st.task(3).Status)
	}
	if len(g.host.started) != 3 {
		t.Fatalf("started %d Claudes, want 3 (two parts of #1, one of #2)", len(g.host.started))
	}
	app := g.partsOf(1)[0]
	if app.Branch != "task/1-feature-1" || app.PaneID == "" || !strings.HasSuffix(app.Worktree, "task-1") {
		t.Fatalf("app part = %+v", app)
	}
	brief, err := os.ReadFile(app.Worktree + ".md")
	if err != nil || !strings.Contains(string(brief), "backend-task-1") || !strings.Contains(string(brief), "app plan") ||
		!strings.Contains(string(brief), `"add_part"`) || strings.Contains(string(brief), "backend plan") {
		t.Fatalf("app brief = %q, %v", brief, err)
	}
	if !strings.Contains(g.host.started[0], "CLAUDE_PEERS_NAME='app-task-1'") || !strings.Contains(g.host.started[0], "--permission-mode auto") {
		t.Fatalf("launch = %s", g.host.started[0])
	}
}

// A feature is ready to merge only when every part reported done; until
// then it runs, and a part needing a person shows on the feature.
func TestRunner_RollsUpParts(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app", "backend")
	g.r.Tick()
	app, be := g.partsOf(1)[0], g.partsOf(1)[1]
	app.Status = Ready
	g.st.SavePart(app)
	g.r.Tick()
	if g.st.task(1).Status != Running {
		t.Fatalf("one part done = %s, want running", g.st.task(1).Status)
	}
	g.host.panes[be.PaneID] = PaneState{Attention: model.AttentionNeedsInput, Reason: model.ReasonQuestion, At: 1}
	g.r.Tick()
	if g.st.task(1).Status != NeedsYou {
		t.Fatalf("a part asking = %s, want needs_you", g.st.task(1).Status)
	}
	be = g.st.part(be.ID)
	be.Status = Ready
	g.st.SavePart(be)
	g.git.blocked["/p/app"] = "app has uncommitted changes on dev"
	g.tick()
	if got := g.st.task(1); got.Status != ReadyToMerge || !strings.Contains(got.Note, "uncommitted") {
		t.Fatalf("all parts done, a folder dirty = %+v", got)
	}
}

// doneParts marks every part of task id built, as their workers' reports do.
func (g *rig) doneParts(id int64) {
	for _, p := range g.partsOf(id) {
		p.Status = Ready
		g.st.SavePart(p)
	}
}

// usedAutoFix spends task id's automatic send-backs, so a failure its
// worker could fix stops for a person, which these merge tests pin.
func (g *rig) usedAutoFix(id int64) {
	task := g.st.task(id)
	task.AutoFixes = maxAutoFixes
	g.st.SaveTask(task)
}

// A built feature merges as one: every part rebased and checked, then every
// trunk moved forward; its workers, worktrees and briefs are cleaned up, and
// a feature that starts after it can start.
func TestMerge_AllPartsTogether(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app", "backend")
	g.feature(2, 1, "admin")
	g.tick()
	g.doneParts(1)
	cleared := len(g.host.cleared)
	g.tick()
	got := g.st.task(1)
	if got.Status != Done || got.Note != "merged app into dev, backend into dev" || got.FinishedAt == 0 {
		t.Fatalf("merged = %+v", got)
	}
	// Each trunk moves to the commit the rebase ended on, the one checked.
	if !strings.HasPrefix(strings.Join(g.git.ffs, ","), "app@rev-app-") || !strings.Contains(g.git.ffs[1], "backend@rev-backend-") || g.git.rebased != 2 {
		t.Fatalf("ffs %v, rebased %d", g.git.ffs, g.git.rebased)
	}
	if len(g.git.removed) != 2 || len(g.host.cleared)-cleared != 2 {
		t.Fatalf("cleanup: removed %v, cleared %v", g.git.removed, g.host.cleared[cleared:])
	}
	if _, err := os.Stat(g.partsOf(1)[0].Worktree + ".md"); !os.IsNotExist(err) {
		t.Fatal("the brief was left behind")
	}
	g.tick()
	if g.st.task(2).Status != Running {
		t.Fatalf("the task after it = %s, want running once it merged", g.st.task(2).Status)
	}
}

// A red check or a clash in one repo stops the whole feature before any
// trunk moves, and says where; trying again after the fix merges it.
func TestMerge_ARedPartStopsTheWholeFeature(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app", "backend")
	g.tick()
	g.doneParts(1)
	g.usedAutoFix(1)
	g.checks.results["/p/backend"] = CheckResult{Configured: true, Output: "FAIL: TestExport"}
	g.tick()
	got := g.st.task(1)
	if got.Status != MergeFailed || !strings.Contains(got.Note, "backend: checks failed") || !strings.Contains(got.Note, "TestExport") {
		t.Fatalf("red check = %+v", got)
	}
	if len(g.git.ffs) != 0 || len(g.git.removed) != 0 {
		t.Fatalf("a trunk moved or a worktree went on a failed merge: %v %v", g.git.ffs, g.git.removed)
	}
	g.tick()
	if g.st.task(1).Status != MergeFailed {
		t.Fatal("a failed merge was tried again on its own")
	}
	delete(g.checks.results, "/p/backend")
	g.git.clash["/p/app"] = true
	retry := g.st.task(1)
	retry.Status = ReadyToMerge
	g.st.SaveTask(retry)
	g.tick()
	if got := g.st.task(1); got.Status != MergeFailed || !strings.Contains(got.Note, "app: its branch clashes") {
		t.Fatalf("clash = %+v", got)
	}
	g.git.clash["/p/app"] = false
	retry.Status = ReadyToMerge
	g.st.SaveTask(retry)
	g.tick()
	if g.st.task(1).Status != Done {
		t.Fatalf("after the fixes = %+v", g.st.task(1))
	}
}

// Work left uncommitted in a worktree is not merged (only commits are) and
// would be deleted with it, so the merge stops and names it, before any
// rebase or check runs.
func TestMerge_UncommittedWorkStopsIt(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app", "backend")
	g.tick()
	g.doneParts(1)
	g.usedAutoFix(1)
	g.git.dirty["/p/backend"] = "?? export.go"
	g.tick()
	got := g.st.task(1)
	if got.Status != MergeFailed || !strings.Contains(got.Note, "backend: uncommitted work") || !strings.Contains(got.Note, "export.go") {
		t.Fatalf("uncommitted = %+v", got)
	}
	if len(g.git.ffs) != 0 || len(g.git.removed) != 0 {
		t.Fatalf("a trunk moved or a worktree went: %v %v", g.git.ffs, g.git.removed)
	}
}

// A folder that got changes while the checks ran stops the merge before
// any trunk moves, and the feature waits, as it would before a merge; it
// merges on its own once the folder is clean.
func TestMerge_FolderDirtiedDuringTheChecks(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app", "backend")
	g.tick()
	g.doneParts(1)
	g.checks.during = func() {
		g.git.mu.Lock()
		g.git.blocked["/p/app"] = "app has uncommitted changes on dev"
		g.git.mu.Unlock()
	}
	g.tick()
	got := g.st.task(1)
	if got.Status != ReadyToMerge || got.Note != "waiting to merge: app has uncommitted changes on dev" || len(g.git.ffs) != 0 {
		t.Fatalf("dirtied during checks = %+v, ffs %v", got, g.git.ffs)
	}
	g.checks.during = nil
	g.tick()
	if got := g.st.task(1); got.Status != ReadyToMerge || len(g.git.ffs) != 0 {
		t.Fatalf("merged while the folder was still dirty: %+v, ffs %v", got, g.git.ffs)
	}
	delete(g.git.blocked, "/p/app")
	g.tick()
	if g.st.task(1).Status != Done {
		t.Fatalf("clean folders = %s", g.st.task(1).Status)
	}
}

// A worker folder that moved while the checks ran in it (a new commit, or
// work left uncommitted) stops the merge before any trunk moves: the checks
// graded files that are not the commit that would land.
func TestMerge_WorktreeMovedDuringTheChecks(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app", "backend")
	g.tick()
	g.doneParts(1)
	g.usedAutoFix(1)
	g.checks.during = func() {
		g.git.mu.Lock()
		g.git.moved["/p/backend"] = "tracked files changed (M export.go)"
		g.git.mu.Unlock()
	}
	g.tick()
	got := g.st.task(1)
	if got.Status != MergeFailed || !strings.Contains(got.Note, "backend: its worktree changed while the checks ran") || len(g.git.ffs) != 0 {
		t.Fatalf("moved during checks = %+v, ffs %v", got, g.git.ffs)
	}
}

// A later repo that will not fast-forward leaves the feature half in: the
// note names what already merged, and nothing is cleaned up, so the
// unmerged part's worktree and branch are still there to finish by hand.
func TestMerge_HalfMergedIsSaidAndKept(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app", "backend")
	g.tick()
	g.doneParts(1)
	g.git.ffErr["/p/backend"] = errors.New("dev moved since the rebase; try the merge again")
	cleared := len(g.host.cleared)
	g.tick()
	got := g.st.task(1)
	if got.Status != MergeFailed || !strings.Contains(got.Note, "backend did not merge") ||
		!strings.Contains(got.Note, "already merged: app into dev") {
		t.Fatalf("half merged = %+v", got)
	}
	if len(g.git.ffs) != 1 || !strings.HasPrefix(g.git.ffs[0], "app@") {
		t.Fatalf("ffs = %v, want only app", g.git.ffs)
	}
	if len(g.git.removed) != 0 || len(g.host.cleared) != cleared {
		t.Fatalf("a half-merged feature was cleaned up: removed %v, cleared %v", g.git.removed, g.host.cleared[cleared:])
	}
}

// What cleanup could not remove is named in the done note: a worker that
// did not close keeps its worktree untouched, and a worktree git would not
// remove stays; a part with neither problem is tidied.
func TestMerge_CleanupSaysWhatItLeft(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app", "backend", "admin")
	g.tick()
	g.doneParts(1)
	g.git.rmErr["/p/backend"] = errors.New("kept, it holds uncommitted files")
	g.checks.during = func() {
		g.host.mu.Lock()
		g.host.clearErr = errors.New("pane would not close")
		g.host.mu.Unlock()
	}
	g.r.Tick()
	g.r.merges.Wait()
	// No worker closed, so no worktree may be removed, and the note says so.
	got := g.st.task(1)
	if got.Status != Done || !strings.Contains(got.Note, "left for you") {
		t.Fatalf("done with leftovers = %+v", got)
	}
	if len(g.git.removed) != 0 {
		t.Fatalf("a worktree whose worker did not close was removed: %v", g.git.removed)
	}

	g2 := newRig(t)
	g2.feature(1, 0, "app", "backend")
	g2.tick()
	g2.doneParts(1)
	g2.git.rmErr["/p/backend"] = errors.New("kept, it holds uncommitted files")
	g2.tick()
	note := g2.st.task(1).Note
	if !strings.HasPrefix(note, "merged app into dev, backend into dev; left for you: ") ||
		!strings.Contains(note, "uncommitted") || len(g2.git.removed) != 1 || !strings.Contains(g2.git.removed[0], "app-") {
		t.Fatalf("note %q, removed %v", note, g2.git.removed)
	}
}

// A part that changes the repo's gate files is not merged on its own
// checks, however green: the feature stops for a person, nothing moves, and
// the note names the files. A feature that leaves them alone merges.
func TestMerge_GateFileChangesWaitForAPerson(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app", "backend")
	g.tick()
	g.doneParts(1)
	g.git.gateChanges["/p/backend"] = []string{".claude/gates/gocyclo-frozen.txt"}
	g.tick()
	got := g.st.task(1)
	if got.Status != MergeFailed || !strings.Contains(got.Note, "backend: it changes the repo's gate files") ||
		!strings.Contains(got.Note, "gocyclo-frozen.txt") || len(g.git.ffs) != 0 {
		t.Fatalf("gate files changed = %+v, ffs %v", got, g.git.ffs)
	}
	delete(g.git.gateChanges, "/p/backend")
	got.Status = ReadyToMerge
	g.st.SaveTask(got)
	g.tick()
	if g.st.task(1).Status != Done {
		t.Fatalf("gate files left alone = %s", g.st.task(1).Status)
	}
}

// A failed merge with long check output still says which repo was red.
func TestMerge_LongFailureNamesTheRepo(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app", "backend")
	g.tick()
	g.doneParts(1)
	g.usedAutoFix(1)
	g.checks.results["/p/backend"] = CheckResult{Configured: true, Output: strings.Repeat(strings.Repeat("y", 149)+"\n", 20)}
	g.tick()
	if got := g.st.task(1); got.Status != MergeFailed || !strings.HasPrefix(got.Note, "backend: checks failed") {
		t.Fatalf("long failure note starts %q", got.Note[:40])
	}
}

// A long check failure keeps its first line, which names the repo.
func TestNoteOf_KeepsTheHeadline(t *testing.T) {
	long := "backend: checks failed:\n" + strings.Repeat(strings.Repeat("x", 149)+"\n", 20) + "(failed: go test ./...: exit status 1)"
	got := noteOf(errors.New(long))
	if !strings.HasPrefix(got, "backend: checks failed:") || !strings.HasSuffix(got, "exit status 1)") || len(got) > maxNote+8 {
		t.Fatalf("note = %q (%d bytes)", got[:60], len(got))
	}
	longHead := noteOf(errors.New("backend: " + strings.Repeat("z", maxNote) + "\n" + strings.Repeat("w", maxNote)))
	if !strings.HasPrefix(longHead, "backend: zzz") {
		t.Fatalf("a long first line lost its start: %q", longHead[:20])
	}
	if short := noteOf(errors.New("app: its branch clashes with dev")); short != "app: its branch clashes with dev" {
		t.Fatalf("a short note was changed: %q", short)
	}
}

// A repo that lists no checks is never merged on trust.
func TestMerge_NoChecksIsNoMerge(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	g.tick()
	g.doneParts(1)
	g.checks.results["/p/app"] = CheckResult{}
	g.tick()
	if got := g.st.task(1); got.Status != MergeFailed || !strings.Contains(got.Note, "merge it by hand") || len(g.git.ffs) != 0 {
		t.Fatalf("no checks = %+v, ffs %v", got, g.git.ffs)
	}
}

// One merge at a time: a second built feature waits for the first.
func TestMerge_OneAtATime(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	g.feature(2, 0, "backend")
	g.tick()
	g.doneParts(1)
	g.doneParts(2)
	g.checks.hold = make(chan struct{})
	g.r.Tick()
	g.r.Tick()
	first, second := g.st.task(1).Status, g.st.task(2).Status
	close(g.checks.hold)
	g.r.merges.Wait()
	if first != Merging || second != ReadyToMerge {
		t.Fatalf("while one merges: %s, %s", first, second)
	}
	g.tick()
	if g.st.task(1).Status != Done || g.st.task(2).Status != Done {
		t.Fatalf("after: %s, %s", g.st.task(1).Status, g.st.task(2).Status)
	}
}

// A feature keeps its slot until it is merged: with one slot, the next
// feature does not start when the first reports done, nor while its checks
// run, only once it is in the trunk. The checks and the next build never
// share the machine, and the next build starts from a trunk that has the
// first feature in it.
func TestRunner_KeepsItsSlotUntilMerged(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.feature(1, 0, "app")
	g.feature(2, 0, "backend")
	g.tick()
	g.doneParts(1)
	g.checks.hold = make(chan struct{})
	g.r.Tick() // #1 is built: its merge starts
	g.r.Tick() // a pass while its checks run
	first, second, started := g.st.task(1).Status, g.st.task(2).Status, len(g.host.started)
	close(g.checks.hold)
	g.r.merges.Wait()
	if first != Merging || second != Waiting || started != 1 {
		t.Fatalf("while #1's checks run: #1 %s, #2 %s, %d workers started; want merging, waiting, 1", first, second, started)
	}
	g.tick()
	if g.st.task(1).Status != Done || g.st.task(2).Status != Running {
		t.Fatalf("after the merge: #1 %s, #2 %s; want done, running", g.st.task(1).Status, g.st.task(2).Status)
	}
}

// A built feature that waits on a person's own folder (uncommitted changes
// on the trunk) gives its slot up: someone's work in progress must not stop
// the queue for the night. It merges once the folder is clean.
func TestRunner_ABuiltFeatureWaitingOnAFolderLeavesItsSlot(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.feature(1, 0, "app")
	g.feature(2, 0, "backend")
	g.tick()
	g.doneParts(1)
	g.git.blocked["/p/app"] = "app has uncommitted changes on dev"
	g.tick() // #1 is built and found waiting on the folder
	g.tick()
	if one := g.st.task(1); one.Status != ReadyToMerge || !strings.Contains(one.Note, "uncommitted") {
		t.Fatalf("#1 = %+v, want ready to merge and saying why it waits", one)
	}
	if g.st.task(2).Status != Running {
		t.Fatalf("#2 = %s, want running in the slot #1 gave up", g.st.task(2).Status)
	}
	delete(g.git.blocked, "/p/app")
	g.tick()
	if g.st.task(1).Status != Done {
		t.Fatalf("#1 once the folder was clean = %s, want done", g.st.task(1).Status)
	}
}

// A folder that turns dirty while the checks run sends the feature back to
// wait for it, and that wait holds no slot either.
func TestRunner_AFolderDirtiedDuringTheChecksLeavesTheSlot(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.feature(1, 0, "app")
	g.feature(2, 0, "backend")
	g.tick()
	g.doneParts(1)
	g.checks.during = func() {
		g.git.mu.Lock()
		g.git.blocked["/p/app"] = "app has uncommitted changes on dev"
		g.git.mu.Unlock()
	}
	g.tick() // #1's checks pass, the folder is dirty by then
	g.tick()
	if g.st.task(1).Status != ReadyToMerge || g.st.task(2).Status != Running {
		t.Fatalf("#1 %s, #2 %s; want ready to merge and #2 running", g.st.task(1).Status, g.st.task(2).Status)
	}
}

// A feature whose folder turned dirty during its checks gives its slot up
// at once, also while another feature's merge keeps every pass from looking
// at it: with #1 merging, #2 (waiting on a person's folder since its own
// checks) holds nothing, and the next task in its window starts.
func TestRunner_AFolderDirtiedDuringTheChecksIsParkedAtOnce(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.feature(1, 0, "app")
	g.feature(2, 0, "backend")
	g.inWindow(1, "w1")
	g.inWindow(2, "w2")
	g.tick() // one feature builds in each window
	g.doneParts(1)
	g.git.blocked["/p/app"] = "app has uncommitted changes on dev"
	g.tick() // #1 waits on its folder
	g.doneParts(2)
	g.checks.during = func() {
		g.git.mu.Lock()
		g.git.blocked["/p/backend"] = "backend has uncommitted changes on dev"
		g.git.mu.Unlock()
	}
	g.tick() // #2's checks pass, its folder is dirty by then
	g.checks.during = nil
	if two := g.st.task(2); two.Status != ReadyToMerge || !strings.Contains(two.Note, "backend has uncommitted") {
		t.Fatalf("setup: #2 = %s note %q, want back in line waiting on its folder", two.Status, two.Note)
	}
	delete(g.git.blocked, "/p/app") // #1's folder is clean; #2's is not
	g.feature(3, 0, "admin")
	g.inWindow(3, "w2")
	g.checks.hold = make(chan struct{})
	g.r.Tick() // #1 is picked to merge: the pass never gets to #2
	first, second, third := g.st.task(1).Status, g.st.task(2).Status, g.st.task(3).Status
	close(g.checks.hold)
	g.r.merges.Wait()
	if first != Merging || second != ReadyToMerge || third != Running {
		t.Fatalf("#1 %s, #2 %s, #3 %s; want merging, ready to merge, and #3 running in the slot #2 gave up", first, second, third)
	}
}

// A feature that leaves its wait on a folder merges alone too. The pass
// that finds the folder clean starts its merge before it hands out slots,
// so a task filed meanwhile does not start beside the checks.
func TestRunner_AFeatureLeavingAFolderWaitMergesAlone(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.feature(1, 0, "app")
	g.tick()
	g.doneParts(1)
	g.git.blocked["/p/app"] = "app has uncommitted changes on dev"
	g.tick() // #1 waits on the folder, holding no slot
	delete(g.git.blocked, "/p/app")
	g.feature(2, 0, "backend")
	g.checks.hold = make(chan struct{})
	g.r.Tick() // the folder is clean and #2 is new, in one pass
	first, second, started := g.st.task(1).Status, g.st.task(2).Status, len(g.host.started)
	close(g.checks.hold)
	g.r.merges.Wait()
	if first != Merging || second != Waiting || started != 1 {
		t.Fatalf("#1 %s, #2 %s, %d workers started; want merging, waiting, 1", first, second, started)
	}
}

// A merged feature is done before its tidy-up starts: removing a worktree
// can take minutes, and a daemon stopped in the middle (every deploy stops
// it) must not find the feature still merging and merge it a second time,
// against a folder half gone. Its note says the tidy-up is not finished
// until it is, so one cut short stays visible; then the note says what was
// left. The merge line is taken until the tidy-up ends, and free after.
func TestMerge_IsDoneBeforeTheTidyUp(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	g.tick()
	g.doneParts(1)
	var during store.Task
	g.git.onRemove = func() { during = g.st.task(1) }
	g.git.rmErr["/p/app"] = errors.New("left half removed")
	g.tick()
	if during.Status != Done || during.Note != "merged app into dev; tidy-up not finished" || during.FinishedAt == 0 {
		t.Fatalf("while the worktree is removed: %+v, want done, saying what merged and that the tidy-up is not finished", during)
	}
	got := g.st.task(1)
	if got.Status != Done || !strings.Contains(got.Note, "left for you") || !strings.Contains(got.Note, "left half removed") ||
		strings.Contains(got.Note, "tidy-up not finished") {
		t.Fatalf("after the tidy-up: %+v, want done and saying what was left", got)
	}
	// The next feature merges: the line was freed although something was left.
	g.git.onRemove = nil
	g.feature(2, 0, "backend")
	g.tick()
	g.doneParts(2)
	g.tick()
	if two := g.st.task(2); two.Status != Done || two.Note != "merged backend into dev" {
		t.Fatalf("the next feature = %s note %q, want merged with a clean note", two.Status, two.Note)
	}
}

// A wait on a folder is forgotten once the feature leaves it. Built again
// later (cancelled and queued again, say) while another window's merge is
// in flight, nobody looks at its folder that pass, and it must hold its
// slot like any other built feature rather than count as still waiting.
func TestRunner_AnOldFolderWaitIsForgotten(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.feature(1, 0, "app")
	g.feature(9, 0, "admin")
	g.inWindow(1, "w1")
	g.inWindow(9, "w2")
	g.tick() // one feature builds in each window
	g.doneParts(1)
	g.git.blocked["/p/app"] = "app has uncommitted changes on dev"
	g.tick() // #1 waits on the folder, and gives w1's slot up
	one := g.st.task(1)
	one.Status = Cancelled
	g.st.SaveTask(one)
	g.tick() // a pass sees it out of the merge line
	g.doneParts(9)
	g.checks.hold = make(chan struct{})
	g.r.Tick() // w2's feature is merging: the merge line is taken
	delete(g.git.blocked, "/p/app")
	one.Status = ReadyToMerge // #1 is built again
	g.st.SaveTask(one)
	g.feature(2, 0, "backend")
	g.inWindow(2, "w1")
	g.r.Tick() // no merge step looks at #1 this pass
	nine, first, second := g.st.task(9).Status, g.st.task(1).Status, g.st.task(2).Status
	close(g.checks.hold)
	g.r.merges.Wait()
	if nine != Merging || first != ReadyToMerge || second != Waiting {
		t.Fatalf("#9 %s, #1 %s, #2 %s; want merging, ready to merge, and #2 waiting: #1 holds w1's slot again", nine, first, second)
	}
}

// built reports part p done at the rig's clock, as its worker's own report
// does (Report), and leaves its pane showing st.
func (g *rig) built(p store.Part, st PaneState) {
	next, err := Report(p, "done", "built", g.now.UnixMilli())
	if err != nil {
		panic(err)
	}
	g.st.SavePart(next)
	g.host.panes[p.PaneID] = st
}

// The merge waits for the worker's turn to end. A worker that says done is
// often sent straight back to work by its own Stop hook (a receipt to sign:
// the whole test suite again, in the worktree the merge is about to check),
// and the two runs then fail each other. So a Stop is not enough: the checks
// start once Claude's idle reminder says the turn really ended. The feature
// keeps its slot while it waits.
func TestMerge_WaitsForTheWorkersTurnToEnd(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.feature(1, 0, "app")
	g.feature(2, 0, "backend")
	g.tick()
	app := g.partsOf(1)[0]
	g.built(app, PaneState{Attention: model.AttentionDone, Reason: model.ReasonFinished, At: g.now.UnixMilli()})
	g.tick()
	g.tick()
	one := g.st.task(1)
	if one.Status != ReadyToMerge || one.Note != noteSettling || g.git.rebased != 0 {
		t.Fatalf("right after the report: %s note %q, %d rebases; want waiting on its worker, nothing touched", one.Status, one.Note, g.git.rebased)
	}
	if g.st.task(2).Status != Waiting {
		t.Fatalf("#2 = %s, want waiting: #1 holds the slot while its worker winds down", g.st.task(2).Status)
	}
	g.now = g.now.Add(61 * time.Second)
	g.host.panes[app.PaneID] = PaneState{Attention: model.AttentionNeedsInput, Reason: model.ReasonFinished, At: g.now.UnixMilli()}
	g.tick()
	if got := g.st.task(1); got.Status != Done || got.Note != "merged app into dev" {
		t.Fatalf("after the idle reminder = %s note %q, want merged", got.Status, got.Note)
	}
}

// A worker that never sends the reminder does not hold its feature for
// good: after SettleLimit with no sign the merge goes ahead.
func TestMerge_AWorkerThatNeverGoesQuietIsNotWaitedOnForGood(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	g.tick()
	app := g.partsOf(1)[0]
	g.built(app, PaneState{Attention: model.AttentionDone, Reason: model.ReasonFinished, At: g.now.UnixMilli()})
	g.now = g.now.Add(SettleLimit - time.Second)
	g.tick()
	if got := g.st.task(1).Status; got != ReadyToMerge {
		t.Fatalf("inside the limit = %s, want still waiting", got)
	}
	g.now = g.now.Add(2 * time.Second)
	g.tick()
	if got := g.st.task(1).Status; got != Done {
		t.Fatalf("past the limit = %s, want merged", got)
	}
}

func TestSettled(t *testing.T) {
	now := time.Date(2026, 11, 11, 23, 0, 0, 0, time.UTC)
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	p := store.Part{Status: Ready, PaneID: "p", FinishedAt: ms(-2 * time.Minute)}
	reminder := func(at int64) PaneState {
		return PaneState{Attention: model.AttentionNeedsInput, Reason: model.ReasonFinished, At: at}
	}
	cases := []struct {
		name string
		p    store.Part
		st   PaneState
		ok   bool
		want bool
	}{
		{"idle reminder since the report", p, reminder(ms(-time.Minute)), true, true},
		{"an idle reminder from before the report", p, reminder(ms(-3 * time.Minute)), true, false},
		{"only a Stop since the report", p, PaneState{Attention: model.AttentionDone, Reason: model.ReasonFinished, At: ms(-time.Minute)}, true, false},
		{"prompted since the report (a review woke it)", p, PaneState{Attention: model.AttentionIdle, At: ms(-time.Minute)}, true, false},
		{"asking since the report", p, PaneState{Attention: model.AttentionNeedsInput, Reason: model.ReasonPermission, At: ms(-time.Minute)}, true, false},
		{"no sign since long before the limit", store.Part{Status: Ready, PaneID: "p", FinishedAt: ms(-SettleLimit - time.Minute)}, PaneState{}, true, true},
		{"a sign inside the limit restarts the wait", store.Part{Status: Ready, PaneID: "p", FinishedAt: ms(-SettleLimit - time.Minute)},
			PaneState{Attention: model.AttentionDone, At: ms(-time.Minute)}, true, false},
		{"its session closed", p, PaneState{}, false, true},
		{"Claude exited", p, PaneState{AtShell: true}, true, true},
		{"no worker", store.Part{Status: Ready, FinishedAt: ms(-time.Second)}, PaneState{}, true, true},
	}
	for _, c := range cases {
		if got := Settled(c.p, c.st, c.ok, now); got != c.want {
			t.Errorf("%s: settled = %v, want %v", c.name, got, c.want)
		}
	}
}

// A check that fails leaves its whole output in a file beside the worktree
// for the worker to read (the last lines of a test run are often a summary
// table, not the failure), and the failure names the file. A later green
// run removes it, and so does the clean-up after the merge.
func TestMerge_AFailedCheckLeavesItsWholeOutput(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	g.tick()
	g.doneParts(1)
	g.usedAutoFix(1)
	g.checks.results["/p/app"] = CheckResult{Configured: true, Output: "the summary table", Full: "FAIL: TestRoute at line 111\nthe summary table"}
	g.tick()
	app := g.partsOf(1)[0]
	logFile := app.Worktree + ".check.log"
	got, err := os.ReadFile(logFile)
	if err != nil || !strings.Contains(string(got), "TestRoute at line 111") {
		t.Fatalf("check log = %q, %v", got, err)
	}
	// The file is named after the failure, not in its first line: a lens row
	// shows only that line, and a long path there hides what failed.
	if note := g.st.task(1).Note; !strings.HasPrefix(note, "app: checks failed:\n") || !strings.Contains(note, "the summary table") ||
		!strings.HasSuffix(note, "(the whole output is in "+logFile+")") {
		t.Fatalf("the failure does not name the file last: %q", note)
	}
	delete(g.checks.results, "/p/app")
	one := g.st.task(1)
	one.Status = ReadyToMerge
	g.st.SaveTask(one)
	g.tick()
	if g.st.task(1).Status != Done {
		t.Fatalf("second try = %s", g.st.task(1).Status)
	}
	if _, err := os.Stat(logFile); !os.IsNotExist(err) {
		t.Fatalf("the old check log is still there after a green merge: %v", err)
	}
}

// A check that passed only on its second run still merges. The done note
// names it and the file that holds what its failed run printed, and that
// file outlives the tidy-up: a test that fails now and then is merged, and
// can still be found.
func TestMerge_SaysWhichChecksNeededASecondRun(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app", "backend")
	g.tick()
	g.doneParts(1)
	g.checks.results["/p/backend"] = CheckResult{Configured: true, OK: true,
		Flaked: []Flake{{Line: "npm test", Output: "FAIL redact.test.ts:111"}, {Line: "bun test", Output: "(fail) corpus > slow"}}}
	backend := g.partsOf(1)[1]
	flaky := backend.Worktree + ".flaky.log"
	g.tick()
	got := g.st.task(1)
	want := "merged app into dev, backend into dev; backend: npm test, bun test failed once and passed when run again (the failed output: " + flaky + ")"
	if got.Status != Done || got.Note != want {
		t.Fatalf("merged after a second run = %s note %q, want %q", got.Status, got.Note, want)
	}
	kept, err := os.ReadFile(flaky)
	if err != nil || !strings.Contains(string(kept), "redact.test.ts:111") || !strings.Contains(string(kept), "corpus > slow") ||
		!strings.Contains(string(kept), "npm test") {
		t.Fatalf("the failed runs' output after the tidy-up = %q, %v", kept, err)
	}
	if _, err := os.Stat(g.partsOf(1)[0].Worktree + ".flaky.log"); !os.IsNotExist(err) {
		t.Fatalf("the part whose checks passed first time got a flaky file: %v", err)
	}
}

// A check that flaked is kept and logged even when a later check fails the
// merge: the failure goes to the worker, the flake is still on record.
func TestMerge_AFlakeIsKeptWhenALaterCheckFails(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	g.tick()
	g.doneParts(1)
	g.usedAutoFix(1)
	g.checks.results["/p/app"] = CheckResult{Configured: true, Output: "FAIL: TestRoute",
		Flaked: []Flake{{Line: "npm test", Output: "FAIL redact.test.ts:111"}}}
	g.tick()
	app := g.partsOf(1)[0]
	if got := g.st.task(1); got.Status != MergeFailed || !strings.Contains(got.Note, "TestRoute") {
		t.Fatalf("task = %s note %q, want merge failed on the later check", got.Status, got.Note)
	}
	if kept, err := os.ReadFile(app.Worktree + ".flaky.log"); err != nil || !strings.Contains(string(kept), "redact.test.ts:111") {
		t.Fatalf("the flake's output = %q, %v; want it kept", kept, err)
	}
}

// A merge the daemon dropped mid-way (it stopped) is picked up again.
func TestMerge_ResumesAfterARestart(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	g.tick()
	g.doneParts(1)
	task := g.st.task(1)
	task.Status = Merging
	g.st.SaveTask(task)
	g.tick()
	if g.st.task(1).Status != Done {
		t.Fatalf("a dropped merge = %s", g.st.task(1).Status)
	}
}

// A part added to a running feature (a worker pulling in another repo)
// starts on the next pass without taking a slot.
func TestRunner_AddedPartStartsWithoutASlot(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.feature(1, 0, "app")
	g.feature(2, 0, "backend")
	g.r.Tick()
	g.st.parts = append(g.st.parts, store.Part{ID: 99, TaskID: 1, RepoPath: "/p/admin", Plan: "a setting", Status: Pending})
	g.r.Tick()
	if g.st.part(99).Status != Running || g.st.task(2).Status != Waiting {
		t.Fatalf("added part %s, task 2 %s", g.st.part(99).Status, g.st.task(2).Status)
	}
	brief, _ := os.ReadFile(g.st.part(99).Worktree + ".md")
	if !strings.Contains(string(brief), "app-task-1") {
		t.Fatalf("the added part does not know its team: %q", brief)
	}
}

// A retried part of a feature that holds no slot (none of its parts at
// work) waits for a free one like a new feature, retries ahead of new work:
// with one slot, #1's retry starts, #2's waits, and waiting #3 stays put. A
// retry on a feature already at work (#4 in a full window) still starts.
func TestRunner_RetryWaitsForASlot(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.feature(1, 0, "app")
	g.feature(2, 0, "app")
	g.feature(3, 0, "app")
	g.feature(4, 0, "app", "backend")
	g.inWindow(4, "w2")
	for _, id := range []int64{1, 2} {
		g.st.tasks[id-1].Status = NeedsYou
		g.st.parts[id-1].Status = Pending // what Retry leaves on a failed part
	}
	g.st.tasks[3].Status = Running
	g.st.parts[3].Status, g.st.parts[3].PaneID = Running, "busy" // #4's app part at work holds w2's slot
	g.host.panes["busy"] = PaneState{Attention: model.AttentionRunning, At: 1}
	g.st.parts[4].Status = Pending // #4's backend part retried
	g.tick()
	if g.partsOf(1)[0].Status != Running || g.partsOf(2)[0].Status != Pending || g.st.task(3).Status != Waiting {
		t.Fatalf("#1 part %s, #2 part %s, #3 %s; want running, pending, waiting",
			g.partsOf(1)[0].Status, g.partsOf(2)[0].Status, g.st.task(3).Status)
	}
	if g.partsOf(4)[1].Status != Running {
		t.Fatalf("#4's retried part = %s, want running: its feature already holds a slot", g.partsOf(4)[1].Status)
	}
}

// Unreadable window numbers hold a retry on a feature with nothing at work
// (#1, which would otherwise take w1's free slot), while a retry on one at
// work (#2) still starts.
func TestRunner_UnreadableSlotsHoldARetry(t *testing.T) {
	g := newRig(t)
	g.r.WindowSlots = func() (map[string]int, error) { return nil, errors.New("database is locked") }
	g.feature(1, 0, "app")
	g.feature(2, 0, "app", "backend")
	g.inWindow(2, "w2")
	g.st.tasks[0].Status, g.st.tasks[1].Status = NeedsYou, Running
	g.st.parts[1].Status, g.st.parts[1].PaneID = Running, "busy"
	g.host.panes["busy"] = PaneState{Attention: model.AttentionRunning, At: 1}
	g.tick()
	if g.partsOf(1)[0].Status != Pending || g.partsOf(2)[1].Status != Running || len(g.host.started) != 1 {
		t.Fatalf("#1 part %s, #2 retried part %s, started %v; want pending, running, one start",
			g.partsOf(1)[0].Status, g.partsOf(2)[1].Status, g.host.started)
	}
}

// A feature waiting on a person, with nothing to retry, holds no slot: the
// next task in its window starts. A question asked at night must not stall
// the window.
func TestRunner_QuestionWithNothingToRetryLeavesTheSlot(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.feature(1, 0, "app")
	g.feature(2, 0, "app")
	g.st.tasks[0].Status = NeedsYou
	g.st.parts[0].Status, g.st.parts[0].PaneID, g.st.parts[0].Note = NeedsYou, "asking", noteAsking
	g.host.panes["asking"] = PaneState{Attention: model.AttentionNeedsInput, Reason: model.ReasonQuestion, At: 1}
	g.tick()
	if g.st.task(2).Status != Running || g.partsOf(1)[0].Status != NeedsYou {
		t.Fatalf("#2 %s, #1 part %s; want #2 running and #1 left to its person", g.st.task(2).Status, g.partsOf(1)[0].Status)
	}
}

func TestRunner_WaitsForWhatATaskStartsAfter(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "backend")
	g.feature(2, 1, "app")
	g.r.Tick()
	if g.st.task(2).Status != Waiting {
		t.Fatal("task 2 started before task 1 was done")
	}
	one := g.st.task(1)
	one.Status = ReadyToMerge
	g.st.SaveTask(one)
	g.r.Tick()
	if g.st.task(2).Status != Waiting {
		t.Fatal("task 2 started on unmerged work")
	}
	one.Status = Done
	g.st.SaveTask(one)
	g.r.Tick()
	if g.st.task(2).Status != Running {
		t.Fatalf("task 2 = %s once task 1 was done", g.st.task(2).Status)
	}
}

// A task cancelled while its part was starting gets no worker left behind:
// the cancel found no pane to close, so the start closes the one it made.
func TestRunner_CancelledWhileStarting(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	g.host.onStart = func() {
		task := g.st.task(1)
		task.Status = Cancelled
		g.st.SaveTask(task)
	}
	g.r.Tick()
	p := g.partsOf(1)[0]
	if p.PaneID != "" {
		t.Fatalf("a cancelled task's part recorded a pane: %+v", p)
	}
	// Cleared twice: before the start, then the worker the cancel missed.
	if c := g.host.cleared; len(c) != 2 || c[0] != c[1] || !strings.HasSuffix(c[1], "task-1") {
		t.Fatalf("cleared %v, want the new worker's worktree cleared again", c)
	}
}

// When the list cannot be read right after a start, the start closes its
// new worker and says why, rather than guess the task is still wanted; a
// start that can read it keeps its worker.
func TestRunner_UnreadableListAfterAStartClosesTheWorker(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	g := newRig(t)
	g.feature(1, 0, "app")
	g.feature(2, 0, "backend")
	g.host.onStart = func() {
		if len(g.host.started) == 0 { // the first start only
			g.st.tasksErr = errors.New("database is locked")
		} else {
			g.st.tasksErr = nil
		}
	}
	g.r.Tick()
	if p := g.partsOf(1)[0]; p.PaneID != "" {
		t.Fatalf("a start that could not check for a cancel recorded its pane: %+v", p)
	}
	if !strings.Contains(logged.String(), "#1: list unreadable after a start") {
		t.Fatalf("nothing logged: %q", logged.String())
	}
	if p := g.partsOf(2)[0]; p.PaneID == "" || p.Status != Running {
		t.Fatalf("a start that could check lost its worker: %+v", p)
	}
}

// A part that cannot start fails with the reason, and the feature asks for
// a person; the other part still runs.
func TestRunner_APartThatCannotStart(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app", "backend")
	g.host.clearErr = nil
	g.host.noSession = true
	g.r.Tick()
	g.r.Tick()
	got := g.partsOf(1)[0]
	if got.Status != Failed || !strings.Contains(got.Note, "no ccmux session") || g.st.task(1).Status != NeedsYou {
		t.Fatalf("part = %+v, task %s", got, g.st.task(1).Status)
	}
}

// A part queued again keeps the branch it was built on, whatever the title
// says now; its worktree is cleared of past workers first, and a worktree
// that cannot be cleared gets no second Claude.
func TestRunner_RetryKeepsBranchAndClearsTheWorktree(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	g.r.Tick()
	first := g.partsOf(1)[0]
	task := g.st.task(1)
	task.Title = "A new title"
	g.st.SaveTask(task)
	again := first
	again.Status = Pending
	g.st.SavePart(again)
	g.r.Tick()
	got := g.st.part(first.ID)
	if got.Branch != first.Branch || got.Status != Running || g.host.cleared[len(g.host.cleared)-1] != first.Worktree {
		t.Fatalf("retry = %+v, cleared %v", got, g.host.cleared)
	}
	got.Status = Pending
	g.st.SavePart(got)
	g.host.clearErr = errors.New("pane would not close")
	before := len(g.host.started)
	g.r.Tick()
	if p := g.st.part(first.ID); p.Status != Failed || len(g.host.started) != before {
		t.Fatalf("an uncleared worktree got a new worker: %+v", p)
	}
}

// The grace for a new pane counts from when the pane started, not from the
// claim: a pass whose starts took longer than the grace does not fail its
// last part, still at its shell while Claude comes up.
func TestRunner_GraceCountsFromThePane(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app", "backend")
	g.host.onStart = func() { g.now = g.now.Add(StartGrace) } // each start is slow
	g.r.Tick()
	g.host.onStart = nil
	last := g.partsOf(1)[1]
	g.host.panes[last.PaneID] = PaneState{AtShell: true}
	g.now = g.now.Add(time.Second)
	g.r.Tick()
	if p := g.st.part(last.ID); p.Status != Running {
		t.Fatalf("a part whose pane just started = %+v", p)
	}
	first := g.partsOf(1)[0]
	g.host.panes[first.PaneID] = PaneState{AtShell: true}
	g.now = g.now.Add(StartGrace)
	g.r.Tick()
	if p := g.st.part(first.ID); p.Status != Failed {
		t.Fatalf("a part at its shell past the grace = %s", p.Status)
	}
}

// The slow part of a start runs without the lock the routes take.
func TestRunner_StartsOutsideTheLock(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	lock := &probeLock{}
	g.r.Lock = lock
	g.host.onStart = func() {
		if lock.held {
			t.Error("a pane was started while the task lock was held")
		}
	}
	g.r.Tick()
	if g.partsOf(1)[0].Status != Running {
		t.Fatalf("part = %s", g.partsOf(1)[0].Status)
	}
}

type probeLock struct{ held bool }

func (p *probeLock) Lock()   { p.held = true }
func (p *probeLock) Unlock() { p.held = false }

// A worker that ended its turn long ago without reporting is asked once,
// over the bus, to report: most often it is waiting for something that
// never wakes it (a clean commit review sends nothing). It stays running
// meanwhile. Only a second quiet spell asks for a person.
func TestRunner_AQuietWorkerIsNudgedOnceBeforeAPerson(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	g.tick()
	app := g.partsOf(1)[0]
	stop := func() {
		g.host.panes[app.PaneID] = PaneState{Attention: model.AttentionDone, Reason: model.ReasonFinished, At: g.now.UnixMilli()}
		g.now = g.now.Add(QuietLimit + time.Minute)
	}
	stop()
	g.tick()
	got, told := g.st.part(app.ID), g.host.told[app.PaneID]
	if got.Status != Running || got.Note != noteNudged || g.st.task(1).Status != Running {
		t.Fatalf("quiet once: part %s note %q, task %s; want still running, nudged", got.Status, got.Note, g.st.task(1).Status)
	}
	if len(told) != 1 || !strings.Contains(told[0], "Task #1") || !strings.Contains(told[0], `"report"`) || !strings.Contains(told[0], "nothing is going to wake you") {
		t.Fatalf("the worker was told %q", told)
	}
	// The message starts a turn; a pass during it changes nothing.
	g.host.panes[app.PaneID] = PaneState{Attention: model.AttentionIdle, At: g.now.UnixMilli()}
	g.tick()
	if got := g.st.part(app.ID); got.Status != Running || len(g.host.told[app.PaneID]) != 1 {
		t.Fatalf("while it answers: part %s, told %d times", got.Status, len(g.host.told[app.PaneID]))
	}
	stop() // it stopped again, still without a report
	g.tick()
	got = g.st.part(app.ID)
	if got.Status != NeedsYou || got.Note != noteQuiet || len(g.host.told[app.PaneID]) != 1 || g.st.task(1).Status != NeedsYou {
		t.Fatalf("quiet twice: part %s note %q, told %d, task %s; want needs you, told once", got.Status, got.Note, len(g.host.told[app.PaneID]), g.st.task(1).Status)
	}
}

// A quiet worker nobody can reach on the bus asks for a person at once.
func TestRunner_AQuietWorkerNobodyCanReachNeedsAPerson(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	g.tick()
	app := g.partsOf(1)[0]
	g.host.deaf = true
	g.host.panes[app.PaneID] = PaneState{Attention: model.AttentionDone, Reason: model.ReasonFinished, At: g.now.UnixMilli()}
	g.now = g.now.Add(QuietLimit + time.Minute)
	g.tick()
	if got := g.st.part(app.ID); got.Status != NeedsYou || got.Note != noteQuiet {
		t.Fatalf("deaf quiet worker: %s note %q, want needs you", got.Status, got.Note)
	}
}

// A nudged worker that reports done merges like any other, and after a
// failed merge sent it back it may be nudged again: the note that held the
// nudge is gone.
func TestRunner_ANudgeIsPerRun(t *testing.T) {
	g := newRig(t)
	g.feature(1, 0, "app")
	g.tick()
	app := g.partsOf(1)[0]
	quiet := func() {
		g.host.panes[app.PaneID] = PaneState{Attention: model.AttentionDone, Reason: model.ReasonFinished, At: g.now.UnixMilli()}
		g.now = g.now.Add(QuietLimit + time.Minute)
		g.tick()
	}
	quiet()
	g.checks.results["/p/app"] = CheckResult{Configured: true, Output: "FAIL: TestRoute"}
	g.doneParts(1)
	g.tick() // merges, fails, goes back to its worker
	if got := g.st.part(app.ID); got.Status != Running || got.Note != noteSentBack {
		t.Fatalf("after the failed merge: %s note %q, want sent back", got.Status, got.Note)
	}
	quiet()
	if got, told := g.st.part(app.ID), g.host.told[app.PaneID]; got.Status != Running || got.Note != noteNudged || len(told) != 3 {
		t.Fatalf("quiet after a send-back: %s note %q, told %d times; want nudged again (nudge, send-back, nudge)", got.Status, got.Note, len(told))
	}
}

// The brief tells a worker to take its own recommendation and say so, to
// stop only for what cannot be undone or guessed, to finish everything
// before it reports, and to run nothing after. These lines are the policy
// the queue's hands-off running rests on; the plan comes last, under them.
func TestBrief_DecideReportLastRunNothingAfter(t *testing.T) {
	task := store.Task{ID: 7, Title: "Export"}
	part := store.Part{ID: 3, TaskID: 7, RepoPath: "/p/app", Plan: "HARD RULE: if the test needs an edit, report stuck."}
	l := layoutOf("/root", task, part)
	brief := briefText(task, part, l, "dev", nil, "")
	for _, want := range []string{
		"take it, and go on",
		"DECISIONS I MADE",
		"stop and propose a redesign: choose the one you would propose",
		"Report stuck only when",
		"cannot be undone from this branch",
		"you have no recommendation",
		"Do not ask with a question card",
		"not yours to waive",
		"sign it before you report",
		"A clean review sends you nothing",
		"After the report, run nothing in this folder",
		`status "stuck"`,
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q", want)
		}
	}
	if d, plan := strings.Index(brief, "## Decisions are yours"), strings.Index(brief, "HARD RULE"); d < 0 || plan < d {
		t.Fatalf("the decisions section (at %d) must come before the plan (at %d)", d, plan)
	}
	if strings.Contains(brief, "%!") {
		t.Fatalf("a format verb went wrong:\n%s", brief)
	}
}

func TestJudge(t *testing.T) {
	now := time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC)
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	run := store.Part{ID: 1, Status: Running, PaneID: "p", StartedAt: ms(-time.Minute), UpdatedAt: ms(-time.Minute)}
	asked := store.Part{Status: NeedsYou, PaneID: "p", Note: noteAsking, UpdatedAt: ms(-5 * time.Minute)}
	stuck := store.Part{Status: NeedsYou, PaneID: "p", Note: "need a key", UpdatedAt: ms(-5 * time.Minute)}
	question := PaneState{Attention: model.AttentionNeedsInput, Reason: model.ReasonQuestion, At: ms(-time.Minute)}
	stopNow := PaneState{Attention: model.AttentionDone, Reason: model.ReasonFinished, At: ms(-time.Minute)}
	cases := []struct {
		name   string
		task   store.Part
		st     PaneState
		ok     bool
		want   string
		change bool
	}{
		{"working, no signal yet", run, PaneState{}, true, Running, false},
		{"silent too long", store.Part{Status: Running, PaneID: "p", StartedAt: ms(-SilentLimit - time.Minute)}, PaneState{}, true, NeedsYou, true},
		{"silent too long, then a sign", store.Part{Status: NeedsYou, PaneID: "p", Note: noteSilent, UpdatedAt: ms(-2 * time.Minute)}, stopNow, true, Running, true},
		{"pane gone or session closed", run, PaneState{}, false, Failed, true},
		{"at a shell inside the grace", run, PaneState{AtShell: true}, true, Running, false},
		{"asks a question", run, question, true, NeedsYou, true},
		{"finished a turn just now", run, stopNow, true, Running, false},
		{"quiet too long", run, PaneState{Attention: model.AttentionDone, At: ms(-QuietLimit - time.Minute)}, true, NeedsYou, true},
		{"idle reminder counts as finished, not asking", run, PaneState{Attention: model.AttentionNeedsInput, Reason: model.ReasonFinished, At: ms(-time.Minute)}, true, Running, false},
		{"question answered", asked, stopNow, true, Running, true},
		{"still asking", asked, question, true, NeedsYou, false},
		{"a stuck report's own Stop is no answer", stuck, stopNow, true, NeedsYou, false},
		{"a person typed to a stuck worker", stuck, PaneState{Attention: model.AttentionIdle, At: ms(-time.Minute)}, true, Running, true},
		{"a signal older than the last change", stuck, PaneState{Attention: model.AttentionIdle, At: ms(-10 * time.Minute)}, true, NeedsYou, false},
		{"a built part is left alone", store.Part{Status: Ready, PaneID: "p"}, PaneState{}, false, Ready, false},
		{"asked to report, answering", store.Part{Status: Running, PaneID: "p", Note: noteNudged, StartedAt: ms(-time.Hour), UpdatedAt: ms(-NudgeLimit + time.Minute)},
			PaneState{Attention: model.AttentionIdle, At: ms(-NudgeLimit + time.Minute)}, true, Running, false},
		{"asked to report, no sign since", store.Part{Status: Running, PaneID: "p", Note: noteNudged, StartedAt: ms(-2 * time.Hour), UpdatedAt: ms(-NudgeLimit - time.Minute)},
			PaneState{Attention: model.AttentionIdle, At: ms(-NudgeLimit - time.Minute)}, true, NeedsYou, true},
		{"asked to report long ago, then a person typed and a long turn runs: not timed",
			store.Part{Status: Running, PaneID: "p", Note: noteNudged, StartedAt: ms(-5 * time.Hour), UpdatedAt: ms(-4 * time.Hour)},
			PaneState{Attention: model.AttentionIdle, At: ms(-NudgeLimit - time.Minute)}, true, Running, false},
		{"a person typed long ago, a long turn since: never timed", store.Part{Status: Running, PaneID: "p", StartedAt: ms(-3 * time.Hour)},
			PaneState{Attention: model.AttentionIdle, At: ms(-2 * time.Hour)}, true, Running, false},
	}
	for _, c := range cases {
		got, changed := Judge(c.task, c.st, c.ok, now)
		if got.Status != c.want || changed != c.change {
			t.Errorf("%s: %s changed=%v, want %s changed=%v", c.name, got.Status, changed, c.want, c.change)
		}
	}
}

func TestReport(t *testing.T) {
	run := store.Part{ID: 1, Status: Running}
	got, err := Report(run, "done", "built it", 5)
	if err != nil || got.Status != Ready || got.Note != "built it" || got.FinishedAt != 5 {
		t.Fatalf("done = %+v, %v", got, err)
	}
	if got, _ := Report(run, "stuck", "need a key", 5); got.Status != NeedsYou {
		t.Fatalf("stuck = %+v", got)
	}
	if _, err := Report(store.Part{Status: Pending}, "done", "", 5); err == nil {
		t.Fatal("a pending part took a report")
	}
	if _, err := Report(run, "maybe", "", 5); err == nil {
		t.Fatal("an unknown report was taken")
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Export button!":           "export-button",
		"  ---  ":                  "task",
		"Åäö & CSV export":         "csv-export",
		strings.Repeat("abc ", 20): "abc-abc-abc-abc-abc-abc-abc-abc-abc-abc",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

// The machine-wide cap holds across windows: each window may run one, but
// with a total of 2 only the first two windows' tasks start; the third
// waits for a slot anywhere. No cap (0) leaves the windows alone, and an
// unreadable cap starts nothing new.
func TestRunner_TotalCapAcrossWindows(t *testing.T) {
	g := newRig(t)
	g.r.Slots = 1
	g.r.TotalSlots = func() (int, error) { return 2, nil }
	for i, w := range []string{"w1", "w2", "w3"} {
		g.feature(int64(i+1), 0, "app")
		g.inWindow(int64(i+1), w)
	}
	g.tick()
	if g.st.task(1).Status != Running || g.st.task(2).Status != Running || g.st.task(3).Status != Waiting {
		t.Fatalf("with a total of 2: %s %s %s, want running running waiting",
			g.st.task(1).Status, g.st.task(2).Status, g.st.task(3).Status)
	}
	// A later pass starts from what earlier ones left running: #4 arrives
	// in a fourth window with room of its own, and the cap still holds it.
	g.feature(4, 0, "app")
	g.inWindow(4, "w4")
	g.tick()
	if g.st.task(1).Status != Running || g.st.task(2).Status != Running || g.st.task(3).Status != Waiting || g.st.task(4).Status != Waiting {
		t.Fatalf("next pass at a full cap: %s %s %s %s, want running running waiting waiting",
			g.st.task(1).Status, g.st.task(2).Status, g.st.task(3).Status, g.st.task(4).Status)
	}
	g.r.TotalSlots = func() (int, error) { return 0, errors.New("database is locked") }
	g.tick()
	if g.st.task(3).Status != Waiting {
		t.Fatalf("unreadable cap started #3: %s", g.st.task(3).Status)
	}
	g.r.TotalSlots = func() (int, error) { return 0, nil }
	g.tick()
	if g.st.task(3).Status != Running {
		t.Fatalf("no cap: #3 = %s, want running in its own window's slot", g.st.task(3).Status)
	}
}
