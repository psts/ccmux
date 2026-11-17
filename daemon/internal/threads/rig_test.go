package threads

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// fakeSessions is the runner supervisor over a real registry: it writes the
// rows the real one would, and notes what it was asked to start, move and
// stop instead of touching systemd.
type fakeSessions struct {
	mu        sync.Mutex
	st        *store.SQLite
	n         int
	relocated []string
	sent      map[string][]string // session id → prompts
	createErr error
	moveErr   error // what Relocate fails with, when set
}

func (f *fakeSessions) Create(n runner.NewSession) (store.RunnerSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return store.RunnerSession{}, f.createErr
	}
	f.n++
	sess := store.RunnerSession{
		ID: fmt.Sprintf("s%d", f.n), PaneID: fmt.Sprintf("p%d", f.n), Name: n.Name, Cwd: n.Cwd, Model: n.Model, Wanted: store.SessionRunning,
		WorkspaceID: n.WorkspaceID, Kind: n.Kind, Title: n.Title, ThreadID: n.ThreadID, Role: n.Role, Repo: n.Repo, ReadOnly: n.ReadOnly,
		CreatedAt: int64(f.n), UpdatedAt: int64(f.n),
	}
	return sess, f.st.AddRunnerSession(sess)
}

func (f *fakeSessions) Place(id string, p store.SessionPlace) error {
	return f.st.SetRunnerSessionPlace(id, p, 9)
}

func (f *fakeSessions) Relocate(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.moveErr != nil {
		return f.moveErr
	}
	f.relocated = append(f.relocated, id)
	return nil
}

func (f *fakeSessions) failMoves(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moveErr = err
}

func (f *fakeSessions) SetWanted(id, wanted string) error {
	return f.st.SetRunnerSessionWanted(id, wanted, 9)
}

func (f *fakeSessions) SetModel(id, m string) error { return f.st.SetRunnerSessionModel(id, m, 9) }

func (f *fakeSessions) Send(id, text, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sent == nil {
		f.sent = map[string][]string{}
	}
	f.sent[id] = append(f.sent[id], text)
	return "prompt", nil
}

func (f *fakeSessions) moves() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.relocated...)
}

func (f *fakeSessions) prompts(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent[id]...)
}

// fakeHost is a window of repo sessions.
type fakeHost struct {
	window string
	list   []*model.Workspace
}

func (h *fakeHost) Workspace(id string) *model.Workspace {
	for _, ws := range h.list {
		if ws.ID == id {
			return ws
		}
	}
	return nil
}
func (h *fakeHost) WindowOfWorkspace(string) (string, error) { return h.window, nil }

// WindowPeers: every pane the rig makes is in the one window.
func (h *fakeHost) WindowPeers(string) []*model.Workspace { return h.list }

// fakeGit is git and the repo's checks as a thread uses them: it makes and
// removes nothing, and notes what was asked for. Each failure it can play
// is named by the repo's folder name.
type fakeGit struct {
	mu          sync.Mutex
	added       []string // "repo path branch base"
	err         error    // what AddWorktree fails with
	blocked     map[string]string
	uncommitted map[string]string
	red         map[string]string // repo → its failing check's output
	landed      []string          // "repo rev trunk", in order
	removed     []string          // worktree folder names
	kept        map[string]string // worktree folder name → why it stays
	landHook    func()            // runs as the first check starts
}

func (g *fakeGit) Trunk(string) (string, error) { return "main", nil }
func (g *fakeGit) AddWorktree(repo, path, branch, base string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return g.err
	}
	g.added = append(g.added, fmt.Sprintf("%s %s %s %s", filepath.Base(repo), filepath.Base(path), branch, base))
	return nil
}

func (g *fakeGit) TrunkBlocked(repo, _ string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.blocked[filepath.Base(repo)], nil
}

func (g *fakeGit) Uncommitted(worktree string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.uncommitted[repoOfWorktree(worktree)], nil
}
func (g *fakeGit) Rebase(worktree, _ string) (string, error) {
	return "rev-" + repoOfWorktree(worktree), nil
}
func (g *fakeGit) Moved(string, string) (string, error) { return "", nil }
func (g *fakeGit) ChangedUnder(string, string, string, string) ([]string, error) {
	return nil, nil
}

func (g *fakeGit) FastForward(repo, rev, trunk string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.landed = append(g.landed, fmt.Sprintf("%s %s %s", filepath.Base(repo), rev, trunk))
	return nil
}

