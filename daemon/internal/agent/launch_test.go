package agent

import (
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/harness"
)

func TestLaunchCommandOpencode(t *testing.T) {
	d := Definition{Name: "x-poster", Model: "anthropic/claude-sonnet-5"}
	l := LaunchCommand(d, harness.Harness{Name: "opencode", Command: "opencode"}, "/b", LaunchOpts{Dirs: []string{"/repo"}, Prompt: "post about it's release", Port: 41234})
	if l.Persist != "CLAUDE_PEERS_NAME=x-poster opencode --agent x-poster --port 41234 --model anthropic/claude-sonnet-5" {
		t.Errorf("persist %q", l.Persist)
	}
	if l.Deliver != l.Persist || strings.Contains(l.Deliver, "post about") {
		t.Errorf("opencode prompt must travel over the port, not the command line: %q", l.Deliver)
	}
	if OpencodePort(l.Persist) != 41234 {
		t.Error("port must be readable back from the persisted line")
	}
}

func TestLaunchCommandClaude(t *testing.T) {
	d := Definition{Name: "kb-writer", AddDirs: []string{"/srv/kb"}}
	h := harness.Harness{Name: harness.Builtin, Command: harness.FallbackClaudeCommand}
	l := LaunchCommand(d, h, "/home/u/.ccmux/agents/kb-writer", LaunchOpts{Dirs: []string{"/repo", "/repo2"}, Prompt: "write it"})
	for _, want := range []string{
		"CLAUDE_PEERS_NAME=kb-writer env -u TMUX claude --dangerously-load-development-channels server:claude-peers",
		"--name kb-writer", "--plugin-dir /home/u/.ccmux/agents/kb-writer",
		"--append-system-prompt-file /home/u/.ccmux/agents/kb-writer/AGENTS.md",
		"--add-dir /repo --add-dir /repo2 --add-dir /srv/kb",
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
	// Default permissions: read/edit allow, bash/webfetch ask → only an allow list.
	def := LaunchCommand(withDefaults(d), h, "/b", LaunchOpts{}).Persist
	if !strings.Contains(def, "--allowedTools 'Edit,Glob,Grep,Read,Write'") || strings.Contains(def, "--disallowedTools") {
		t.Errorf("default permission flags: %s", def)
	}
	locked := d
	locked.Permissions = Permissions{Read: "allow", Edit: "deny", Bash: "ask", Webfetch: "deny", BashAllow: []string{"git log *"}}
	got := LaunchCommand(locked, h, "/b", LaunchOpts{}).Persist
	if !strings.Contains(got, "--disallowedTools 'Edit,WebFetch,WebSearch,Write'") || !strings.Contains(got, "--allowedTools 'Bash(git log *),Glob,Grep,Read'") {
		t.Errorf("permission flags: %s", got)
	}
}

func TestLaunchCommandOtherHarness(t *testing.T) {
	l := LaunchCommand(Definition{Name: "a-b"}, harness.Harness{Name: "pi", Command: "pi"}, "/b", LaunchOpts{Prompt: "hi there"})
	if l.Persist != "CLAUDE_PEERS_NAME=a-b pi" || l.Deliver != "CLAUDE_PEERS_NAME=a-b pi 'hi there'" {
		t.Errorf("%+v", l)
	}
}

// Env files are named by path ahead of every harness line, for ccmuxd's
// env-exec verb to load as data; their VALUES never reach the persisted
// command, and the opencode port is still readable behind the prefix.
func TestLaunchCommandSourcesEnvFiles(t *testing.T) {
	files := []string{"/w/shared/.env", "/w/agents/x y/.env"}
	l := LaunchCommand(Definition{Name: "x-poster"}, harness.Harness{Name: "opencode", Command: "opencode"}, "/b", LaunchOpts{EnvFiles: files, Port: 5000})
	want := "ccmuxd env-exec -f /w/shared/.env -f '/w/agents/x y/.env' -- CLAUDE_PEERS_NAME=x-poster opencode --agent x-poster --port 5000"
	if l.Persist != want {
		t.Errorf("persist:\n got %q\nwant %q", l.Persist, want)
	}
	if OpencodePort(l.Persist) != 5000 {
		t.Error("port must be readable behind the env prefix")
	}
	pi := LaunchCommand(Definition{Name: "a-b"}, harness.Harness{Name: "pi", Command: "pi"}, "/b", LaunchOpts{EnvFiles: files, Prompt: "hi"})
	if !strings.HasPrefix(pi.Deliver, "ccmuxd env-exec ") || !strings.HasSuffix(pi.Deliver, " -- CLAUDE_PEERS_NAME=a-b pi hi") {
		t.Errorf("other harness: %q", pi.Deliver)
	}
}

// Continuing a conversation is one start's choice: --session rides on the
// typed line, never on the persisted one a later wake replays.
func TestLaunchCommandResumesOnTheTypedLineOnly(t *testing.T) {
	l := LaunchCommand(Definition{Name: "scout"}, harness.Harness{Name: "opencode", Command: "opencode"}, "/b", LaunchOpts{Port: 7, Session: "ses_abc"})
	if !strings.HasSuffix(l.Deliver, " --port 7 --session ses_abc") {
		t.Errorf("deliver: %q", l.Deliver)
	}
	if strings.Contains(l.Persist, "--session") || OpencodePort(l.Persist) != 7 {
		t.Errorf("persist: %q", l.Persist)
	}
}
