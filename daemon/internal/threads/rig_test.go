package threads

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
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
func (h *fakeHost) WindowPeers(string) []*model.Workspace    { return h.list }

// fakeGit makes no worktree: it notes what was asked for.
type fakeGit struct {
	mu    sync.Mutex
	added []string // "repo path branch base"
	err   error
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
	r.svc = &Service{St: st, Sessions: r.sess, Host: r.host, Git: r.git, Root: filepath.Join(root, "wt"),
		Now: func() time.Time { return time.UnixMilli(1000) }, MoveAfter: time.Millisecond}
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
