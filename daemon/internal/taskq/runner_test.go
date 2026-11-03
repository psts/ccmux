package taskq

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

type memStore struct{ tasks []store.Task }

func (m *memStore) Tasks() ([]store.Task, error) { return append([]store.Task{}, m.tasks...), nil }
func (m *memStore) SaveTask(t store.Task) (bool, error) {
	for i := range m.tasks {
		if m.tasks[i].ID == t.ID {
			m.tasks[i] = t
			return true, nil
		}
	}
	return false, nil
}
func (m *memStore) get(id int64) store.Task {
	for _, t := range m.tasks {
		if t.ID == id {
			return t
		}
	}
	return store.Task{}
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
func (h *fakeHost) StartTaskPane(wsID, cwd, cmd string) (string, error) {
	if h.onStart != nil {
		h.onStart()
	}
	h.started = append(h.started, cmd)
	id := fmt.Sprintf("pane-%s-%d", filepath.Base(cwd), len(h.started))
	h.panes[id] = PaneState{}
	return id, nil
}
func (h *fakeHost) ClearTaskFolder(dir string) error {
	h.cleared = append(h.cleared, dir)
	return h.clearErr
}
func (h *fakeHost) TaskPane(id string) (PaneState, bool) { st, ok := h.panes[id]; return st, ok }
func (h *fakeHost) TasksChanged()                        { h.changed++ }

type fakeGit struct{ added []string }

func (g *fakeGit) Trunk(string) (string, error) { return "dev", nil }
func (g *fakeGit) AddWorktree(repo, path, branch, base string) error {
	g.added = append(g.added, branch+"<-"+base)
	return os.MkdirAll(path, 0o755)
}

type rig struct {
	r    *Runner
	st   *memStore
	host *fakeHost
	git  *fakeGit
	now  time.Time
}

func newRig(t *testing.T, tasks ...store.Task) *rig {
	g := &rig{st: &memStore{tasks: tasks}, host: &fakeHost{panes: map[string]PaneState{}}, git: &fakeGit{},
		now: time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC)}
	g.r = &Runner{Store: g.st, Host: g.host, Git: g.git, Lock: &sync.Mutex{}, Slots: 2,
		Root: t.TempDir(), Now: func() time.Time { return g.now }}
	return g
}

func waiting(id int64, after int64) store.Task {
	return store.Task{ID: id, RepoPath: "/p/admin", Title: "Task number " + string(rune('0'+id)), Plan: "do it",
		Status: Waiting, AfterID: after}
}

func TestRunner_StartsUpToTheSlotsInOrder(t *testing.T) {
	g := newRig(t, waiting(1, 0), waiting(2, 0), waiting(3, 0))
	g.r.Tick()
	if g.st.get(1).Status != Running || g.st.get(2).Status != Running || g.st.get(3).Status != Waiting {
		t.Fatalf("after one pass: %s %s %s", g.st.get(1).Status, g.st.get(2).Status, g.st.get(3).Status)
	}
	one := g.st.get(1)
	if one.Branch != "task/1-task-number-1" || one.PaneID == "" || !strings.HasSuffix(one.Worktree, "task-1") {
		t.Fatalf("task 1 = %+v", one)
	}
	if g.git.added[0] != "task/1-task-number-1<-dev" || g.host.changed != 1 {
		t.Fatalf("git = %v, changed = %d", g.git.added, g.host.changed)
	}
	brief, err := os.ReadFile(strings.TrimSuffix(one.Worktree, "task-1") + "task-1.md")
	if err != nil || !strings.Contains(string(brief), "do it") || !strings.Contains(string(brief), `status "done"`) {
		t.Fatalf("brief = %q, %v", brief, err)
	}
	cmd := g.host.started[0]
	for _, want := range []string{"--permission-mode auto", "CLAUDE_PEERS_NAME='admin-task-1'", "server:claude-peers", "--add-dir"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("launch command lacks %q: %s", want, cmd)
		}
	}
	// A slot frees when a task leaves running; the next pass fills it.
	g.host.panes[one.PaneID] = PaneState{AtShell: true}
	g.now = g.now.Add(StartGrace + time.Second)
	g.r.Tick()
	if g.st.get(1).Status != Failed || g.st.get(3).Status != Running {
		t.Fatalf("after task 1 exited: %s, task 3 %s", g.st.get(1).Status, g.st.get(3).Status)
	}
}

func TestRunner_WaitsForWhatATaskStartsAfter(t *testing.T) {
	g := newRig(t, waiting(1, 0), waiting(2, 1))
	g.r.Tick()
	if g.st.get(2).Status != Waiting {
		t.Fatalf("task 2 started before task 1 was done: %s", g.st.get(2).Status)
	}
	one := g.st.get(1)
	one.Status = ReadyToMerge
	g.st.SaveTask(one)
	g.r.Tick()
	if g.st.get(2).Status != Waiting {
		t.Fatal("task 2 started on unmerged work")
	}
	one.Status = Done
	g.st.SaveTask(one)
	g.r.Tick()
	if g.st.get(2).Status != Running {
		t.Fatalf("task 2 did not start once task 1 was done: %s", g.st.get(2).Status)
	}
}

