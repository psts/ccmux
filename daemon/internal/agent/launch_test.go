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
	if ChatPort(l.Persist) != 41234 {
		t.Error("port must be readable back from the persisted line")
	}
}

func TestLaunchCommandClaude(t *testing.T) {
	d := Definition{Name: "kb-writer", AddDirs: []string{"/srv/kb"}, Model: "opus"}
	h := harness.Harness{Name: harness.Builtin, Command: harness.FallbackClaudeCommand}
	opts := LaunchOpts{Dirs: []string{"/repo", "/repo2"}, Prompt: "write it", Port: 41235, Serve: "/home/u/.ccmux/agents/.ccmux/claude-serve/serve.mjs", Node: "/usr/bin/node"}
	l := LaunchCommand(d, h, "/home/u/.ccmux/agents/kb-writer", opts)
	for _, want := range []string{
		"CLAUDE_PEERS_NAME=kb-writer /usr/bin/node /home/u/.ccmux/agents/.ccmux/claude-serve/serve.mjs serve --port 41235",
		"--agent kb-writer", "--plugin-dir /home/u/.ccmux/agents/kb-writer",
		"--system-prompt-file /home/u/.ccmux/agents/kb-writer/AGENTS.md",
		"--add-dir /repo --add-dir /repo2 --add-dir /srv/kb", "--model opus",
	} {
		if !strings.Contains(l.Persist, want) {
			t.Errorf("persist missing %q: %s", want, l.Persist)
		}
	}
	if strings.Contains(l.Persist, "write it") || l.Deliver != l.Persist {
		t.Errorf("the prompt travels over the port, never the line: %q / %q", l.Persist, l.Deliver)
	}
	if strings.Contains(l.Persist, "claude --") {
		t.Errorf("the harness's own TUI line must not be used: %s", l.Persist)
	}
	if ChatPort(l.Persist) != 41235 {
		t.Error("port must be readable back from the persisted line")
	}
	opts.Session = "0f0e0d0c-1111-2222-3333-444444444444"
	l = LaunchCommand(d, h, "/b", opts)
	if !strings.HasSuffix(l.Deliver, " --session 0f0e0d0c-1111-2222-3333-444444444444") || strings.Contains(l.Persist, "--session") {
		t.Errorf("resume rides on the typed line only: %q / %q", l.Deliver, l.Persist)
	}
	// Without a node path the line falls back to whatever the pane's PATH has.
	if bare := LaunchCommand(d, h, "/b", LaunchOpts{Serve: "/s.mjs"}).Persist; !strings.Contains(bare, " node /s.mjs serve ") {
		t.Errorf("bare node fallback: %s", bare)
	}
	// Default permissions: read/edit allow, bash/webfetch ask → only an allow list.
	def := LaunchCommand(withDefaults(d), h, "/b", LaunchOpts{}).Persist
	if !strings.Contains(def, "--allowed-tools 'Edit,Glob,Grep,Read,Write'") || strings.Contains(def, "--disallowed-tools") {
		t.Errorf("default permission flags: %s", def)
	}
	locked := d
	locked.Permissions = Permissions{Read: "allow", Edit: "deny", Bash: "ask", Webfetch: "deny", BashAllow: []string{"git log *"}}
	got := LaunchCommand(locked, h, "/b", LaunchOpts{}).Persist
	if !strings.Contains(got, "--allowed-tools 'Bash(git log *),Glob,Grep,Read'") || !strings.Contains(got, "--disallowed-tools 'Edit,WebFetch,WebSearch,Write'") {
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
	if ChatPort(l.Persist) != 5000 {
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
	if strings.Contains(l.Persist, "--session") || ChatPort(l.Persist) != 7 {
		t.Errorf("persist: %q", l.Persist)
	}
}
