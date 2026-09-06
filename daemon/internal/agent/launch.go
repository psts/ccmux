package agent

import (
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"ccmux.dev/ccmuxd/internal/harness"
)

// Launch is the pair of commands an instance start needs: Persist is what
// the pane records as its startup command (a revive runs it as-is; a
// keep-alive wake regenerates it from the current base), Deliver is what
// gets typed now — Persist plus the one-shot
// first prompt, if any. The prompt is deliberately never persisted: a
// restart must not replay a message.
type Launch struct {
	Persist string
	Deliver string
}

// LaunchCommand renders the harness command for base d. Every harness gets
// CLAUDE_PEERS_NAME so the peers shim registers under the agent's name
// instead of the folder's. The rest is per harness:
//
//   - claude: the base rides in as a plugin and an appended system prompt,
//     the window's project folders (dirs) are reachable through --add-dir,
//     the prompt is positional after "--" (the channels flag is variadic and
//     would swallow it);
//   - opencode: the instance's opencode.jsonc (WriteInstanceConfig) already
//     carries instructions, permissions and MCP, so only --agent and the
//     server --port remain; the prompt travels over that port afterwards;
//   - anything else: the harness command as configured, prompt appended
//     positionally, which is what pi and codex accept.
func LaunchCommand(d Definition, h harness.Harness, baseDir string, dirs []string, prompt string, port int) Launch {
	prefix := "CLAUDE_PEERS_NAME=" + shellQuote(d.Name) + " "
	var persist, deliver string
	switch h.Name {
	case harness.Builtin:
		persist = prefix + claudeFlags(d, h.Command, baseDir, dirs)
		deliver = persist
		if prompt != "" {
			deliver += " -- " + shellQuote(prompt)
		}
	case "opencode":
		// The prompt is NOT on this line: --prompt only prefills the TUI. The
		// caller pushes it through the server on --port (PushPrompt), which
		// is why the port is persisted: a wake reads it back (OpencodePort).
		persist = prefix + h.Command + " --agent " + shellQuote(d.Name) + " --port " + strconv.Itoa(port)
		if d.Model != "" {
			persist += " --model " + shellQuote(d.Model)
		}
		deliver = persist
	default:
		persist = prefix + h.Command
		deliver = persist
		if prompt != "" {
			deliver += " " + shellQuote(prompt)
		}
	}
	return Launch{Persist: persist, Deliver: deliver}
}

func claudeFlags(d Definition, cmd, baseDir string, dirs []string) string {
	parts := []string{cmd,
		"--name", shellQuote(d.Name),
		"--plugin-dir", shellQuote(baseDir),
		"--append-system-prompt-file", shellQuote(filepath.Join(baseDir, fileAgents)),
	}
	for _, dir := range dirs {
		parts = append(parts, "--add-dir", shellQuote(dir))
	}
	for _, dir := range d.AddDirs {
		parts = append(parts, "--add-dir", shellQuote(dir))
	}
	if d.Model != "" {
		parts = append(parts, "--model", shellQuote(d.Model))
	}
	parts = append(parts, claudePermissionFlags(d.Permissions)...)
	return strings.Join(parts, " ")
}

// claudeTools maps the neutral permission gates onto Claude Code tool names.
var claudeTools = map[string][]string{
	"read": {"Read", "Glob", "Grep"}, "edit": {"Edit", "Write"}, "bash": {"Bash"}, "webfetch": {"WebFetch", "WebSearch"},
}

// claudePermissionFlags translates Permissions for the claude harness: deny →
// --disallowedTools, allow → --allowedTools, ask → nothing (the default
// prompt), and each BashAllow pattern → an allowed Bash(pattern). opencode
// gets the same gates through its config (opencodePermission).
func claudePermissionFlags(p Permissions) []string {
	var allow, deny []string
	for gate, v := range map[string]string{"read": p.Read, "edit": p.Edit, "bash": p.Bash, "webfetch": p.Webfetch} {
		switch v {
		case "allow":
			allow = append(allow, claudeTools[gate]...)
		case "deny":
			deny = append(deny, claudeTools[gate]...)
		}
	}
	for _, pat := range p.BashAllow {
		allow = append(allow, "Bash("+pat+")")
	}
	sort.Strings(allow)
	sort.Strings(deny)
	var out []string
	if len(allow) > 0 {
		out = append(out, "--allowedTools", shellQuote(strings.Join(allow, ",")))
	}
	if len(deny) > 0 {
		out = append(out, "--disallowedTools", shellQuote(strings.Join(deny, ",")))
	}
	return out
}

// bareWord is what may go on a shell line unquoted, so the recorded startup
// command reads like something a human typed.
var bareWord = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

// shellQuote single-quotes s for a POSIX shell unless it is a bare word.
func shellQuote(s string) string {
	if bareWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