func TestRunner_ANeedsYouTaskFreesItsSlot(t *testing.T) {
	g := newRig(t, waiting(1, 0), waiting(2, 0), waiting(3, 0))
	g.r.Slots = 1
	g.r.Tick()
	g.host.panes[g.st.get(1).PaneID] = PaneState{Attention: model.AttentionNeedsInput, Reason: model.ReasonQuestion, At: 1}
	g.r.Tick()
	if g.st.get(1).Status != NeedsYou || g.st.get(2).Status != Running {
		t.Fatalf("a question held the queue: %s, %s", g.st.get(1).Status, g.st.get(2).Status)
	}
}

func TestRunner_NoSessionFailsTheTaskWithTheReason(t *testing.T) {
	g := newRig(t, waiting(1, 0))
	g.host.noSession = true
	g.r.Tick()
	got := g.st.get(1)
	if got.Status != Failed || !strings.Contains(got.Note, "no ccmux session") {
		t.Fatalf("task = %+v", got)
	}
}

func TestJudge(t *testing.T) {
	now := time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC)
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	run := store.Task{ID: 1, Status: Running, PaneID: "p", StartedAt: ms(-time.Minute), UpdatedAt: ms(-time.Minute)}
	asked := store.Task{Status: NeedsYou, PaneID: "p", Note: noteAsking, UpdatedAt: ms(-5 * time.Minute)}
	stuck := store.Task{Status: NeedsYou, PaneID: "p", Note: "need a key", UpdatedAt: ms(-5 * time.Minute)}
	question := PaneState{Attention: model.AttentionNeedsInput, Reason: model.ReasonQuestion, At: ms(-time.Minute)}
	stopNow := PaneState{Attention: model.AttentionDone, Reason: model.ReasonFinished, At: ms(-time.Minute)}
	cases := []struct {
		name   string
		task   store.Task
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
		{"ready to merge is left alone", store.Task{Status: ReadyToMerge, PaneID: "p"}, PaneState{}, false, ReadyToMerge, false},
	}
	for _, c := range cases {
		got, changed := Judge(c.task, c.st, c.ok, now)
		if got.Status != c.want || changed != c.change {
			t.Errorf("%s: %s changed=%v, want %s changed=%v", c.name, got.Status, changed, c.want, c.change)
		}
	}
}

// A task queued again keeps the branch it was built on, whatever its title
// says now, and its worktree is cleared of past workers before a new one
// starts there; a worktree that cannot be cleared gets no second Claude.
func TestRunner_RetryKeepsBranchAndClearsTheWorktree(t *testing.T) {
	g := newRig(t, waiting(1, 0), waiting(2, 0))
	g.r.Tick()
	first := g.st.get(1)
	if len(g.host.cleared) != 2 || g.host.cleared[0] != first.Worktree {
		t.Fatalf("cleared %v before the first starts, want each worktree", g.host.cleared)
	}
	first.Status, first.Title = Waiting, "A new title"
	g.st.SaveTask(first)
	g.r.Tick()
	again := g.st.get(1)
	if again.Branch != first.Branch || again.Status != Running || g.host.cleared[2] != first.Worktree {
		t.Fatalf("retry = %+v, cleared %v", again, g.host.cleared)
	}
	again.Status = Waiting
	g.st.SaveTask(again)
	g.host.clearErr = errors.New("pane would not close")
	before := len(g.host.started)
	g.r.Tick()
	if got := g.st.get(1); got.Status != Failed || len(g.host.started) != before {
		t.Fatalf("an uncleared worktree got a new worker: %+v, starts %d -> %d", got, before, len(g.host.started))
	}
	if g.st.get(2).Status != Running {
		t.Fatal("another task's worker was touched")
	}
}

// The slow part of a start runs without the lock the routes take.
func TestRunner_StartsOutsideTheLock(t *testing.T) {
	g := newRig(t, waiting(1, 0))
	lock := &probeLock{}
	g.r.Lock = lock
	g.host.onStart = func() { lock.check(t) }
	g.r.Tick()
	if g.st.get(1).Status != Running {
		t.Fatalf("task = %s", g.st.get(1).Status)
	}
}

type probeLock struct{ held bool }

func (p *probeLock) Lock()   { p.held = true }
func (p *probeLock) Unlock() { p.held = false }
func (p *probeLock) check(t *testing.T) {
	if p.held {
		t.Error("a pane was started while the task lock was held")
	}
}

func TestReport(t *testing.T) {
	run := store.Task{ID: 1, Status: Running}
	got, err := Report(run, "done", "built it", 5)
	if err != nil || got.Status != ReadyToMerge || got.Note != "built it" || got.FinishedAt != 5 {
		t.Fatalf("done = %+v, %v", got, err)
	}
	if got, _ := Report(run, "stuck", "need a key", 5); got.Status != NeedsYou {
		t.Fatalf("stuck = %+v", got)
	}
	if _, err := Report(store.Task{ID: 1, Status: Waiting}, "done", "", 5); err == nil {
		t.Fatal("a waiting task took a report")
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
