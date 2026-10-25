package api

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
)

// A repo session in a window gets shared/<its name>/ and its repo's local
// Claude settings list that folder; the agent session in the same window
// is not a repo session and gets no folder of this kind.
func TestGrantSharedFolders_RepoSessionGetsItsOwnFolder(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	f := newWindowAgentFixture(t, "sleep 1;:")
	if code, _ := f.start(t, "x-poster", ""); code != 201 {
		t.Fatalf("start = %d", code)
	}
	repo, _ := repoSessionInWindow(t, f)

	f.srv.GrantSharedFolders()

	win, _, _ := f.srv.windowByName("Chart Labs")
	shared := f.srv.agents.SharedDir(win.ID, win.Name)
	own := filepath.Join(shared, filepath.Base(repo))
	if fi, err := os.Stat(own); err != nil || !fi.IsDir() {
		t.Fatalf("no shared folder for the repo session: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(repo, ".claude", "settings.local.json"))
	if err != nil || !strings.Contains(string(raw), own) {
		t.Fatalf("repo settings = %s %v, want %s listed", raw, err, own)
	}
	if _, err := os.Stat(filepath.Join(shared, "x-poster")); err == nil {
		t.Error("the agent got a repo-session folder")
	}
}

// repoSessionInWindow makes a git repo session "app" in the fixture's window.
func repoSessionInWindow(t *testing.T, f *windowAgentFixture) (string, string) {
	t.Helper()
	// Its own repo, never the fixture's /tmp: the grant writes into the repo.
	repo := filepath.Join(t.TempDir(), "app")
	os.MkdirAll(repo, 0o755)
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".claude/settings.local.json\n"), 0o644)
	resp, err := http.Post(f.base+"/v1/workspaces", "application/json",
		strings.NewReader(`{"name":"app","repoPath":`+strconv.Quote(repo)+`,"createdBy":"tester","startupCommand":""}`))
	if err != nil {
		t.Fatal(err)
	}
	var ws wsResult
	json.NewDecoder(resp.Body).Decode(&ws)
	resp.Body.Close()
	f.put(t, "/v1/workspaces/"+ws.ID+"/group", `{"group":"Chart Labs"}`, 204)
	return repo, ws.ID

}

// The join side: a repo session joining later is granted as its notice is
// sent; an agent's join, or a leave, writes nothing.
func TestSendNotice_GrantsARepoJoinOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	f := newWindowAgentFixture(t, "sleep 1;:")
	repo, wsID := repoSessionInWindow(t, f)
	settings := filepath.Join(repo, ".claude", "settings.local.json")
	win, _, _ := f.srv.windowByName("Chart Labs")
	shared := f.srv.agents.SharedDir(win.ID, win.Name)

	f.srv.sendNotice(manager.MembershipChange{WorkspaceID: wsID, Name: "app", RepoPath: repo, Agent: "x-poster", To: "Chart Labs"})
	f.srv.sendNotice(manager.MembershipChange{WorkspaceID: wsID, Name: "app", RepoPath: repo, From: "Chart Labs"})
	if _, err := os.Stat(settings); err == nil {
		t.Fatal("an agent join or a leave wrote the repo's settings")
	}
	f.srv.sendNotice(manager.MembershipChange{WorkspaceID: wsID, Name: "app workspace", RepoPath: repo, To: "Chart Labs"})
	raw, err := os.ReadFile(settings)
	if own := filepath.Join(shared, "app"); err != nil || !strings.Contains(string(raw), own) {
		t.Fatalf("join grant = %s %v, want %s (the bus name, not the workspace name)", raw, err, own)
	}
}
