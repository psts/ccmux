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
	for i := range m.parts {
		if m.parts[i].ID == p.ID {
			m.parts[i] = p
			return true, nil
		}
	}
	return false, nil
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
	noSession bool
	started   []string // commands typed, in order
	cleared   []string // worktrees cleared of past workers
	clearErr  error
	panes     map[string]PaneState
	changed   int
	onStart   func()
}

func (h *fakeHost) TaskWorkspace(repo string) (string, error) {
	if h.noSession {
		return "", errors.New("no ccmux session for " + repo)
	}
	return "ws-" + filepath.Base(repo), nil
}
func (h *fakeHost) ClearTaskFolder(dir string) error {
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
func (h *fakeHost) TasksChanged()                        { h.changed++ }

type fakeGit struct{ added []string }

func (g *fakeGit) Trunk(string) (string, error) { return "dev", nil }
func (g *fakeGit) AddWorktree(repo, path, branch, base string) error {
	g.added = append(g.added, filepath.Base(repo)+":"+branch+"<-"+base)
	return os.MkdirAll(path, 0o755)
}

type rig struct {
	r    *Runner
	st   *memStore
	host *fakeHost
	git  *fakeGit
	now  time.Time
}

func newRig(t *testing.T) *rig {
	g := &rig{st: &memStore{}, host: &fakeHost{panes: map[string]PaneState{}}, git: &fakeGit{},
		now: time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC)}
	g.r = &Runner{Store: g.st, Host: g.host, Git: g.git, Lock: &sync.Mutex{}, Slots: 2,
		Root: t.TempDir(), Now: func() time.Time { return g.now }}
	return g
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
	g.r.Tick()
	if got := g.st.task(1); got.Status != ReadyToMerge || got.FinishedAt == 0 {
		t.Fatalf("all parts done = %+v", got)
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
