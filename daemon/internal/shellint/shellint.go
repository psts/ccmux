// Package shellint reproduces ccmux's invisible zsh integration for hosted
// panes: a temporary ZDOTDIR of proxy dotfiles that source the user's real
// config, then install a preexec hook capturing the typed command line (aliases
// included) to $CCMUX_CMD_FILE. Ported from TerminalStore.setupZdotdir. The
// integration must never inject visible commands into the terminal (project
// rule) — everything happens via startup files.
package shellint

import (
	"os"
	"path/filepath"
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

// WriteZdotdir creates the proxy ZDOTDIR at dir with all four dotfiles.
func WriteZdotdir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for name, content := range proxyFiles {
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
