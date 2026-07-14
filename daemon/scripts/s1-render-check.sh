#!/bin/sh
# S1 visual rendering gate for the lens pivot.
#
# Runs a shell inside the managed ccmux tmux server and views it through the
# passthrough control-mode lens (the same raw-%output path SwiftTerm / xterm.js
# will use). Launch Claude Code inside, then eyeball:
#   - tables / box-drawing alignment (the failure the WezTerm spoof once fixed)
#   - 24-bit colors
#   - Shift+Enter inserts a newline in the composer (extended-keys / CSI-u)
#   - ESC cancels a turn with no perceptible lag
#   - spinner animates without artifacts
#
# TERM_PROGRAM will be "tmux" here (no WezTerm spoof) — that's the point of the test.
# Press Ctrl-] to detach and tear down.
#
# Usage: scripts/s1-render-check.sh [repo-dir]   (defaults to $PWD)
set -e
DAEMON_DIR="$(cd "$(dirname "$0")/.." && pwd)"
SOCK=ccmux-s1
REPO="${1:-$PWD}"

COLS=$(tput cols); ROWS=$(tput lines)

echo "Building lens..."
( cd "$DAEMON_DIR" && go build -o /tmp/ccmux-spike-lens ./cmd/spike-lens )

tmux -L "$SOCK" kill-server 2>/dev/null || true
tmux -L "$SOCK" -f "$DAEMON_DIR/config/tmux.conf" new-session -d -s s1 -x "$COLS" -y "$ROWS" -c "$REPO"
tmux -L "$SOCK" set-option -g window-size manual

echo "Attaching lens ($COLS x $ROWS). Launch Claude with:  claude"
echo "Detach with Ctrl-]"
sleep 1

STTY_SAVED=$(stty -g)
stty raw -echo
/tmp/ccmux-spike-lens -L "$SOCK" -t s1 -cols "$COLS" -rows "$ROWS" || true
stty "$STTY_SAVED"

tmux -L "$SOCK" kill-server 2>/dev/null || true
echo "torn down."
