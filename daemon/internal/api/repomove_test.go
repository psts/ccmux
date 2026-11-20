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

	"ccmux.dev/ccmuxd/internal/peers"
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

// restartLog is a supervisor that notes the restarts a move asks for
// (between turns), safe to read while the settle's goroutine writes.
type restartLog struct {
	fakeRunners
	mu  sync.Mutex
	ids []string
}

func (r *restartLog) RestartBetweenTurns(id string) error {
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

// createWSAt makes another plain session of the repo folder repo, named
// after it unless a name is given.
func createWSAt(t *testing.T, f *windowAgentFixture, repo string, name ...string) string {
	t.Helper()
	n := filepath.Base(repo)
	if len(name) > 0 {
		n = name[0]
	}
	resp, err := http.Post(f.base+"/v1/workspaces", "application/json",
		strings.NewReader(`{"name":`+strconv.Quote(n)+`,"repoPath":`+strconv.Quote(repo)+`,"createdBy":"tester","startupCommand":""}`))
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

// The grant is named after the repo's folder, which is the bus name, not
// the session's own name: a session named otherwise has the folder it was
// granted taken back, and the new one granted under the same name.
func TestPutGroup_MoveSettlesASessionNamedOtherThanItsFolder(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	f := newWindowAgentFixture(t, "sleep 1;:")
	repo := gitRepoAt(t)
	wsID := createWSAt(t, f, repo, "frontend")
	f.put(t, "/v1/workspaces/"+wsID+"/group", `{"group":"Chart Labs"}`, 204)
	settings := filepath.Join(repo, ".claude", "settings.local.json")
	old, _, _ := f.srv.windowByName("Chart Labs")
	oldDir := filepath.Join(f.srv.agents.SharedDir(old.ID, old.Name), "app")
	if !fileHas(settings, oldDir) {
		t.Fatalf("the join granted something else than %s", oldDir)
	}

	f.put(t, "/v1/workspaces/"+wsID+"/group", `{"group":"Trading"}`, 204)

	win, _, _ := f.srv.windowByName("Trading")
	if newDir := filepath.Join(f.srv.agents.SharedDir(win.ID, win.Name), "app"); !fileHas(settings, newDir) {
		t.Fatalf("the new window's folder %s was not granted", newDir)
	}
	if fileHas(settings, oldDir) || fileHas(settings, "frontend") {
		raw, _ := os.ReadFile(settings)
		t.Fatalf("the old grant stayed, or one was made under the session's own name: %s", raw)
	}
}

// A tmux Claude the move could not restart is told on the bus that it
// moved, naming both windows; one in another pane hears nothing.
func TestRestartMovedPane_TellsAPaneItCouldNotRestart(t *testing.T) {
	f := newWindowAgentFixture(t, "sleep 1;:")
	svc := peers.NewService(f.st, f.srv.mgr, testSecret)
	f.srv.EnablePeers(svc)
	register := func(pane string) string {
		nextFakePID++
		resp := postJSON(t, f.base+"/v1/peers/register", peers.TokenForPane(testSecret, pane), map[string]any{"pane_id": pane, "pid": nextFakePID, "cwd": "/tmp", "git_root": "/tmp"})
		defer resp.Body.Close()
		var reg struct {
			PeerID string `json:"peer_id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil || reg.PeerID == "" {
			t.Fatalf("register %s: %d %v", pane, resp.StatusCode, err)
		}
		return reg.PeerID
	}
	moved := register(f.ws.Panes[0].ID)
	other := createWS(t, f.base)
	bystander := register(other.Panes[0].ID)
	mv := &movedRepo{id: f.ws.ID, name: "tmp", to: "Trading"}
	mv.from.Name = "Chart Labs"

	f.srv.restartMovedPane(mv, f.ws.Panes[0].ID) // this host's test manager reads no process table: never restartable

	evs, err := svc.Poll(moved)
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Text, "from window Chart Labs to window Trading") {
		t.Fatalf("the moved pane got %v %v", evs, err)
	}
	if evs, _ := svc.Poll(bystander); len(evs) != 0 {
		t.Fatalf("another pane heard it: %v", evs)
	}
}

// A move whose window cannot be read back is not settled: not taken for a
// move into no window (its old grant taken, its sessions restarted).
func TestSettleMove_AnUnreadableWindowSettlesNothing(t *testing.T) {
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
	waitFor(t, "the first restarts", func() bool { return sup.got() == "lead,thread" })
	settings := filepath.Join(repo, ".claude", "settings.local.json")
	before, _ := os.ReadFile(settings)

	// It came from another window, as far as the settle knows; the cached
	// lists still say Chart Labs, so a settle reading them would restart.
	mv := f.srv.repoMove(wsID)
	mv.from.ID, mv.from.Name = "0ld-window", "Old"
	f.st.Close()
	f.srv.settleMove(mv)
	time.Sleep(200 * time.Millisecond)

	if got := sup.got(); got != "lead,thread" {
		t.Fatalf("an unreadable window state restarted the repo: %q", got)
	}
	if after, _ := os.ReadFile(settings); string(after) != string(before) {
		t.Fatalf("the grants changed: %s -> %s", before, after)
	}
}
