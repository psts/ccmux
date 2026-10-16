package manager

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// fakeClaudeScript draws Claude's input box (a rule, an empty ❯ line, a
// rule) and quits on /exit, which is all the restart reads and types.
const fakeClaudeScript = `rule='────────────────────────────────────────'
printf '%s\n❯ \n%s\n' "$rule" "$rule"
while IFS= read -r line; do
  [ "$line" = /exit ] && exit 0
done
`

// draftClaudeScript is the same Claude with a half-typed prompt in its box.
var draftClaudeScript = strings.Replace(fakeClaudeScript, "❯ \\n", "❯ draft\\n", 1)

// installFakeClaude lays out what the native installer leaves on disk: two
// versions (copies of bash, so /proc/<pid>/exe names them) and a launcher
// link, which it returns.
func installFakeClaude(t *testing.T, root string) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	raw, err := os.ReadFile(bash)
	if err != nil {
		t.Fatal(err)
	}
	versions := filepath.Join(root, "versions")
	bin := filepath.Join(root, "bin")
	for _, d := range []string{versions, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []string{"1.0.0", "2.0.0"} {
		if err := os.WriteFile(filepath.Join(versions, v), raw, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(bin, "claude")
	pointLink(t, link, filepath.Join(versions, "1.0.0"))
	return link
}

// pointLink moves the launcher link the way an update does.
func pointLink(t *testing.T, link, target string) {
	t.Helper()
	tmp := link + ".new"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, link); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func paneMark(m *Manager, paneID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, p := m.findPaneLocked(paneID); p != nil {
		return p.ClaudeUpdate
	}
	return "?"
}

// restartRig is a manager on a private tmux server with one pane running
// the fake Claude on 1.0.0, a session with a transcript, and a launcher link
// to move.
type restartRig struct {
	mgr                     *Manager
	paneID, sessionID, link string
	root, wsID              string
}

func newRestartRig(t *testing.T, scriptBody string) restartRig {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	r := restartRig{root: t.TempDir(), sessionID: "3e067cdf-2528-4c9d-8832-e099badf7b02"}
	link := installFakeClaude(t, r.root)
	r.link = link
	script := filepath.Join(r.root, "fake.sh")
	writeFile(t, script, scriptBody)
	cfg := filepath.Join(r.root, "claude-config")
	transcript := filepath.Join(cfg, "projects", "-repo", r.sessionID+".jsonl")
	writeFile(t, transcript, "{}\n")
	quiet := time.Now().Add(-2 * time.Hour) // nothing written for two hours
	if err := os.Chtimes(transcript, quiet, quiet); err != nil {
		t.Fatal(err)
	}

	r.mgr = startItestManager(t, "ccmux-autorestart-itest")
	r.mgr.restart.link, r.mgr.restart.procRoot, r.mgr.restart.claudeDir = link, "/proc", cfg
	// By full path: through PATH, a shell that cannot run the link falls
	// through to the real claude further down.
	ws, err := r.mgr.CreateWorkspace("restart", r.root, r.root, link+" "+script, "tester", "")
	if err != nil {
		t.Fatal(err)
	}
	r.paneID, r.wsID = ws.Panes[0].ID, ws.ID
	waitFor(t, "the fake Claude on 1.0.0, in the pane's foreground", 15*time.Second, func() bool {
		r.mgr.mu.RLock()
		_, p := r.mgr.findPaneLocked(r.paneID)
		fg := p != nil && runsClaude(p)
		r.mgr.mu.RUnlock()
		return fg && r.mgr.runsInstalled(r.paneID, "1.0.0")
	})
	return r
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// startItestManager starts a manager on a fresh tmux server and store, torn
// down with the test.
func startItestManager(t *testing.T, socket string) *Manager {
	t.Helper()
	tsrv := &tmux.Server{Socket: socket, ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })
	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = st.Close() })
	mgr := New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	return mgr
}

// idleAnHour is the hooks of a session whose last turn ended 61 minutes ago.
func (r restartRig) idleAnHour() {
	r.mgr.ApplySession(r.paneID, r.sessionID, model.SessionStarted)
	r.mgr.restart.clocks.attention(r.paneID, model.AttentionDone, model.ReasonFinished, time.Now().Add(-61*time.Minute))
}

// settle waits for a restart the last tick started to finish, either way.
func (r restartRig) settle(t *testing.T, version string) {
	t.Helper()
	waitFor(t, "the restart to finish", 20*time.Second, func() bool {
		_, inflight := r.mgr.restart.state(r.paneID, version)
		return !inflight
	})
}

