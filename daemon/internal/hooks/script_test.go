package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
)

const notifyScript = "../../../hooks/ccmux-notify.sh"

// The script embeds a python program in a single-quoted shell string, so a single
// apostrophe anywhere inside it — in a comment, even — ends the string and the
// whole script dies with a syntax error. Every hook then silently stops reaching
// ccmux: attention never updates, and the trace that would explain it never gets
// written either. `bash -n` costs nothing and catches it.
func TestNotifyScript_ParsesAsShell(t *testing.T) {
	if _, err := os.Stat(notifyScript); err != nil {
		t.Skipf("notify script not found at %s", notifyScript)
	}
	out, err := exec.Command("bash", "-n", notifyScript).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %v\n%s", err, out)
	}
}

// The specific mistake, pinned so it cannot come back: an apostrophe inside the
// python block. `bash -n` above catches most forms of it, but an apostrophe that
// happens to pair with another one produces a script that parses and then behaves
// wrongly, which is worse.
func TestNotifyScript_PythonBlockHasNoApostrophes(t *testing.T) {
	data, err := os.ReadFile(notifyScript)
	if err != nil {
		t.Skipf("notify script not readable: %v", err)
	}
	lines := strings.Split(string(data), "\n")

	start := -1
	for i, l := range lines {
		if strings.Contains(l, "python3 -c '") {
			start = i
			continue
		}
		if start >= 0 && strings.HasPrefix(l, "' 2>/dev/null") {
			return // reached the end with no apostrophe found
		}
		if start >= 0 && strings.Contains(l, "'") {
			t.Errorf("line %d of ccmux-notify.sh has an apostrophe inside the single-quoted python block, "+
				"which ends the string and breaks every hook:\n  %s", i+1, l)
		}
	}
	if start < 0 {
		t.Error("could not find the python block; this test no longer guards what it claims to")
	}
}

// ccmux notifies about work it owns. A Claude session started in a plain
// terminal has no CCMUX_PANE_ID, no pane to flash and no workspace to name — and
// the daemon would not merely ignore it, because ResolvePane falls back to the
// pane whose CWD is the longest prefix of the hook's. A terminal Claude anywhere
// inside a repo that also has a hosted pane would raise an alert naming THAT
// pane. So the script must leave before it writes or sends anything.
func TestNotifyScript_ForeignSessionDoesNothing(t *testing.T) {
	requireScript(t)
	trace := t.TempDir() + "/trace.jsonl"

	cmd := exec.Command("bash", notifyScript, "stop")
	cmd.Stdin = strings.NewReader(`{"session_id":"s1","cwd":"/repo","hook_event_name":"Stop"}`)
	cmd.Env = append(scrubbedEnv(), "CCMUX_HOOK_TRACE="+trace)
	out, err := cmd.CombinedOutput()

	if err != nil {
		t.Fatalf("script must exit 0 for a foreign session: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(trace); statErr == nil {
		written, _ := os.ReadFile(trace)
		t.Errorf("a foreign session wrote a trace line; it should leave silently:\n%s", written)
	}
}

// A ccmux pane comes in two kinds and BOTH must get through. Daemon-hosted panes
// carry CCMUX_PANE_ID; the Mac app's own local panes carry CCMUX_CMD_FILE and
// never see a pane id. Guarding on the pane id alone silently muted every local
// pane, which is how this test came to exist.
func TestNotifyScript_BothPaneKindsAreAccepted(t *testing.T) {
	requireScript(t)
	for _, env := range []string{"CCMUX_PANE_ID=pane-1", "CCMUX_CMD_FILE=/tmp/ccmux-cmd"} {
		trace := t.TempDir() + "/trace.jsonl"
		cmd := exec.Command("bash", notifyScript, "stop")
		cmd.Stdin = strings.NewReader(`{"session_id":"s1","cwd":"/repo","hook_event_name":"Stop"}`)
		cmd.Env = append(scrubbedEnv(), "CCMUX_HOOK_TRACE="+trace, env,
			"CCMUX_HOOKS_SOCK=/tmp/ccmux-no-such-socket.sock")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: script failed: %v\n%s", env, err, out)
		}
		if _, statErr := os.Stat(trace); statErr != nil {
			t.Errorf("%s: no trace written; this pane kind was silently muted", env)
		}
	}
}

