// Package shellint reproduces ccmux's invisible zsh integration for hosted
// panes: a temporary ZDOTDIR of proxy dotfiles that source the user's real
// config, then install a preexec hook capturing the typed command line (aliases
// included) to $CCMUX_CMD_FILE. Ported from TerminalStore.setupZdotdir. The
// integration must never inject visible commands into the terminal (project
// rule) — everything happens via startup files.
package shellint

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CmdFilePath is where a pane's shell records its last typed command line, read
// by the daemon to capture a faithful (alias-preserving) revive command.
func CmdFilePath(paneID string) string { return filepath.Join(os.TempDir(), "ccmux-cmd-"+paneID) }

// ZdotdirPath is the per-pane proxy ZDOTDIR.
func ZdotdirPath(paneID string) string { return filepath.Join(os.TempDir(), "ccmux-zsh-"+paneID) }

// EnvForPane returns the environment a hosted pane's shell should receive.
// Unlike the local driver path, there is NO TERM_PROGRAM=WezTerm spoof: inside
// tmux the inner app sees TERM_PROGRAM=tmux and TERM=tmux-256color (from the
// managed config's default-terminal), and tmux — not a spoof — negotiates
// extended keys. COLORTERM keeps 24-bit color.
func EnvForPane(paneID string) map[string]string {
	return map[string]string{
		"COLORTERM":      "truecolor",
		"CCMUX_PANE_ID":  paneID,
		"CCMUX_CMD_FILE": CmdFilePath(paneID),
		"ZDOTDIR":        ZdotdirPath(paneID),
	}
}

// proxyZshrc installs the preexec command-capture hook and the kitty-keyboard
// reset precmd, after sourcing the user's real ~/.zshrc. Resetting ZDOTDIR back
// to $HOME means nested shells use the user's normal config.
const proxyZshrc = `ZDOTDIR="$HOME"
[[ -f "$HOME/.zshrc" ]] && source "$HOME/.zshrc"
if [[ -n "$CCMUX_CMD_FILE" ]]; then
    __ccmux_preexec() { print -r -- "$1" > "$CCMUX_CMD_FILE" }
    preexec_functions+=(__ccmux_preexec)
fi
__ccmux_reset_keyboard_protocol() { printf '\e[<99u' }
precmd_functions+=(__ccmux_reset_keyboard_protocol)
`

var proxyFiles = map[string]string{
	".zshenv":   `[[ -f "$HOME/.zshenv" ]] && source "$HOME/.zshenv"` + "\n",
	".zprofile": `[[ -f "$HOME/.zprofile" ]] && source "$HOME/.zprofile"` + "\n",
	".zshrc":    proxyZshrc,
	".zlogin":   `[[ -f "$HOME/.zlogin" ]] && source "$HOME/.zlogin"` + "\n",
}

// pathPrepend puts a directory at the FRONT of PATH from the proxy .zshrc.
//
// It has to happen here rather than in the pane's tmux environment: a login
// shell rebuilds PATH from the user's own config (and, on macOS, path_helper
// reorders it), so anything handed in via `new-session -e PATH=…` lands
// wherever that rebuild leaves it. Appending after the user's .zshrc has run is
// the only point where "first" is still true. Front, not back, because the shim
// must beat a real clipboard tool: a genuine xclip would write the HOST's
// clipboard, which is not the machine the lens is on.
// Single-quoted, not %q: Go quoting escapes `"` and `\` but leaves `$` and
// backticks alone, and this line is sourced by every hosted pane's shell — a `$`
// in the path would expand there. Single quotes suppress all of it.
const pathPrepend = "\nexport PATH=%s:\"$PATH\"\n"

// shQuote wraps s in single quotes, ending and reopening them around any single
// quote it contains ('\”) — the only escape a POSIX shell honours inside them.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// WriteZdotdir creates the proxy ZDOTDIR at dir with all four dotfiles. A
// non-empty binDir is prepended to PATH for panes using this ZDOTDIR.
func WriteZdotdir(dir, binDir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	files := proxyFiles
	if binDir != "" {
		files = make(map[string]string, len(proxyFiles))
		for k, v := range proxyFiles {
			files[k] = v
		}
		files[".zshrc"] = proxyZshrc + fmt.Sprintf(pathPrepend, shQuote(binDir))
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// Cleanup removes a pane's ZDOTDIR and command file.
func Cleanup(paneID string) {
	os.RemoveAll(ZdotdirPath(paneID))
	os.Remove(CmdFilePath(paneID))
}
