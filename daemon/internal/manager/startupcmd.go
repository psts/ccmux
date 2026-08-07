package manager

import (
	"path/filepath"
	"strings"
)

// startupProgram returns the base name of the program a startup command runs,
// looking past the wrappers a command line puts in front of it: leading
// `VAR=value` assignments and an `env` invocation with its options.
//
// It exists because two callers ask the same question ("is this a claude
// pane?") and used to answer it differently — one matched fields[0] == "claude",
// the other skipped only `VAR=value`. Neither survived FallbackStartupCommand
// gaining its `env -u TMUX` prefix: panes came up titled "Terminal".
//
// Returns "" when there is no program (empty command, or `env` with nothing
// after its flags).
func startupProgram(startupCmd string) string {
	fields := strings.Fields(startupCmd)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch {
		case strings.Contains(f, "="):
			continue // VAR=value, either a shell assignment or an env operand
		case filepath.Base(f) == "env":
			// Skip env's own options. -u/--unset take a separate argument
			// unless it was given as --unset=NAME (caught by the "=" case).
			for i+1 < len(fields) {
				switch next := fields[i+1]; {
				case next == "-u" || next == "--unset":
					i += 2
					continue
				case next == "-" || next == "-i" || next == "--ignore-environment":
					i++
					continue
				case strings.Contains(next, "="):
					i++
					continue
				}
				break
			}
		default:
			return filepath.Base(f)
		}
	}
	return ""
}
