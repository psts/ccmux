package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/harness"
)

// A claude start goes through the sidecar: it lands BEFORE the resume
// lookup (which reads history through it), its script and node reach the
// launch line with a real chat port, the prompt stays off the line, and a
// host that cannot install it gets a 503 naming the reason. The fixture's
// own harness has no chat, which is the row none of this may touch.
func TestResolveAgentLaunch_ClaudeSidecar(t *testing.T) {
	f := newWindowAgentFixture(t, "sleep 1;:")
	d, _, msg := f.srv.agentDefinition("x-poster")
	if msg != "" {
		t.Fatal(msg)
	}
	d.Harness, d.Start = harness.Builtin, "continue"
	landed, historyRead := false, false
	f.srv.ensureSidecar = func() (string, string, error) {
		landed = true
		return "/opt/ccmux/claude-serve/serve.mjs", "/opt/node/bin/node", nil
	}
	f.srv.offlineSessions = func(_ context.Context, harnessName, dir string) ([]agent.OpencodeSession, error) {
		historyRead = true
		if !landed {
			return nil, errors.New("claude sidecar not installed yet")
		}
		if harnessName != harness.Builtin {
			return nil, fmt.Errorf("history asked for the %s harness", harnessName)
		}
		return []agent.OpencodeSession{{ID: "ses_prev", Directory: dir}}, nil
	}

	l, status, msg := f.srv.resolveAgentLaunch(d, t.TempDir(), []string{"/repo"}, "", "", "post it", nil)
	if msg != "" {
		t.Fatalf("claude start: %d %s", status, msg)
	}
	if !strings.Contains(l.Persist, "/opt/node/bin/node /opt/ccmux/claude-serve/serve.mjs serve --port ") {
		t.Errorf("the sidecar and node must reach the line: %s", l.Persist)
	}
	if port := agent.ChatPort(l.Persist); port == 0 || port != l.Port {
		t.Errorf("a claude start needs a real chat port on its line (%d) matching the launch (%d)", port, l.Port)
	}
	if strings.Contains(l.Persist, "post it") || l.Prompt != "post it" {
		t.Errorf("the prompt travels over the port, not the line: %q / %q", l.Persist, l.Prompt)
	}
	if !strings.HasSuffix(l.Deliver, " --session ses_prev") || strings.Contains(l.Persist, "--session") {
		t.Errorf("continue rides the typed line only: %q / %q", l.Deliver, l.Persist)
	}
	if !historyRead {
		t.Error("start: continue must have looked up the conversation to continue")
	}

	// A host without node: the start says so, and never reaches the history
	// read that would have failed with a less useful message.
	landed, historyRead = false, false
	f.srv.ensureSidecar = func() (string, string, error) {
		return "", "", errors.New("the claude harness for agents needs node (Node.js 18+) on this host: node: not found")
	}
	if _, status, msg := f.srv.resolveAgentLaunch(d, t.TempDir(), nil, "", "", "", nil); status != 503 || !strings.Contains(msg, "node") {
		t.Errorf("no node: %d %s", status, msg)
	}
	if historyRead {
		t.Error("the sidecar comes first: a start that cannot land it must not read history through it")
	}

	// The fixture's own harness has no chat: no sidecar, no port, no history.
	d.Harness = "noop"
	f.srv.ensureSidecar = func() (string, string, error) {
		t.Error("sidecar looked up for a harness with no chat")
		return "", "", nil
	}
	l, status, msg = f.srv.resolveAgentLaunch(d, t.TempDir(), nil, "", "", "post it", nil)
	if msg != "" || l.Port != 0 || agent.ChatPort(l.Persist) != 0 || strings.Contains(l.Persist, "serve.mjs") {
		t.Errorf("noop harness: %d %s port=%d %s", status, msg, l.Port, l.Persist)
	}
}
