package api

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// movedRows is a session table for a move: one of each kind of row in the
// moved repo, and one in another repo.
type movedRows struct {
	fakeRunnerRows
	mu   sync.Mutex
	wsID string // the moved repo's, once it is made
}

func (m *movedRows) set(id string) { m.mu.Lock(); m.wsID = id; m.mu.Unlock() }

func (m *movedRows) RunnerSessions() ([]store.RunnerSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return []store.RunnerSession{
		{ID: "lead", WorkspaceID: m.wsID, Wanted: store.SessionRunning},
		{ID: "thread", WorkspaceID: m.wsID, Wanted: store.SessionRunning, Role: store.RoleLead},
		{ID: "asleep", WorkspaceID: m.wsID, Wanted: store.SessionAsleep},
		{ID: "builder", WorkspaceID: m.wsID, Wanted: store.SessionRunning, Role: store.RoleBuilder},
		{ID: "other", WorkspaceID: "ws-other", Wanted: store.SessionRunning},
	}, nil
}

// restartLog is a supervisor that notes restarts, safe to read while the
// settle's goroutine writes.
type restartLog struct {
	fakeRunners
	mu  sync.Mutex
	ids []string
}

func (r *restartLog) Restart(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
	return nil
}

func (r *restartLog) got() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.ids, ",")
}

// createWSAt makes another plain session of the repo folder repo.
func createWSAt(t *testing.T, f *windowAgentFixture, repo string) string {
	t.Helper()
	resp, err := http.Post(f.base+"/v1/workspaces", "application/json",
		strings.NewReader(`{"name":"app","repoPath":`+strconv.Quote(repo)+`,"createdBy":"tester","startupCommand":""}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var ws wsResult
	json.NewDecoder(resp.Body).Decode(&ws)
	return ws.ID
}

// A repo moved to another window loses the old window's shared folder and
// gets the new one's, and its running sessions restart: not an asleep one,
// not a task's builder, not another repo's. A repo put into its first window
// is a move too (its sessions knew no window's lists).
func TestPutGroup_MoveSettlesAccessAndRestartsTheRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	f := newWindowAgentFixture(t, "sleep 1;:")
	sup, rows := &restartLog{}, &movedRows{}
	f.srv.SetRunners(sup, rows)
	repo := gitRepoAt(t)
	wsID := createWSAt(t, f, repo)
	rows.set(wsID)
	f.put(t, "/v1/workspaces/"+wsID+"/group", `{"group":"Chart Labs"}`, 204)
	settings := filepath.Join(repo, ".claude", "settings.local.json")
	old, _, _ := f.srv.windowByName("Chart Labs")
	oldDir := filepath.Join(f.srv.agents.SharedDir(old.ID, old.Name), "app")
	waitFor(t, "the first window's grant", func() bool { return fileHas(settings, oldDir) })
	waitFor(t, "the first restarts", func() bool { return sup.got() == "lead,thread" })

	f.put(t, "/v1/workspaces/"+wsID+"/group", `{"group":"Trading"}`, 204)

	win, _, _ := f.srv.windowByName("Trading")
	newDir := filepath.Join(f.srv.agents.SharedDir(win.ID, win.Name), "app")
	waitFor(t, "the new window's grant", func() bool { return fileHas(settings, newDir) && !fileHas(settings, oldDir) })
	waitFor(t, "the second restarts", func() bool { return sup.got() == "lead,thread,lead,thread" })

	f.put(t, "/v1/workspaces/"+wsID+"/group", `{"group":"trading"}`, 204)
	time.Sleep(100 * time.Millisecond)
	if got := sup.got(); got != "lead,thread,lead,thread" {
		t.Fatalf("a move to the window it is in restarted again: %q", got)
	}
}

// The old grant stays while another session of the same repo folder is
// still in the old window: the grant is the folder's, not the session's.
func TestPutGroup_MoveKeepsAGrantASiblingStillUses(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	f := newWindowAgentFixture(t, "sleep 1;:")
	repo, wsID := repoSessionInWindow(t, f)
	sibling := createWSAt(t, f, repo)
	f.put(t, "/v1/workspaces/"+sibling+"/group", `{"group":"Chart Labs"}`, 204)
	settings := filepath.Join(repo, ".claude", "settings.local.json")
	old, _, _ := f.srv.windowByName("Chart Labs")
	oldDir := filepath.Join(f.srv.agents.SharedDir(old.ID, old.Name), "app")
	waitFor(t, "the first window's grant", func() bool { return fileHas(settings, oldDir) })

	f.put(t, "/v1/workspaces/"+wsID+"/group", `{"group":"Trading"}`, 204)

	win, _, _ := f.srv.windowByName("Trading")
	newDir := filepath.Join(f.srv.agents.SharedDir(win.ID, win.Name), "app")
	waitFor(t, "the new window's grant", func() bool { return fileHas(settings, newDir) })
	if !fileHas(settings, oldDir) {
		t.Fatal("the old grant was taken from the sibling still in Chart Labs")
	}
}

// moveNote names both windows, and "no window" for a repo taken out of one.
func TestMoveNote_NamesBothWindows(t *testing.T) {
	mv := &movedRepo{to: "Trading"}
	mv.from.Name = "Chart Labs"
	if got := moveNote(mv); !strings.Contains(got, "from window Chart Labs to window Trading") || !strings.Contains(got, "list_peers") {
		t.Fatalf("note = %q", got)
	}
	if got := moveNote(&movedRepo{}); !strings.Contains(got, "from no window to no window") {
		t.Fatalf("note = %q", got)
	}
}

// gitRepoAt makes a git repo "app" whose local settings git ignores.
func gitRepoAt(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "app")
	os.MkdirAll(repo, 0o755)
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".claude/settings.local.json\n"), 0o644)
	return repo
}

func fileHas(path, s string) bool {
	raw, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(raw), s)
}

// An agent's home moved by the route is not settled: its session is not
// restarted (its sleeps and starts go through its own door) and no shared
// folder is granted or taken in its home.
func TestPutGroup_AnAgentsMoveIsNotSettled(t *testing.T) {
	f := newWindowAgentFixture(t, "sleep 1;:")
	if code, _ := f.start(t, "x-poster", ""); code != 201 {
		t.Fatalf("start = %d", code)
	}
	win, _, _ := f.srv.windowByName("Chart Labs")
	home := f.srv.agentWorkspace(win, "x-poster")
	if home == nil {
		t.Fatal("no agent home in the window")
	}
	sup, rows := &restartLog{}, &movedRows{}
	rows.set(home.ID)
	f.srv.SetRunners(sup, rows)

	f.put(t, "/v1/workspaces/"+home.ID+"/group", `{"group":"Trading"}`, 204)
	time.Sleep(300 * time.Millisecond)

	if got := sup.got(); got != "" {
		t.Fatalf("an agent's move restarted %q", got)
	}
	if _, err := os.Stat(filepath.Join(home.RepoPath, ".claude", "settings.local.json")); err == nil {
		t.Fatal("an agent's move wrote shared-folder grants into its home")
	}
	if f.srv.repoMove(home.ID) != nil {
		t.Fatal("an agent's home reads as a repo to settle")
	}
}
