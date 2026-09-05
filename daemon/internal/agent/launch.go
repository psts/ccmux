package agent

import (
	"path/filepath"
	"strings"

	"ccmux.dev/ccmuxd/internal/harness"
)

// Launch is the pair of commands an instance start needs: Persist is what
// the pane records as its startup command (a revive or a keep-alive restart
// runs it as-is), Deliver is what gets typed now — Persist plus the one-shot
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
//     the project is reachable through --add-dir, the prompt is positional
//     after "--" (the channels flag is variadic and would swallow it);
//   - opencode: the instance's opencode.jsonc (WriteInstanceConfig) already
//     carries instructions, permissions and MCP, so only --agent and the
//     prompt remain;
//   - anything else: the harness command as configured, prompt appended
//     positionally, which is what pi and codex accept.
func LaunchCommand(d Definition, h harness.Harness, baseDir, repo, prompt string) Launch {
	prefix := "CLAUDE_PEERS_NAME=" + shellQuote(d.Name) + " "
	var persist, deliver string
	switch h.Name {
	case harness.Builtin:
		persist = prefix + claudeFlags(d, h.Command, baseDir, repo)
		deliver = persist
		if prompt != "" {
			deliver += " -- " + shellQuote(prompt)
		}
	case "opencode":
		persist = prefix + h.Command + " --agent " + shellQuote(d.Name)
		if d.Model != "" {
			persist += " --model " + shellQuote(d.Model)
		}
		deliver = persist
		if prompt != "" {
			deliver += " --prompt " + shellQuote(prompt)
		}
	default:
		persist = prefix + h.Command
		deliver = persist
		if prompt != "" {
			deliver += " " + shellQuote(prompt)
		}
	}
	return Launch{Persist: persist, Deliver: deliver}
}

func claudeFlags(d Definition, cmd, baseDir, repo string) string {
	parts := []string{cmd,
		"--name", shellQuote(d.Name),
		"--plugin-dir", shellQuote(baseDir),
		"--append-system-prompt-file", shellQuote(filepath.Join(baseDir, fileAgents)),
		"--add-dir", shellQuote(repo),
	}
	for _, dir := range d.AddDirs {
		parts = append(parts, "--add-dir", shellQuote(dir))
	}
	if d.Model != "" {
		parts = append(parts, "--model", shellQuote(d.Model))
	}
	return strings.Join(parts, " ")
}

// shellQuote single-quotes s for a POSIX shell; a plain word stays bare so
// the recorded startup command reads like something a human typed.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == '/')
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
