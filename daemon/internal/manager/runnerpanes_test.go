package manager

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/shellint"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// runnerManager is a cold manager over a real registry: the repo /r/app with
// one tmux Claude pane, and a session row for a runner in the same folder.
func runnerManager(t *testing.T) (*Manager, *store.SQLite, store.RunnerSession) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ws := &model.Workspace{ID: "ws", Name: "app", RepoPath: "/r/app", TmuxSession: "ccmux-app"}
	tmuxPane := &model.Pane{ID: "tmux1", WorkspaceID: "ws", CWD: "/r/app", Harness: "claude", Attention: model.AttentionIdle}
	if err := st.SaveWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePane(tmuxPane); err != nil {
		t.Fatal(err)
	}
	sess := store.RunnerSession{ID: "0a1b2c3d-aaaa-bbbb-cccc-000000000001", Cwd: "/r/app", Wanted: store.SessionRunning, PaneID: "run1", WorkspaceID: "ws", CreatedAt: 5}
	if err := st.AddRunnerSession(sess); err != nil {
		t.Fatal(err)
	}
	m := New(context.Background(), &tmux.Server{Socket: "unused"}, st)
	ws.Panes = []*model.Pane{tmuxPane}
	m.byID["ws"] = &entry{ws: ws}
	return m, st, sess
}

