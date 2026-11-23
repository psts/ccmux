package threads

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/jobs"
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
	sends     int // the prompts Send has named
	relocated []string
	sent      map[string][]string // session id → prompts
	ids       map[string]bool     // the prompt ids queued
	sendErr   error               // what SendAs fails with, when set
	createErr error
	moveErr   error // what Relocate fails with, when set
	wantFails int   // how many SetWanted calls fail before one works
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
		Worktree: n.Worktree, Branch: n.Branch, Effort: n.Effort, PermissionMode: n.PermissionMode, Advisor: n.Advisor,
		CreatedAt: int64(f.n), UpdatedAt: int64(f.n),
	}
	if n.WorkspaceID == "" {
		sess.PaneID = "" // as the supervisor: no workspace, no pane
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
	f.mu.Lock()
	failing := f.wantFails > 0
	if failing {
		f.wantFails--
	}
	f.mu.Unlock()
	if failing {
		return errors.New("systemd did not answer")
	}
	return f.st.SetRunnerSessionWanted(id, wanted, 9)
}

func (f *fakeSessions) SetModel(id, m string) error { return f.st.SetRunnerSessionModel(id, m, 9) }

func (f *fakeSessions) Send(id, text, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return "", f.sendErr
	}
	f.sends++
	return f.queue(id, fmt.Sprintf("prompt-%d", f.sends), text)
}

// SendAs queues a prompt once per id, as the supervisor does: the same id
// sent again queues nothing. A session that has ended takes none.
func (f *fakeSessions) SendAs(id, promptID, text, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return "", f.sendErr
	}
	if sess, err := f.st.RunnerSession(id); err == nil && sess != nil && sess.Wanted == store.SessionDone {
		return "", runner.ErrSessionDone
	}
	return f.queue(id, promptID, text)
}

func (f *fakeSessions) queue(id, promptID, text string) (string, error) {
	if f.sent == nil {
		f.sent, f.ids = map[string][]string{}, map[string]bool{}
	}
	if f.ids[promptID] {
		return promptID, nil
	}
	f.ids[promptID] = true
	f.sent[id] = append(f.sent[id], text)
	return promptID, nil
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

// fakeJobs is the job manager as a thread uses it, over the real registry
// (so a thread has one open merge job at a time, by the same index) and
// with no systemd: a started job lands through the rig's own Lander on its
// own goroutine, as the job's service would, and its end is handed to the
// thread's handler. A test that wants to play the daemon itself (a restart
// between a job's end and its settling) sets manual and hands the end over
// by hand.
type fakeJobs struct {
	mu       sync.Mutex
	r        *rig
	dir      string
	n        int
	startErr error
	openErr  error // what OpenJob fails with, when set
	lastErr  error // what LastJob fails with, when set
	manual   bool
	started  []jobs.New
	stopped  []string
	results  map[string]jobs.Result
	wg       sync.WaitGroup
}

func (f *fakeJobs) Start(n jobs.New) (store.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return store.Job{}, f.startErr
	}
	f.n++
	kept, _ := json.Marshal(n.Ctx)
	job := store.Job{ID: fmt.Sprintf("%s-%d", n.Name, f.n), Kind: n.Kind, Subject: n.Subject, State: store.JobRunning, Ctx: string(kept), Attempts: 1, StartedAt: int64(f.n)}
	if err := f.r.st.AddJob(job); err != nil {
		return store.Job{}, err
	}
	f.started = append(f.started, n)
	if !f.manual {
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			// The row is done before the thread hears: a test that asks for
			// the next merge the moment the thread is open finds no job in
			// its way. (The real manager writes the row right after the
			// handler, for a crash in between; here nothing crashes.)
			res := f.land(job, n.Input.(taskq.LandInput))
			f.done(job, store.JobApplied)
			f.r.svc.MergeHandler().Apply(job, res)
		}()
	}
	return job, nil
}

// land is the job itself: the landing, and its result.
func (f *fakeJobs) land(job store.Job, in taskq.LandInput) jobs.Result {
	note, err := f.r.svc.Lander.Land(in.Work)
	raw, _ := json.Marshal(taskq.LandResult(in.Work, note, err, nil))
	res := jobs.Result{ID: job.ID, Kind: job.Kind, Output: raw}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.results == nil {
		f.results = map[string]jobs.Result{}
	}
	f.results[job.ID] = res
	return res
}

func (f *fakeJobs) done(job store.Job, outcome string) {
	job.State, job.Outcome, job.EndedAt = store.JobDone, outcome, 99
	if err := f.r.st.SaveJob(job); err != nil {
		f.r.t.Errorf("job %s not marked done: %v", job.ID, err)
	}
}

func (f *fakeJobs) Stop(id, note string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, err := f.r.st.Job(id)
	if err != nil || job == nil {
		return store.ErrNoJob
	}
	job.State, job.Note = store.JobStopping, note
	f.stopped = append(f.stopped, id)
	return f.r.st.SaveJob(*job)
}

func (f *fakeJobs) OpenJob(subject string) (*store.Job, error) {
	f.mu.Lock()
	err := f.openErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.r.st.OpenJob(subject)
}

func (f *fakeJobs) LastJob(subject string) (*store.Job, error) {
	f.mu.Lock()
	err := f.lastErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.r.st.LastJob(subject)
}
func (f *fakeJobs) Unit(id string) string { return "ccmux-job-test@" + id + ".service" }

func (f *fakeJobs) Steps(id string) ([]jobs.Entry, error) {
	return jobs.ReadJournal(filepath.Join(f.dir, id, "journal.jsonl"))
}

func (f *fakeJobs) Result(id string) (jobs.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	res, ok := f.results[id]
	if !ok {
		return jobs.Result{}, os.ErrNotExist
	}
	return res, nil
}

func (f *fakeJobs) Applied(id string) (*jobs.Journal, error) {
	return jobs.OpenJournal(filepath.Join(f.dir, id, "applied.jsonl"))
}

// job reads the thread's last merge job fresh.
func (f *fakeJobs) job(threadID int64) store.Job {
	f.r.t.Helper()
	job, err := f.r.st.LastJob(mergeSubject(threadID))
	if err != nil || job == nil {
		f.r.t.Fatalf("thread %d has no merge job: %v", threadID, err)
	}
	return *job
}

type rig struct {
	jobs *fakeJobs
	// reread is the workspaces whose pages were told to read their thread
	// again (Service.Changed), in order.
	reread []string
	t      *testing.T
	st     *store.SQLite
	sess   *fakeSessions
	host   *fakeHost
	git    *fakeGit
	svc    *Service
	root   string // holds the repos
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
	r.svc.Changed = func(lead store.RunnerSession) {
		r.sess.mu.Lock()
		defer r.sess.mu.Unlock()
		r.reread = append(r.reread, lead.WorkspaceID)
	}
	r.jobs = &fakeJobs{r: r, dir: filepath.Join(root, "jobs")}
	r.svc.Jobs = r.jobs
	t.Cleanup(r.jobs.wg.Wait) // before the registry closes under a job's end
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

// rereads is the workspaces whose pages were told to read again, so far.
func (r *rig) rereads() []string {
	r.sess.mu.Lock()
	defer r.sess.mu.Unlock()
	return append([]string(nil), r.reread...)
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
	// Ten seconds, not two: what is waited for takes milliseconds, but on a
	// loaded host (the whole suite beside other work) two seconds lost once.
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("never happened: %s", what)
}