func (g *fakeGit) RemoveWorktree(_, path, _, _ string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if why := g.kept[repoOfWorktree(path)]; why != "" {
		return fmt.Errorf("%s", why)
	}
	g.removed = append(g.removed, repoOfWorktree(path))
	return nil
}

func (g *fakeGit) Run(repo, _, _ string) taskq.CheckResult {
	g.mu.Lock()
	hook, out := g.landHook, g.red[filepath.Base(repo)]
	g.landHook = nil
	g.mu.Unlock()
	if hook != nil {
		hook()
	}
	return taskq.CheckResult{Configured: true, OK: out == "", Output: out}
}

func (g *fakeGit) changes(worktree, _ string) (taskq.BranchChanges, error) {
	return taskq.BranchChanges{Commits: 2, Files: []taskq.FileChange{{Path: "src/" + repoOfWorktree(worktree) + ".js", Added: 3}}}, nil
}

func (g *fakeGit) set(change func(*fakeGit)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	change(g)
}

func (g *fakeGit) did(read func(*fakeGit) []string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), read(g)...)
}

// repoOfWorktree is the repo a thread worktree belongs to, by its folder:
// <root>/<repo>-<hash>/thread-<id>.
func repoOfWorktree(worktree string) string {
	dir := filepath.Base(filepath.Dir(worktree))
	return dir[:strings.LastIndex(dir, "-")]
}

type rig struct {
	t    *testing.T
	st   *store.SQLite
	sess *fakeSessions
	host *fakeHost
	git  *fakeGit
	svc  *Service
	root string // holds the repos
}

// newRig is a threads service over a real registry and three repo sessions
// (app, backend, admin) in one window.
func newRig(t *testing.T) *rig {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r := &rig{t: t, st: st, sess: &fakeSessions{st: st}, host: &fakeHost{window: "w1"}, git: &fakeGit{}, root: root}
	for _, name := range []string{"app", "backend", "admin"} {
		repo := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		r.host.list = append(r.host.list, &model.Workspace{ID: "ws-" + name, Name: name, RepoPath: repo})
	}
	r.svc = &Service{St: st, Sessions: r.sess, Host: r.host, Lander: taskq.Lander{Git: r.git, MergeGit: r.git, Checks: r.git, Tag: "threads"}, Root: filepath.Join(root, "wt"),
		Changes: r.git.changes,
		Now:     func() time.Time { return time.UnixMilli(1000) }, MoveAfter: time.Millisecond}
	return r
}

func (r *rig) repo(name string) string { return filepath.Join(r.root, name) }

// thread starts a thread led from app.
func (r *rig) thread(title string) (store.Thread, store.RunnerSession) {
	r.t.Helper()
	th, lead, err := r.svc.Create(New{WorkspaceID: "ws-app", Title: title, Model: "haiku"})
	if err != nil {
		r.t.Fatal(err)
	}
	return th, lead
}

// helper stands up the thread's helper for the repo called name and
// returns its row.
func (r *rig) helper(th store.Thread, name string) store.RunnerSession {
	r.t.Helper()
	if err := r.svc.StartHelper(th.ID, r.repo(name)); err != nil {
		r.t.Fatal(err)
	}
	members, err := r.st.ThreadMembers(th.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, m := range members {
		if m.Repo == r.repo(name) {
			return m
		}
	}
	r.t.Fatalf("no helper for %s among %+v", name, members)
	return store.RunnerSession{}
}

// edits moves a member into its worktree, as its first change does, and
// waits for the restart that follows.
func (r *rig) edits(id string) store.RunnerSession {
	r.t.Helper()
	before := len(r.sess.moves())
	if _, err := r.svc.FirstEdit(id); err != nil {
		r.t.Fatal(err)
	}
	eventually(r.t, "the member's restart into its worktree", func() bool { return len(r.sess.moves()) > before })
	return r.row(id)
}

// state reads a thread's row fresh.
func (r *rig) state(id int64) store.Thread {
	r.t.Helper()
	th, err := r.st.Thread(id)
	if err != nil || th == nil {
		r.t.Fatalf("thread %d: %+v %v", id, th, err)
	}
	return *th
}

// row reads a session fresh.
func (r *rig) row(id string) store.RunnerSession {
	r.t.Helper()
	sess, err := r.st.RunnerSession(id)
	if err != nil || sess == nil {
		r.t.Fatalf("session %s: %+v %v", id, sess, err)
	}
	return *sess
}

// eventually waits for something a goroutine does.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("never happened: %s", what)
}