// storedPaneIDs is every pane the panes table holds: what the next daemon
// start would hand tmux bookkeeping.
func storedPaneIDs(t *testing.T, st *store.SQLite) []string {
	t.Helper()
	saved, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, ws := range saved {
		for _, p := range ws.Panes {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

func TestARunnerPaneIsHeldBesideTheTmuxPanes(t *testing.T) {
	m, _, sess := runnerManager(t)
	m.SyncRunnerPane(sess)
	m.SyncRunnerPane(sess) // the same row again changes nothing
	e := m.byID["ws"]
	if len(e.ws.Panes) != 1 || e.ws.Panes[0].ID != "tmux1" || len(e.runners) != 1 {
		t.Fatalf("tmux's list is untouched and the runner pane is beside it: panes=%d runners=%d", len(e.ws.Panes), len(e.runners))
	}
	if lastClaudePane(e.ws.Panes) != "tmux1" {
		t.Fatal("a bus spawn still picks the tmux Claude pane")
	}
	if ws := m.WorkspaceForPane("run1"); ws != "ws" {
		t.Fatalf("workspace of the runner pane = %q", ws)
	}
	if _, ok := m.GroupForPane("run1"); !ok {
		t.Fatal("the bus resolves the runner pane's group like any pane's")
	}
	if h := m.HarnessForPane("run1"); h != "claude" {
		t.Fatalf("the proxy routes it on the claude harness tier, got %q", h)
	}
	info, ok := m.RunnerPane("run1")
	if !ok || info.Name != "app-s-0a1b2c3d" || info.SessionID != sess.ID || info.Repo != "/r/app" {
		t.Fatalf("RunnerPane = %+v %v", info, ok)
	}
	if _, ok := m.RunnerPane("tmux1"); ok {
		t.Fatal("a tmux pane is not a runner pane")
	}
}

func TestARunnersBusNameIsNeverARepoFoldersName(t *testing.T) {
	m, _, sess := runnerManager(t)
	m.byID["other"] = &entry{ws: &model.Workspace{ID: "other", RepoPath: "/r/backend"}}
	for rowName, want := range map[string]string{
		"":          "app-s-0a1b2c3d",
		"reviewer":  "reviewer",
		"app":       "app-s-0a1b2c3d", // its own repo's session owns that name
		"backend":   "app-s-0a1b2c3d", // and another repo's owns this one
		"app-notes": "app-notes",
	} {
		sess.Name = rowName
		m.SyncRunnerPane(sess)
		if got, _ := m.RunnerPane("run1"); got.Name != want {
			t.Errorf("row name %q: bus name %q, want %q", rowName, got.Name, want)
		}
	}
}

func TestARunnerPaneNeverReachesThePanesTable(t *testing.T) {
	m, st, sess := runnerManager(t)
	m.SyncRunnerPane(sess)
	if !m.ApplyAgentSignal("run1", model.AttentionNeedsInput, model.ReasonPermission) {
		t.Fatal("the runner's signal finds its pane")
	}
	m.ApplySession("run1", sess.ID, model.SessionStarted) // a hook
	m.ApplyAttention("run1", model.AttentionDone, model.ReasonFinished)
	m.MarkActed("ws", "run1")
	m.ApplyAgentSignal("run1", model.AttentionDone, model.ReasonFinished)
	if ids := storedPaneIDs(t, st); len(ids) != 1 || ids[0] != "tmux1" {
		t.Fatalf("the panes table holds only tmux panes, or a revive would give the runner a window: %v", ids)
	}
	row, _ := st.RunnerSession(sess.ID)
	if row.Attention != "done" || row.AttentionReason != "finished" || row.AttentionSince == 0 {
		t.Fatalf("its attention is kept on its session row: %+v", row)
	}
}

func TestARunnersAttentionIsItsRunnersToSet(t *testing.T) {
	m, _, sess := runnerManager(t)
	m.SyncRunnerPane(sess)
	_, ch := m.events.subscribe()
	watched := true
	m.Watched = func(string) bool { return watched }

	m.ApplyAgentSignal("run1", model.AttentionNeedsInput, model.ReasonQuestion)
	evs := drain(ch)
	if len(evs) != 1 || evs[0].Kind != "attention" || evs[0].WorkspaceID != "ws" || evs[0].PaneID != "run1" ||
		evs[0].Attention != model.AttentionNeedsInput || evs[0].Reason != model.ReasonQuestion || evs[0].Since == 0 {
		t.Fatalf("one firehose event naming its workspace, and no retire though the workspace's terminal is watched: %+v", evs)
	}
	// The same session's hooks do not set it a second time.
	m.ApplyAttention("run1", model.AttentionIdle, model.ReasonNone)
	m.MarkSeen("ws") // a look at the terminal has not seen the runner
	if p := m.PaneByID("run1"); p.Attention != model.AttentionNeedsInput {
		t.Fatalf("still asking: %q", p.Attention)
	}
	if !m.Remind("run1") {
		t.Fatal("the idle reminder alerts for the claim it still makes")
	}
	if evs := drain(ch); len(evs) != 1 || !evs[0].Remind {
		t.Fatalf("one reminder event: %+v", evs)
	}
	m.MarkActed("ws", "run1")
	if p := m.PaneByID("run1"); p.Attention != model.AttentionIdle {
		t.Fatalf("dealt with: %q", p.Attention)
	}
	// And a hook still sets a tmux pane's.
	m.ApplyAttention("tmux1", model.AttentionDone, model.ReasonFinished)
	watched = false
	m.ApplyAttention("tmux1", model.AttentionDone, model.ReasonFinished)
	if p := m.PaneByID("tmux1"); p.Attention != model.AttentionDone {
		t.Fatalf("tmux pane: %q", p.Attention)
	}
}

func TestAHookNamingARunnerNeverTakesTheFolderFallback(t *testing.T) {
	m, st, sess := runnerManager(t)
	st.AddRunnerSession(store.RunnerSession{ID: "ended", Cwd: "/r/app", Wanted: store.SessionDone, PaneID: "gone1", WorkspaceID: "ws"})
	if err := m.LoadRunnerPanes(); err != nil {
		t.Fatal(err)
	}
	if got := m.ResolvePane("run1", "/r/app"); got != "run1" {
		t.Fatalf("a live runner's hook lands on its own pane: %q", got)
	}
	if got := m.ResolvePane("gone1", "/r/app"); got != "" {
		t.Fatalf("an ended runner's hook lands nowhere, not on the tmux pane in its folder: %q", got)
	}
	if got := m.ResolvePane("", "/r/app/sub"); got != "tmux1" {
		t.Fatalf("a hook with no pane id still falls back to the tmux pane, never the runner: %q", got)
	}
	if got := m.ResolvePane("stranger", "/r/app"); got != "tmux1" {
		t.Fatalf("an unknown pane id that was never a runner keeps the old fallback: %q", got)
	}
	sess.Wanted = store.SessionDone
	m.SyncRunnerPane(sess)
	if got := m.ResolvePane("run1", "/r/app"); got != "" {
		t.Fatalf("once it ends, its hooks land nowhere: %q", got)
	}
	if _, ok := m.GroupForPane("run1"); ok {
		t.Fatal("and its pane is gone, which is what lets the bus reap its peer")
	}
}

func TestRunnerPanesComeBackFromTheirRowsAtStart(t *testing.T) {
	m, st, sess := runnerManager(t)
	st.SaveRunnerAttention("run1", "needs_input", "permission", 42)
	st.SetRunnerSessionWanted(sess.ID, store.SessionAsleep, 6)
	if err := m.LoadRunnerPanes(); err != nil {
		t.Fatal(err)
	}
	p := m.PaneByID("run1")
	if p == nil || p.Runtime != model.RuntimeRunner || p.SessionID != sess.ID || p.CWD != "/r/app" {
		t.Fatalf("rebuilt: %+v", p)
	}
	if p.Attention != model.AttentionNeedsInput || p.AttentionReason != model.ReasonPermission || p.AttentionSince != 42 {
		t.Fatalf("with the attention the last daemon recorded: %+v", p)
	}
	if got := m.PaneLiveness("run1"); got != LiveAsleep {
		t.Fatalf("asleep, as its row says: %s", got)
	}
	if got := m.RunnerPanes(); len(got) != 1 || got[0].ID != "run1" {
		t.Fatalf("RunnerPanes: %+v", got)
	}
	if info, _ := m.RunnerPane("run1"); !info.Asleep {
		t.Fatalf("the bus is told it sleeps: %+v", info)
	}
	sess.Wanted = store.SessionRunning
	m.SyncRunnerPane(sess)
	if info, _ := m.RunnerPane("run1"); info.Asleep {
		t.Fatalf("and that it woke: %+v", info)
	}
}

func TestPaneLivenessIsOneAnswerForBothRuntimes(t *testing.T) {
	m, _, sess := runnerManager(t)
	m.SyncRunnerPane(sess)
	if got := m.PaneLiveness("run1"); got != LiveRunning {
		t.Fatalf("with no supervisor wired, the row's word: %s", got)
	}
	var asked string
	var fails error
	m.RunnerLiveness = func(sessionID string) (string, error) { asked = sessionID; return "asleep", fails }
	if got := m.PaneLiveness("run1"); got != LiveAsleep || asked != sess.ID {
		t.Fatalf("a runner pane asks the supervisor about its session: %s (asked %q)", got, asked)
	}
	fails = errors.New("systemd did not answer")
	if got := m.PaneLiveness("run1"); got != LiveRunning {
		t.Fatalf("a supervisor that cannot say leaves the row's word, never a guess from the failed call: %s", got)
	}
	fails, asked = nil, ""
	if got := m.PaneLiveness("tmux1"); got != LiveAsleep || asked != "" {
		t.Fatalf("a tmux pane in a closed workspace is asleep, and no supervisor is asked: %s", got)
	}
	if got := m.PaneLiveness("nope"); got != LiveDone {
		t.Fatalf("an unknown pane is done: %s", got)
	}
}

func TestDeletingAWorkspaceEndsItsRunnerSessions(t *testing.T) {
	m, _, sess := runnerManager(t)
	m.SyncRunnerPane(sess)
	ended := make(chan string, 1)
	m.EndRunnerSession = func(id string) error { ended <- id; return nil }
	if err := m.KillWorkspace("ws"); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-ended:
		if id != sess.ID {
			t.Fatalf("ended %q", id)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the session was left running with no workspace")
	}
	if got := m.ResolvePane("run1", "/r/app"); got != "" {
		t.Fatalf("its pane is gone and stays a runner's: %q", got)
	}
}

func TestASessionWhoseWorkspaceIsGoneHasNoPane(t *testing.T) {
	m, _, sess := runnerManager(t)
	sess.WorkspaceID = "deleted"
	m.SyncRunnerPane(sess)
	if _, ok := m.RunnerPane("run1"); ok {
		t.Fatal("no workspace, no pane")
	}
	if got := m.ResolvePane("run1", "/r/app"); got != "" {
		t.Fatalf("and its hooks still land nowhere: %q", got)
	}
	sess.PaneID = ""
	m.SyncRunnerPane(sess) // a session from before panes: nothing to do
}

func TestRunnerWorkspaceForFindsTheRepoHoldingAFolder(t *testing.T) {
	m, _, _ := runnerManager(t)
	m.byID["inner"] = &entry{ws: &model.Workspace{ID: "inner", RepoPath: "/r/app/packages/web", CreatedAt: 2}}
	m.byID["twin"] = &entry{ws: &model.Workspace{ID: "twin", RepoPath: "/r/app/packages/web", CreatedAt: 1}}
	m.byID["bot"] = &entry{ws: &model.Workspace{ID: "bot", RepoPath: "/r/app/agents/bot", Agent: "bot"}}
	for cwd, want := range map[string]string{
		"/r/app":                  "ws",
		"/r/app/daemon":           "ws",
		"/r/app/packages/web/src": "twin", // the deeper repo, the older of two alike
		"/r/application":          "",     // a longer name is not inside it
		"/r/app/agents/bot":       "ws",   // an agent's folder is not a repo session
		"/elsewhere":              "",
	} {
		if got := m.RunnerWorkspaceFor(cwd); got != want {
			t.Errorf("%s: workspace %q, want %q", cwd, got, want)
		}
	}
}

func TestRunnerEnvIsThePaneEnvThatAppliesToClaude(t *testing.T) {
	m := &Manager{LocalURL: "http://127.0.0.1:7911", HooksSocket: "/run/hooks.sock",
		ExtraPaneEnv: func(paneID string) map[string]string { return map[string]string{"CCMUX_PANE_TOKEN": "tok-" + paneID} }}
	const paneID = "8f5e1f6e-3d0f-4d1e-9d3b-0a6a3f6f0160"
	t.Cleanup(func() { shellint.Cleanup(paneID) })
	env, pane := m.RunnerEnv(paneID), m.paneEnv(nil, paneID)
	for _, name := range []string{"CCMUX_PANE_ID", "CCMUX_PANE_TOKEN", "CCMUX_DAEMON_URL", "CCMUX_HOOKS_SOCK", "ANTHROPIC_BASE_URL",
		"_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL", "CLAUDE_CODE_MAX_MCP_DESCRIPTION_LENGTH"} {
		if env[name] == "" || env[name] != pane[name] {
			t.Errorf("%s: runner %q, tmux pane %q: a runner gets what a pane's Claude gets", name, env[name], pane[name])
		}
	}
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:7911/llm/pane/"+paneID {
		t.Errorf("its own proxy route: %q", env["ANTHROPIC_BASE_URL"])
	}
	if env["CCMUX_PEERS_CHANNEL"] != "0" {
		t.Error("its shim polls: a channel push never reaches an SDK session's model")
	}
	for _, name := range []string{"ZDOTDIR", "CCMUX_CMD_FILE", "OPENCODE_CONFIG_CONTENT", "COLORTERM"} {
		if _, has := env[name]; has {
			t.Errorf("%s is a shell's, not a runner's", name)
		}
	}
}

// Claude Code sends no idle reminder in a runner session, so the daemon
// keeps that clock: a finished turn left alone for the wait is reminded,
// which is what its alert waits for.
func TestAFinishedRunnerTurnIsRemindedLikeAPanes(t *testing.T) {
	m, _, sess := runnerManager(t)
	m.runnerReminders.after = 30 * time.Millisecond
	m.SyncRunnerPane(sess)
	_, ch := m.events.subscribe()
	reminded := func() int {
		n := 0
		for _, ev := range drain(ch) {
			if ev.Remind && ev.PaneID == "run1" && ev.Attention == model.AttentionDone {
				n++
			}
		}
		return n
	}

	m.ApplyAgentSignal("run1", model.AttentionDone, model.ReasonFinished)
	n := 0
	for end := time.Now().Add(10 * time.Second); n == 0 && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		n += reminded()
	}
	time.Sleep(150 * time.Millisecond)
	if n += reminded(); n != 1 {
		t.Fatalf("a finished turn nobody looked at is reminded once: %d", n)
	}
	// The turn carried on inside the wait: no reminder for the first end.
	m.ApplyAgentSignal("run1", model.AttentionDone, model.ReasonFinished)
	m.ApplyAgentSignal("run1", model.AttentionRunning, model.ReasonNone)
	time.Sleep(150 * time.Millisecond)
	if n := reminded(); n != 0 {
		t.Fatalf("a new turn calls the reminder off: %d", n)
	}
	// Dealt with inside the wait: it claims nothing by then.
	m.ApplyAgentSignal("run1", model.AttentionDone, model.ReasonFinished)
	m.MarkActed("ws", "run1")
	time.Sleep(150 * time.Millisecond)
	if n := reminded(); n != 0 {
		t.Fatalf("a pane somebody dealt with gets none: %d", n)
	}
	// A card alerts at once and waits for no reminder; an ended session has none pending.
	m.ApplyAgentSignal("run1", model.AttentionNeedsInput, model.ReasonPermission)
	m.ApplyAgentSignal("run1", model.AttentionDone, model.ReasonFinished)
	sess.Wanted = store.SessionDone
	m.SyncRunnerPane(sess)
	time.Sleep(150 * time.Millisecond)
	if n := reminded(); n != 0 {
		t.Fatalf("an ended session is not reminded: %d", n)
	}
}

func TestARunnerPaneFollowsItsSessionIntoAnotherFolder(t *testing.T) {
	m, _, sess := runnerManager(t)
	m.SyncRunnerPane(sess)
	// A thread member moved into its worktree.
	sess.Cwd = "/wt/app/thread-3"
	m.SyncRunnerPane(sess)
	panes := m.RunnerPanes()
	if len(panes) != 1 || panes[0].CWD != "/wt/app/thread-3" {
		t.Fatalf("the pane is where its session works now, and still one pane: %+v", panes)
	}
	if info, ok := m.RunnerPane("run1"); !ok || info.Repo != "/r/app" {
		t.Fatalf("its repo is still its workspace's: %+v %v", info, ok)
	}
}
