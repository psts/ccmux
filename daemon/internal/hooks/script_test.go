package hooks

import (
	"os"
	"os/exec"
	"strings"
	"testing"
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

// The whole point of routing the subagent events is that the daemon can count
// live agents, which needs agent_id on the wire.
func TestNotifyScript_ForwardsAgentID(t *testing.T) {
	for _, bin := range []string{"bash", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	if _, err := os.Stat(notifyScript); err != nil {
		t.Skipf("notify script not found at %s", notifyScript)
	}

	trace := t.TempDir() + "/trace.jsonl"
	cmd := exec.Command("bash", notifyScript, "subagent-start")
	cmd.Stdin = strings.NewReader(`{"session_id":"s1","cwd":"/repo","agent_id":"a-42","hook_event_name":"SubagentStart"}`)
	cmd.Env = append(os.Environ(),
		"CCMUX_HOOK_TRACE="+trace,
		"CCMUX_HOOKS_SOCK=/tmp/ccmux-no-such-socket.sock", // nothing listening; the trace still lands
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}

	written, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("no trace written: %v", err)
	}
	if !strings.Contains(string(written), `"agent_id": "a-42"`) {
		t.Errorf("trace line is missing the agent id:\n%s", written)
	}
	if !strings.Contains(string(written), `"event": "subagent_start"`) {
		t.Errorf("trace line has the wrong event:\n%s", written)
	}
}