// The counterpart: inside a ccmux pane the script does its job, and the trace
// keeps every payload field worth reading later.
func TestNotifyScript_CcmuxPaneIsTraced(t *testing.T) {
	requireScript(t)
	trace := t.TempDir() + "/trace.jsonl"

	cmd := exec.Command("bash", notifyScript, "subagent-start")
	cmd.Stdin = strings.NewReader(`{"session_id":"s1","cwd":"/repo","agent_id":"a-42","agent_type":"Explore","hook_event_name":"SubagentStart"}`)
	cmd.Env = append(scrubbedEnv(),
		"CCMUX_HOOK_TRACE="+trace,
		"CCMUX_PANE_ID=pane-1",
		"CCMUX_HOOKS_SOCK=/tmp/ccmux-no-such-socket.sock", // nothing listening; the trace still lands
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}

	written, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("no trace written: %v", err)
	}
	for _, want := range []string{`"event": "subagent_start"`, `"agent_id": "a-42"`, `"agent_type": "Explore"`} {
		if !strings.Contains(string(written), want) {
			t.Errorf("trace line is missing %s:\n%s", want, written)
		}
	}
}

// requireScript skips when the script or its interpreters are unavailable.
func requireScript(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"bash", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	if _, err := os.Stat(notifyScript); err != nil {
		t.Skipf("notify script not found at %s", notifyScript)
	}
}

