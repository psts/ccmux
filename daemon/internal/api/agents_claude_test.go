package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

	dir := t.TempDir()
	l, status, msg := f.srv.resolveAgentLaunch(d, dir, []string{"/repo"}, "", "", "post it", nil)
	if msg != "" {
		t.Fatalf("claude start: %d %s", status, msg)
	}
	if !claudeProjectTrusted(t, f.srv.claudeConfig, dir) {
		t.Errorf("a claude start must mark %s trusted in %s", dir, f.srv.claudeConfig)
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

	// A Claude Code config that does not parse is left alone, and the start
	// says so instead of running a session that drops the brief's imports.
	f.srv.ensureSidecar = func() (string, string, error) { return "/s/serve.mjs", "/n/node", nil }
	if err := os.WriteFile(f.srv.claudeConfig, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, status, msg := f.srv.resolveAgentLaunch(d, t.TempDir(), nil, "", "", "", nil); status != 503 || !strings.Contains(msg, "untouched") {
		t.Errorf("broken claude config: %d %s", status, msg)
	}
	if got, _ := os.ReadFile(f.srv.claudeConfig); string(got) != "{oops" {
		t.Errorf("broken claude config rewritten: %q", got)
	}
	if err := os.Remove(f.srv.claudeConfig); err != nil {
		t.Fatal(err)
	}

	// The fixture's own harness has no chat: no sidecar, no port, no history.
	d.Harness = "noop"
	f.srv.ensureSidecar = func() (string, string, error) {
		t.Error("sidecar looked up for a harness with no chat")
		return "", "", nil
	}
	dir = t.TempDir()
	l, status, msg = f.srv.resolveAgentLaunch(d, dir, nil, "", "", "post it", nil)
	if msg != "" || l.Port != 0 || agent.ChatPort(l.Persist) != 0 || strings.Contains(l.Persist, "serve.mjs") {
		t.Errorf("noop harness: %d %s port=%d %s", status, msg, l.Port, l.Persist)
	}
	if _, err := os.Stat(f.srv.claudeConfig); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("another harness must not touch the Claude Code config: %v", err)
	}
}

// claudeProjectTrusted reports whether the Claude Code config at path holds
// all three trust / external-include flags for dir.
func claudeProjectTrusted(t *testing.T, path, dir string) bool {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Logf("claude config: %v", err)
		return false
	}
	var cfg struct {
		Projects map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Logf("claude config: %v", err)
		return false
	}
	p := cfg.Projects[dir]
	for _, flag := range []string{"hasTrustDialogAccepted", "hasClaudeMdExternalIncludesApproved", "hasClaudeMdExternalIncludesWarningShown"} {
		if v, _ := p[flag].(bool); !v {
			return false
		}
	}
	return true
}