func (r restartRig) wantMark(t *testing.T, when, want string) {
	t.Helper()
	r.mgr.restartTick(time.Now())
	if got := paneMark(r.mgr, r.paneID); got != want {
		t.Fatalf("mark %s = %q, want %q", when, got, want)
	}
}

// A pane behind the installed version and idle past the wait is exited and
// started again with its own startup command plus --resume, comes back on
// the new version, and loses its mark.
func TestAutoRestartRelaunchesAnIdlePaneOnTheNewVersion(t *testing.T) {
	r := newRestartRig(t, fakeClaudeScript)
	r.idleAnHour()
	r.wantMark(t, "before any update", "")

	pointLink(t, r.link, filepath.Join(r.root, "versions", "2.0.0"))
	r.wantMark(t, "once behind", "pending")
	waitFor(t, "the pane back on 2.0.0", 30*time.Second, func() bool {
		return r.mgr.runsInstalled(r.paneID, "2.0.0")
	})
	if cmd := paneCmdline(t, r.paneID); !strings.Contains(cmd, "--resume "+r.sessionID) {
		t.Fatalf("relaunch command line %q lacks --resume %s", cmd, r.sessionID)
	}
	waitFor(t, "the restart to finish", 10*time.Second, func() bool {
		_, inflight := r.mgr.restart.state(r.paneID, "2.0.0")
		return !inflight
	})
	r.wantMark(t, "after the restart", "")
}

// paneCmdline is the command line of the pane's top-level fake Claude.
func paneCmdline(t *testing.T, paneID string) string {
	t.Helper()
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		env, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ"))
		if err != nil || !strings.Contains(string(env), "CCMUX_PANE_ID="+paneID+"\x00") {
			continue
		}
		exe, _ := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		if !strings.Contains(exe, "/versions/") {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		return strings.ReplaceAll(string(raw), "\x00", " ")
	}
	return ""
}

// A draft in the input box keeps the pane as it is: nothing is typed, the
// old Claude keeps running, and the pane stays pending for a later tick.
//
// The mark's change reaches the lenses as one workspace-status event, and a
// tick that changes nothing sends none.
func TestAutoRestartLeavesADraftAlone(t *testing.T) {
	r := newRestartRig(t, draftClaudeScript)
	r.idleAnHour()
	id, events := r.mgr.SubscribeEvents()
	t.Cleanup(func() { r.mgr.UnsubscribeEvents(id) })
	pointLink(t, r.link, filepath.Join(r.root, "versions", "2.0.0"))
	r.mgr.restartTick(time.Now())
	r.settle(t, "2.0.0")
	if r.mgr.PaneAtShell(r.paneID) || !r.mgr.runsInstalled(r.paneID, "1.0.0") {
		t.Fatal("a pane with a draft was exited")
	}
	if n := r.statusEvents(events); n != 1 {
		t.Fatalf("workspace-status events for the new mark = %d, want 1", n)
	}
	r.wantMark(t, "with a draft", "pending")
	if n := r.statusEvents(events); n != 0 {
		t.Fatalf("workspace-status events for an unchanged mark = %d, want 0", n)
	}
}

// statusEvents drains the firehose and counts the rig workspace's
// workspace-status events.
func (r restartRig) statusEvents(ch <-chan Event) int {
	n := 0
	for _, ev := range drain(ch) {
		if ev.Kind == "workspace-status" && ev.WorkspaceID == r.wsID {
			n++
		}
	}
	return n
}

// A relaunch that never comes up marks the pane failed and the mark stays,
// though the pane no longer runs any Claude to be behind; the next tick does
// not try again.
func TestAutoRestartMarksAFailedRelaunch(t *testing.T) {
	saved := restartBackWait
	restartBackWait = 3 * time.Second
	t.Cleanup(func() { restartBackWait = saved })
	r := newRestartRig(t, fakeClaudeScript)
	r.idleAnHour()
	newer := filepath.Join(r.root, "versions", "2.0.0")
	pointLink(t, r.link, newer)
	if err := os.Remove(newer); err != nil { // the relaunch's exec fails
		t.Fatal(err)
	}
	r.mgr.restartTick(time.Now())
	r.settle(t, "2.0.0")
	if !r.mgr.PaneAtShell(r.paneID) {
		t.Fatal("the pane should be at its shell after the failed relaunch")
	}
	r.wantMark(t, "after the failed relaunch", "manual")
	if _, inflight := r.mgr.restart.state(r.paneID, "2.0.0"); inflight {
		t.Fatal("a failed version was tried again")
	}
}