// scrubbedEnv drops any ccmux pane variables inherited from the environment the
// test itself runs in — these tests are frequently run FROM a ccmux pane, where
// CCMUX_PANE_ID is set and would silently invert the foreign-session case.
func scrubbedEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CCMUX_PANE_ID=") || strings.HasPrefix(kv, "CCMUX_HOOKS_SOCK=") ||
			strings.HasPrefix(kv, "CCMUX_CMD_FILE=") || strings.HasPrefix(kv, "CCMUX_HOOK_TRACE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// The subagent events have to reach the daemon, not stop at the trace: they are
// the only thing that tells it a quiet session is still working. They were
// trace-only for as long as they were treated as debugging noise, which is
// exactly how long the false idle alerts lasted.
func TestNotifyScript_SubagentEventsAreDeliveredWithTheirAgentID(t *testing.T) {
	requireScript(t)
	dir := sockDir(t)
	sock := filepath.Join(dir, "hooks.sock")

	for _, c := range []struct{ arg, want string }{
		{"subagent-start", "subagent_start"},
		{"subagent-stop", "subagent_stop"},
	} {
		got := listenOnce(t, sock)
		cmd := exec.Command("bash", notifyScript, c.arg)
		cmd.Stdin = strings.NewReader(`{"session_id":"s1","cwd":"/repo","agent_id":"a-42"}`)
		cmd.Env = append(scrubbedEnv(),
			"CCMUX_HOOK_TRACE="+filepath.Join(dir, "trace.jsonl"),
			"CCMUX_PANE_ID=pane-1",
			"CCMUX_HOOKS_SOCK="+sock,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		select {
		case line := <-got:
			for _, want := range []string{`"type": "` + c.want + `"`, `"agent_id": "a-42"`, `"session_id": "s1"`} {
				if !strings.Contains(line, want) {
					t.Errorf("%s payload is missing %s: %q", c.arg, want, line)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never reached the socket", c.arg)
		}
		_ = os.Remove(sock)
	}
}

// End to end, through the real script and the real socket: the incident this
// was built for. Two agents start, the main loop stops talking, Claude Code's
// idle reminder lands 60 seconds later — and the pane must not be flagged.
func TestNotifyScript_IdleReminderIsHeldEndToEnd(t *testing.T) {
	requireScript(t)
	dir := sockDir(t)
	sock := filepath.Join(dir, "hooks.sock")

	r := &mockRouter{resolve: "pane-1"}
	l, err := Listen(sock, r)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	fire := func(arg, payload string) {
		t.Helper()
		cmd := exec.Command("bash", notifyScript, arg)
		cmd.Stdin = strings.NewReader(payload)
		cmd.Env = append(scrubbedEnv(),
			"CCMUX_HOOK_TRACE="+filepath.Join(dir, "trace.jsonl"),
			"CCMUX_PANE_ID=pane-1",
			"CCMUX_HOOKS_SOCK="+sock,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s failed: %v\n%s", arg, err, out)
		}
	}

	fire("subagent-start", `{"session_id":"s1","cwd":"/repo","agent_id":"a1","agent_type":"Explore"}`)
	fire("subagent-start", `{"session_id":"s1","cwd":"/repo","agent_id":"a2","agent_type":"Explore"}`)
	fire("stop", `{"session_id":"s1","cwd":"/repo"}`)
	fire("notification", `{"session_id":"s1","cwd":"/repo","notification_type":"idle_prompt","message":"Claude is waiting for your input"}`)

	// The session signal from `stop` is the proof the messages arrived at all —
	// without it a broken socket would pass this test by delivering nothing.
	waitFor(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.sigCalls > 0 })
	time.Sleep(200 * time.Millisecond)

	if r.applied(model.AttentionNeedsInput) || r.applied(model.AttentionDone) {
		t.Fatalf("pane was told the turn ended while two Explore agents were running: %v", r.atts)
	}
}

// A cwd is what maps a hook to a workspace, and the subagent events do not need
// one — they are tracked by session id. Requiring one would silently disable the
// hold instead of failing loudly.
func TestNotifyScript_SubagentEventNeedsNoCwd(t *testing.T) {
	requireScript(t)
	dir := sockDir(t)
	sock := filepath.Join(dir, "hooks.sock")
	got := listenOnce(t, sock)

	cmd := exec.Command("bash", notifyScript, "subagent-start")
	cmd.Stdin = strings.NewReader(`{"session_id":"s1","agent_id":"a-42"}`)
	cmd.Env = append(scrubbedEnv(),
		"CCMUX_HOOK_TRACE="+filepath.Join(dir, "trace.jsonl"),
		"CCMUX_PANE_ID=pane-1",
		"CCMUX_HOOKS_SOCK="+sock,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}

	select {
	case line := <-got:
		if !strings.Contains(line, `"agent_id": "a-42"`) {
			t.Errorf("delivered payload = %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a subagent_start without a cwd was dropped; the hold would silently stop working")
	}
}

// install-hooks.sh claims --routed-only "cannot quietly prune the pair and
// restore the false alerts". Moving those two tuples back into TRACE_ONLY would
// disable the whole feature for anyone who runs the flag, and until this test
// existed nothing would have failed.
func TestInstallHooks_RoutedOnlyKeepsTheSubagentPair(t *testing.T) {
	requireScript(t)
	installer := "../../../hooks/install-hooks.sh"
	if _, err := os.Stat(installer); err != nil {
		t.Skipf("installer not found at %s", installer)
	}
	settings := filepath.Join(t.TempDir(), "settings.json")

	for _, args := range [][]string{{}, {"--routed-only"}} {
		cmd := exec.Command("bash", append([]string{installer}, args...)...)
		cmd.Env = append(scrubbedEnv(), "CCMUX_CLAUDE_SETTINGS="+settings)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("installer %v failed: %v\n%s", args, err, out)
		}
	}

	data, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("no settings written: %v", err)
	}
	for _, want := range []string{"SubagentStart", "SubagentStop", "subagent-start", "subagent-stop"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("--routed-only pruned %s; the hold is silently dead for anyone who runs it", want)
		}
	}
	// The trace-only set really is pruned, so the test proves the flag works
	// rather than that it does nothing.
	if strings.Contains(string(data), "TeammateIdle") {
		t.Error("--routed-only kept a trace-only hook; this test would pass even if the flag were a no-op")
	}
}
