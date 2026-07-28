#!/bin/bash
# ccmux hook forwarder.
#
# Registered in ~/.claude/settings.json (see install-hooks.sh), alongside any other
# hooks — Claude Code runs every command registered for an event. Reads the hook's
# JSON payload on stdin and forwards a compact {type, cwd, notification_type} message
# to the listening ccmux endpoint over its Unix domain socket.
#
# Socket selection: hosted panes (spawned by the daemon) inherit CCMUX_HOOKS_SOCK
# pointing at the DAEMON's socket, so their hooks reach the daemon. Local panes
# (driven by the native app) have no CCMUX_HOOKS_SOCK and fall back to the app's
# /tmp/ccmux-hooks.sock. This keeps the two streams separate — before, both bound
# the same path and whoever bound last stole the other's hooks (hosted attention
# flash died whenever the app ran).
#
# Exits 0 fast and silently when nothing is listening, so it never blocks or fails
# a Claude turn.
#
# Usage:  ccmux-notify.sh <event-name>
#   where <event-name> is one of: notification, permission-request,
#   ask-user-question, stop, user-prompt-submit, session-start, session-end

SOCKET_PATH="${CCMUX_HOOKS_SOCK:-/tmp/ccmux-hooks.sock}"
[[ -S "$SOCKET_PATH" ]] || exit 0

# Map the CLI event arg to the canonical type string ccmux's listener expects.
case "${1:-unknown}" in
    notification|Notification)               TYPE="notification" ;;
    permission-request|PermissionRequest)    TYPE="permission_request" ;;
    ask-user-question|AskUserQuestion)       TYPE="ask_user_question" ;;
    stop|Stop)                               TYPE="stop" ;;
    user-prompt-submit|UserPromptSubmit)     TYPE="user_prompt_submit" ;;
    session-start|SessionStart)              TYPE="session_start" ;;
    session-end|SessionEnd)                  TYPE="session_end" ;;
    *)                                       TYPE="${1:-unknown}" ;;
esac

# Build the outgoing message in a single python pass: reads stdin, pulls cwd +
# notification_type, and emits properly-escaped compact JSON. Also forwards
# session_id (from the payload) and pane_id (from $CCMUX_PANE_ID, inherited from
# the tmux window the daemon spawned) so the daemon can attribute the event to an
# exact pane; these extra fields are ignored by the app's driver-mode listener.
# Bails (no output) when there's no cwd to map to a workspace.
MSG=$(TYPE="$TYPE" python3 -c "
import sys, json, os
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
cwd = d.get('cwd') or ''
if not cwd:
    sys.exit(0)
print(json.dumps({
    'type': os.environ['TYPE'],
    'cwd': cwd,
    'notification_type': d.get('notification_type') or '',
    'session_id': d.get('session_id') or '',
    'pane_id': os.environ.get('CCMUX_PANE_ID') or '',
}))
" 2>/dev/null)

[[ -z "$MSG" ]] && exit 0

printf '%s' "$MSG" | nc -U -w 1 "$SOCKET_PATH" 2>/dev/null

exit 0
