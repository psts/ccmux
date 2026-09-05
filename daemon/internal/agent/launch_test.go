package agent

import (
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/harness"
)

func TestLaunchCommandOpencode(t *testing.T) {
	d := Definition{Name: "x-poster", Model: "anthropic/claude-sonnet-5"}
	l := LaunchCommand(d, harness.Harness{Name: "opencode", Command: "opencode"}, "/b", "/repo", "post about it's release")
	if l.Persist != "CLAUDE_PEERS_NAME=x-poster opencode --agent x-poster --model anthropic/claude-sonnet-5" {
		t.Errorf("persist %q", l.Persist)
	}
	if !strings.HasSuffix(l.Deliver, ` --prompt 'post about it'\''s release'`) || !strings.HasPrefix(l.Deliver, l.Persist) {
		t.Errorf("deliver %q", l.Deliver)
	}
	if LaunchCommand(d, harness.Harness{Name: "opencode", Command: "opencode"}, "/b", "/repo", "").Deliver != l.Persist {
		t.Error("no prompt: deliver must equal persist")
	}
}

func TestLaunchCommandClaude(t *testing.T) {
	d := Definition{Name: "kb-writer", AddDirs: []string{"/srv/kb"}}
	h := harness.Harness{Name: harness.Builtin, Command: harness.FallbackClaudeCommand}
	l := LaunchCommand(d, h, "/home/u/.ccmux/agents/kb-writer", "/repo", "write it")
	for _, want := range []string{
		"CLAUDE_PEERS_NAME=kb-writer env -u TMUX claude --dangerously-load-development-channels server:claude-peers",
		"--name kb-writer", "--plugin-dir /home/u/.ccmux/agents/kb-writer",
		"--append-system-prompt-file /home/u/.ccmux/agents/kb-writer/AGENTS.md",
		"--add-dir /repo", "--add-dir /srv/kb",
	} {
		if !strings.Contains(l.Persist, want) {
			t.Errorf("persist missing %q: %s", want, l.Persist)
		}
	}
	if strings.Contains(l.Persist, "write it") {
		t.Error("prompt must not be persisted")
	}
	if !strings.HasSuffix(l.Deliver, " -- 'write it'") {
		t.Errorf("claude prompt must be positional after --: %s", l.Deliver)
	}
}

func TestLaunchCommandOtherHarness(t *testing.T) {
	l := LaunchCommand(Definition{Name: "a-b"}, harness.Harness{Name: "pi", Command: "pi"}, "/b", "/r", "hi there")
	if l.Persist != "CLAUDE_PEERS_NAME=a-b pi" || l.Deliver != "CLAUDE_PEERS_NAME=a-b pi 'hi there'" {
		t.Errorf("%+v", l)
	}
}
