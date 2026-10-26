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

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/harness"
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

// Walls are a claude-harness field, kept across a save that does not name
// them; on any other harness the save is refused.
func TestAgents_WallsOnlyOnClaude(t *testing.T) {
	s := agentsServer(t)
	if rec := do(t, s, "PUT", "/v1/agents/radar-seo", `{"description":"d","harness":"claude","walls":true}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"walls":true`) {
		t.Fatalf("walls on claude = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "PUT", "/v1/agents/radar-seo", `{"description":"a lens save that does not know walls"}`); !strings.Contains(rec.Body.String(), `"walls":true`) {
		t.Fatalf("a save without the field dropped walls: %s", rec.Body)
	}
	if rec := do(t, s, "PUT", "/v1/agents/x-poster", `{"description":"d","walls":true}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "claude harness") {
		t.Fatalf("walls on the default harness = %d %s, want 400", rec.Code, rec.Body)
	}
}

func TestWallRoots_HomeAndOwnSharedFolder(t *testing.T) {
	shared, config := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config) // never the real ~/.claude

	if roots, err := wallRoots(agent.Definition{Name: "radar-seo"}, "opencode", "/w/agents/radar-seo", shared); roots != nil || err != nil {
		t.Fatalf("no walls = %v %v", roots, err)
	}
	if _, err := wallRoots(agent.Definition{Name: "radar-seo", Walls: true}, "opencode", "/w/agents/radar-seo", shared); err == nil {
		t.Fatal("walls on opencode must refuse the start: nothing would hold them")
	}
	roots, err := wallRoots(agent.Definition{Name: "radar-seo", Walls: true}, "claude", "/w/agents/radar-seo", shared)
	own := filepath.Join(shared, "radar-seo")
	memory := filepath.Join(config, "projects", "-w-agents-radar-seo", "memory")
	if err != nil || len(roots) != 3 || roots[0] != "/w/agents/radar-seo" || roots[1] != memory || roots[2] != own {
		t.Fatalf("roots = %v %v, want home, its Claude memory store, its shared folder", roots, err)
	}
	for _, dir := range []string{own, memory} {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Fatalf("%s not made: %v", dir, err)
		}
	}
	// Only the memory folder: the rest of the Claude config stays outside.
	if entries, _ := os.ReadDir(config); len(entries) != 1 || entries[0].Name() != "projects" {
		t.Fatalf("config dir holds %v, want only projects/", entries)
	}
}

// The start's options carry a walled agent's roots onto the launch line,
// and an unwalled agent's line has none.
func TestLaunchOpts_WallsReachTheLaunchLine(t *testing.T) {
	shared, home, config := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	h := harness.Harness{Name: harness.Builtin, Command: harness.FallbackClaudeCommand}
	d := agent.Definition{Name: "radar-seo", Walls: true, Permissions: agent.Permissions{Read: "allow", Edit: "allow", Bash: "ask", Webfetch: "ask"}}
	opts, err := launchOpts(d, h.Name, home, []string{"/repo"}, shared, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	opts.Serve = "/s.mjs"
	line := agent.LaunchCommand(d, h, "/b", opts).Persist
	memory, _ := agent.ClaudeMemoryDir(home)
	want := "--write-root " + home + " --write-root " + memory + " --write-root " + filepath.Join(shared, "radar-seo") + " --write-inside allow"
	if !strings.Contains(line, want) || strings.Contains(line, "Edit") {
		t.Fatalf("walled line = %s\nwant %q and no pre-allowed edit tools", line, want)
	}
	d.Walls = false
	plain, _ := launchOpts(d, h.Name, home, nil, shared, "", 0)
	plain.Serve = "/s.mjs"
	if l := agent.LaunchCommand(d, h, "/b", plain).Persist; strings.Contains(l, "--write-root") {
		t.Fatalf("unwalled line has roots: %s", l)
	}
	d.Walls = true
	if _, err := launchOpts(d, "opencode", home, nil, shared, "", 0); err == nil || !strings.Contains(err.Error(), "walls:") {
		t.Fatalf("walls on opencode = %v, want the start refused", err)
	}
}
