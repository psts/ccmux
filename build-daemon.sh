#!/bin/bash
set -e

# Rebuild the ccmux daemon and restart it under launchd. Hosted tmux sessions
# survive the bounce — the daemon re-adopts them on start.

LABEL="com.ccmux.ccmuxd"
OUT="$HOME/bin/ccmuxd"
LOG="$HOME/Library/Logs/ccmuxd.log"

cd "$(dirname "$0")/daemon"

echo "Building ccmuxd..."
go build -o "$OUT" ./cmd/ccmuxd

echo "Building ccmux-peers..."
# The peers MCP binary isn't launchd-managed; new Claude sessions pick up the
# fresh build on their own. Existing sessions keep the old one until they recycle.
go build -o "$HOME/bin/ccmux-peers" ./cmd/ccmux-peers

echo "Restarting $LABEL..."
# -k kills the running instance before relaunching. The agent is loaded in the
# GUI domain for this user, so target gui/<uid>/<label>.
launchctl kickstart -k "gui/$(id -u)/$LABEL"

echo ""
echo "✅ Rebuilt: $OUT"
echo "✅ Rebuilt: $HOME/bin/ccmux-peers"
echo ""
echo "Logs:    tail -f $LOG"
echo "Health:  curl -s http://127.0.0.1:7900/v1/settings | jq ."
