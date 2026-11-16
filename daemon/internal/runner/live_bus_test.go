package runner

// The bus row of the live table: a scratch daemon built from this tree (the
// real wiring in cmd/ccmuxd), a real runner session under systemd, and a test
// peer that talks to it over the bus. Same switch and needs as live_test.go;
// the shim is built here from the tree, so the session never runs the one
// installed on this host.
//
// The scratch daemon is fenced off from the live one: its own CCMUXD_ROOT
// (config, runtime and session state), tmux socket, port (a free one, never
// the daemon's own) and unit template (ccmux-session-livebus@).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const busPrefix = "ccmux-session-livebus"

type busLive struct {
	t     *testing.T
	root  string
	base  string // the scratch daemon's URL
	group string // the window the repo session is in, and the tester's fallback group
	repo  string
	token string // the scratch bus's pane-less token
	me    string // the tester's peer id
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// call is one JSON request to the scratch daemon; out may be nil.
func (b *busLive) call(method, path, token string, in, out any) int {
	b.t.Helper()
	var body bytes.Buffer
	if in != nil {
		json.NewEncoder(&body).Encode(in)
	}
	req, _ := http.NewRequest(method, b.base+path, &body)
	req.Header.Set("content-type", "application/json")
	if token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer res.Body.Close()
	if out != nil {
		json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

func (b *busLive) wait(what string, limit time.Duration, cond func() bool) time.Duration {
	b.t.Helper()
	start := time.Now()
	for time.Since(start) < limit {
		if cond() {
			return time.Since(start)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if log, err := os.ReadFile(filepath.Join(b.root, "daemon.log")); err == nil {
		tail := string(log)
		if len(tail) > 6000 {
			tail = tail[len(tail)-6000:]
		}
		b.t.Logf("scratch daemon log (tail):\n%s", tail)
	}
	b.t.Fatalf("timed out after %s waiting for %s", limit, what)
	return 0
}

// startBusLive builds the shim, starts the scratch daemon and opens a repo
// session in a window, with a test peer in the same group.
func startBusLive(t *testing.T) *busLive {
	root, err := os.MkdirTemp("", "rb") // short: the session sockets live under it
	if err != nil {
		t.Fatal(err)
	}
	b := &busLive{t: t, root: root, group: "g" + filepath.Base(root)}
	b.repo = filepath.Join(root, b.group, "repo")
	os.MkdirAll(filepath.Join(b.repo, ".claude"), 0o755)
	os.MkdirAll(filepath.Join(root, b.group, "tester"), 0o755)
	writeProbeHooks(t, b.repo, filepath.Join(root, "probe.txt"))

	shim := filepath.Join(root, "ccmux-peers")
	if out, err := exec.Command("go", "build", "-o", shim, "ccmux.dev/ccmuxd/cmd/ccmux-peers").CombinedOutput(); err != nil {
		t.Fatalf("building the shim from this tree: %v\n%s", err, out)
	}
	// The sidecar's pinned SDK, in place, so the daemon installs nothing.
	sidecar := filepath.Join(root, "agents", ".ccmux", "claude-serve")
	os.MkdirAll(sidecar, 0o755)
	if err := os.Symlink(os.Getenv("CCMUX_RUNNER_MODULES"), filepath.Join(sidecar, "node_modules")); err != nil {
		t.Fatal(err)
	}

	port, socket := freePort(t), "ccmux-"+filepath.Base(root)
	b.base = fmt.Sprintf("http://127.0.0.1:%d", port)
	cmd := exec.Command(os.Getenv("CCMUX_RUNNER_EXEC"),
		"-socket", socket, "-addr", fmt.Sprintf("127.0.0.1:%d", port),
		"-projects-root", root, "-agents-dir", filepath.Join(root, "agents"), "-worktrees-dir", filepath.Join(root, "wt"),
		"-task-slots", "0", "-runner-unit-prefix", busPrefix, "-peers-shim", shim)
	cmd.Env = append(os.Environ(), "CCMUXD_ROOT="+root, "CCMUX_HOOK_TRACE="+filepath.Join(root, "hooks.jsonl"))
	logFile, _ := os.Create(filepath.Join(root, "daemon.log"))
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.stop(cmd, socket) })

	b.wait("the scratch daemon to answer", 30*time.Second, func() bool { return b.call("GET", "/v1/workspaces", "", nil, nil) == 200 })
	if code := b.call("POST", "/v1/workspaces", "", map[string]string{"name": "repo", "repoPath": b.repo, "cwd": b.repo, "createdBy": "live-test", "group": b.group}, nil); code != 201 {
		t.Fatalf("opening the repo session: HTTP %d", code)
	}
	var info struct{ Token string }
	raw, _ := os.ReadFile(filepath.Join(root, "config", "peers.json"))
	json.Unmarshal(raw, &info)
	b.token = info.Token
	tester := filepath.Join(root, b.group, "tester")
	var reg struct {
		PeerID string `json:"peer_id"`
		Group  string `json:"group"`
	}
	b.call("POST", "/v1/peers/register", b.token, map[string]any{"pid": os.Getpid(), "cwd": tester, "git_root": tester, "name": "tester", "poll_only": true}, &reg)
	if reg.PeerID == "" || !strings.EqualFold(reg.Group, b.group) {
		t.Fatalf("the tester joined the bus in %q, want %q: %+v", reg.Group, b.group, reg)
	}
	b.me = reg.PeerID
	return b
}

// stop ends everything the row started: its sessions' units, the daemon, its
// tmux server, its unit template and its folder.
func (b *busLive) stop(cmd *exec.Cmd, socket string) {
	exec.Command("systemctl", "--user", "stop", busPrefix+"@*.service").Run()
	cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
	}
	exec.Command("tmux", "-L", socket, "kill-server").Run()
	home, _ := os.UserHomeDir()
	os.Remove(filepath.Join(home, ".config", "systemd", "user", busPrefix+"@.service"))
	Systemctl{}.Reload()
	os.RemoveAll(b.root)
}

// writeProbeHooks gives the repo project hooks that note each event they
// fire for, so the row can say which of Claude Code's hooks run in a runner.
func writeProbeHooks(t *testing.T, repo, out string) {
	hooks := map[string]any{}
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "PreToolUse", "PostToolUse", "Notification"} {
		hooks[ev] = []any{map[string]any{"hooks": []any{map[string]string{"type": "command", "command": "echo " + ev + " >> " + out}}}}
	}
	body, _ := json.Marshal(map[string]any{"hooks": hooks})
	if err := os.WriteFile(filepath.Join(repo, ".claude", "settings.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

type busSession struct {
	ID        string `json:"id"`
	PaneID    string `json:"paneId"`
	State     string `json:"state"`
	Attention string `json:"attention"`
}

func (b *busLive) session(id string) (busSession, int) {
	var out struct {
		Session busSession `json:"session"`
		Waiting int        `json:"waiting"`
	}
	b.call("GET", "/v1/sessions/"+id, "", nil, &out)
	return out.Session, out.Waiting
}

// peers is who the tester sees in its group.
func (b *busLive) peers() map[string]string {
	var list []struct{ ID, Name string }
	b.call("POST", "/v1/peers/list", b.token, map[string]string{"peer_id": b.me, "scope": "project"}, &list)
	out := map[string]string{}
	for _, p := range list {
		out[p.Name] = p.ID
	}
	return out
}

// reply waits for a bus message to the tester holding text, and says who
// sent it.
func (b *busLive) reply(text string, limit time.Duration) (from string, took time.Duration) {
	took = b.wait("the reply "+text+" on the bus", limit, func() bool {
		var out struct {
			Events []struct {
				Text   string `json:"text"`
				FromID string `json:"from_id"`
			} `json:"events"`
		}
		b.call("POST", "/v1/peers/poll", b.token, map[string]string{"peer_id": b.me}, &out)
		for _, ev := range out.Events {
			if strings.Contains(ev.Text, text) {
				from = ev.FromID
			}
		}
		return from != ""
	})
	return from, took
}

func (b *busLive) send(toName, text string) {
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	b.call("POST", "/v1/peers/send", b.token, map[string]string{"from_id": b.me, "to_name": toName, "text": text}, &resp)
	if !resp.OK {
		b.t.Fatalf("send to %s: %s", toName, resp.Error)
	}
}

// environOf is a process's environment, and childrenOf its child pids.
func environOf(pid string) string {
	raw, _ := os.ReadFile("/proc/" + pid + "/environ")
	return strings.ReplaceAll(string(raw), "\x00", "\n")
}

func childrenOf(pid string) []string {
	out, _ := exec.Command("pgrep", "-P", pid).Output()
	return strings.Fields(string(out))
}

// hooksSeen is the hook events the trace file and the probe file hold.
func (b *busLive) hooksSeen() (traced, probed []string) {
	raw, _ := os.ReadFile(filepath.Join(b.root, "hooks.jsonl"))
	seen := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		var l struct{ Stage, Event, Detail string }
		if json.Unmarshal([]byte(line), &l) == nil && l.Event != "" && !seen[l.Stage+" "+l.Event+" "+l.Detail] {
			seen[l.Stage+" "+l.Event+" "+l.Detail] = true
			traced = append(traced, strings.TrimSpace(l.Stage+" "+l.Event+" "+l.Detail))
		}
	}
	raw, _ = os.ReadFile(filepath.Join(b.root, "probe.txt"))
	got := map[string]bool{}
	for _, ev := range strings.Fields(string(raw)) {
		if !got[ev] {
			got[ev] = true
			probed = append(probed, ev)
		}
	}
	return traced, probed
}

func liveBusRow(t *testing.T) {
	b := startBusLive(t)
	var sess busSession
	if code := b.call("POST", "/v1/sessions", "", map[string]string{"cwd": b.repo, "model": "haiku"}, &sess); code != 201 || sess.PaneID == "" {
		t.Fatalf("making the session: HTTP %d %+v", code, sess)
	}
	name := "repo-s-" + sess.ID[:8]
	b.wait("the session to be listed on the bus under "+name, 10*time.Second, func() bool { return b.peers()[name] != "" })
	if b.peers()["repo"] != "" {
		t.Fatal("the plain repo name is the repo's own session's, and no Claude runs there")
	}
	peerID := b.peers()[name]

	// Up and through its first turn, so the timed message meets a runner
	// that is running and idle.
	b.call("POST", "/v1/sessions/"+sess.ID+"/prompt", "", map[string]string{"text": "Reply with exactly: READY"}, nil)
	b.wait("the first turn", 3*time.Minute, func() bool { return said(sess.ID, "assistant", "READY") > 0 })
	b.wait("the session to settle", time.Minute, func() bool { s, _ := b.session(sess.ID); return s.Attention == "done" })

	b.send(name, "Send me a message on the bus that says exactly: BUS-PONG")
	sent := time.Now()
	handed := b.wait("the message in Claude's transcript", time.Minute, func() bool { return said(sess.ID, "user", "[claude-peers message from tester") > 0 })
	from, _ := b.reply("BUS-PONG", 3*time.Minute)
	t.Logf("LATENCY bus message -> runner's transcript: %s; -> its reply back on the bus: %s", handed, time.Since(sent))
	if from != peerID {
		t.Fatalf("the reply came from %s, want the session's peer %s", from, peerID)
	}
	if handed > 3*time.Second {
		t.Errorf("a message to a running session took %s to arrive, want under 3s", handed)
	}
	b.wait("the turn to end", time.Minute, func() bool { s, _ := b.session(sess.ID); return s.Attention == "done" })
	if n := said(sess.ID, "user", "[claude-peers message from tester"); n != 1 {
		t.Fatalf("the message is in the transcript %d times, want once", n)
	}
	b.checkProcesses(sess)

	// Asleep, it stays on the bus, and a message wakes it.
	b.call("POST", "/v1/sessions/"+sess.ID+"/wanted", "", map[string]string{"wanted": "asleep"}, nil)
	b.wait("the session to sleep", time.Minute, func() bool {
		active, _ := (Systemctl{}).Active(busPrefix + "@" + sess.ID + ".service")
		return !active
	})
	if b.peers()[name] != peerID {
		t.Fatal("an asleep session left the bus")
	}
	b.send(name, "Send me a message on the bus that says exactly: WOKE-UP")
	woke := time.Now()
	b.reply("WOKE-UP", 4*time.Minute)
	t.Logf("LATENCY bus message -> asleep session woken and replied: %s", time.Since(woke))
	if s, _ := b.session(sess.ID); s.State != "running" {
		t.Fatalf("woken by the message: state %s", s.State)
	}

	b.reportHooks(t, sess)
	if code := b.call("POST", "/v1/sessions/"+sess.ID+"/wanted", "", map[string]string{"wanted": "done"}, nil); code != 200 {
		t.Fatalf("ending the session: HTTP %d", code)
	}
	if b.peers()[name] != "" {
		t.Fatal("an ended session is still on the bus")
	}
}

// checkProcesses reads the running session's processes: its Claude talks to
// the scratch daemon's proxy on its own pane route and does not carry the
// bus-owner mark (so a claude it starts cannot inherit it); its shim is the
// one built from this tree, and carries it.
func (b *busLive) checkProcesses(sess busSession) {
	t := b.t
	node := mainPID(busPrefix + "@" + sess.ID + ".service")
	var claude, shim string
	for _, pid := range childrenOf(node) {
		if strings.Contains(environOf(pid), "CCMUX_PANE_ID="+sess.PaneID) {
			claude = pid
		}
	}
	if claude == "" {
		t.Fatalf("no Claude under the runner %s", node)
	}
	env := environOf(claude)
	for _, want := range []string{
		"ANTHROPIC_BASE_URL=" + b.base + "/llm/pane/" + sess.PaneID, "CCMUX_DAEMON_URL=" + b.base, "CCMUX_PANE_TOKEN=",
		"CCMUX_HOOKS_SOCK=" + filepath.Join(b.root, "run", "hooks.sock"), "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1",
		"CLAUDE_CODE_MAX_MCP_DESCRIPTION_LENGTH=", "CCMUX_PEERS_CHANNEL=0",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("the session's Claude lacks %s", want)
		}
	}
	if strings.Contains(env, "CCMUX_BUS_OWNER") {
		t.Error("the bus-owner mark is in Claude's own environment: a claude it starts would inherit it")
	}
	for _, pid := range childrenOf(claude) {
		if exe, _ := os.Readlink("/proc/" + pid + "/exe"); exe == filepath.Join(b.root, "ccmux-peers") {
			shim = pid
		}
	}
	if shim == "" || !strings.Contains(environOf(shim), "CCMUX_BUS_OWNER=1") {
		t.Errorf("the tree-built shim is not running as the pane's bus owner under Claude %s (shim pid %q)", claude, shim)
	}
	// And the turns it has run went through that route: the proxy logs each
	// request with the pane it came from.
	if log, _ := os.ReadFile(filepath.Join(b.root, "daemon.log")); !strings.Contains(string(log), "llm: pane "+sess.PaneID+" ") || !strings.Contains(string(log), "POST /v1/messages") {
		t.Errorf("the scratch daemon's proxy logged no model request from pane %s", sess.PaneID)
	}
}

// reportHooks says which hooks ran in the session: the user's own (the
// trace their ccmux hook script and the scratch daemon write) and the
// repo's probe hooks. It waits past Claude Code's 60s idle reminder, since
// whether that fires in an SDK session decides if a finished turn alerts.
func (b *busLive) reportHooks(t *testing.T, sess busSession) {
	b.wait("the last turn to end", time.Minute, func() bool { s, _ := b.session(sess.ID); return s.Attention == "done" })
	time.Sleep(70 * time.Second)
	traced, probed := b.hooksSeen()
	t.Logf("HOOKS probe hooks that fired in the session: %v", probed)
	t.Logf("HOOKS user hook trace (stage event detail):\n  %s", strings.Join(traced, "\n  "))
	for _, want := range []string{"SessionStart", "UserPromptSubmit", "Stop"} {
		if !strings.Contains(" "+strings.Join(probed, " ")+" ", " "+want+" ") {
			t.Errorf("the %s hook did not fire in the runner session", want)
		}
	}
}
