#!/bin/bash
# ccmux hook forwarder and tracer.
#
# Registered in ~/.claude/settings.json (see install-hooks.sh), alongside any other
# hooks — Claude Code runs every command registered for an event. Reads the hook's
# JSON payload on stdin and does two things:
#
#   1. Appends one line to the trace (~/Library/Logs/ccmux-hooks.jsonl) for EVERY
#      invocation, before any filtering. This is the ground truth for "which hook
#      fired": it records the events ccmux drops as readily as the ones it acts on,
#      so an unexplained notification can be traced back to the hook behind it. The
#      daemon and the native app append their own decisions to the same file.
#   2. Forwards a compact message to the listening ccmux endpoint over its Unix
#      domain socket — but only for the events ccmux actually routes to attention.
#      Everything else is traced and stops here.
#
# Socket selection: hosted panes (spawned by the daemon) inherit CCMUX_HOOKS_SOCK
# pointing at the DAEMON's socket, so their hooks reach the daemon. Local panes
# (driven by the native app) have no CCMUX_HOOKS_SOCK and fall back to the app's
# /tmp/ccmux-hooks.sock. This keeps the two streams separate — before, both bound
# the same path and whoever bound last stole the other's hooks (hosted attention
# flash died whenever the app ran).
#
# Exits 0 fast and silently whatever happens, so it never blocks or fails a Claude
# turn. A trace that can't be written is not an error worth surfacing here.
#
# Usage:  ccmux-notify.sh <event-name>
#   Routed events:     notification, permission-request, ask-user-question, stop,
#                      user-prompt-submit, session-start, session-end
#   Trace-only events: subagent-start, subagent-stop, task-completed,
#                      teammate-idle, permission-denied, stop-failure, elicitation

# This hook is registered in ~/.claude/settings.json, which is user-level, so it
# runs for EVERY Claude session on the machine — including ones started in a plain
# terminal that ccmux knows nothing about. Those have no CCMUX_PANE_ID.
#
# Leave immediately for them. ccmux notifies about work it owns, and a session
# outside a ccmux pane has no pane to flash and no workspace to name. Worse, the
# daemon would not simply ignore it: ResolvePane falls back to the pane whose CWD
# is the longest prefix of the hook's, so a terminal Claude sitting anywhere
# inside a repo that also has a hosted pane would raise an alert naming THAT pane.
# Silence is the correct answer, and reaching it before the python below also
# keeps the hook near-free for every non-ccmux session.
#
# BOTH variables have to be checked. Daemon-hosted panes get CCMUX_PANE_ID; the
# Mac app's own local panes get CCMUX_CMD_FILE instead and never see a pane id
# (TerminalStore builds their env). Testing only the pane id silently mutes every
# local pane, which is indistinguishable from a foreign terminal without this.
[[ -n "${CCMUX_PANE_ID:-}" || -n "${CCMUX_CMD_FILE:-}" ]] || exit 0

SOCKET_PATH="${CCMUX_HOOKS_SOCK:-/tmp/ccmux-hooks.sock}"
TRACE_PATH="${CCMUX_HOOK_TRACE:-$HOME/Library/Logs/ccmux-hooks.jsonl}"

# Map the CLI event arg to the canonical type string ccmux's listener expects.
# ROUTED=0 means "trace it, don't send it": the event is registered purely so it
# shows up in the log next to the notification it might explain (a subagent
# finishing, an idle reminder), and adding it here must not change what ccmux
# does with attention.
ROUTED=1
case "${1:-unknown}" in
    notification|Notification)               TYPE="notification" ;;
    permission-request|PermissionRequest)    TYPE="permission_request" ;;
    ask-user-question|AskUserQuestion)       TYPE="ask_user_question" ;;
    stop|Stop)                               TYPE="stop" ;;
    user-prompt-submit|UserPromptSubmit)     TYPE="user_prompt_submit" ;;
    session-start|SessionStart)              TYPE="session_start" ;;
    session-end|SessionEnd)                  TYPE="session_end" ;;

    subagent-start|SubagentStart)            TYPE="subagent_start";    ROUTED=0 ;;
    subagent-stop|SubagentStop)              TYPE="subagent_stop";     ROUTED=0 ;;
    task-completed|TaskCompleted)            TYPE="task_completed";    ROUTED=0 ;;
    teammate-idle|TeammateIdle)              TYPE="teammate_idle";     ROUTED=0 ;;
    permission-denied|PermissionDenied)      TYPE="permission_denied"; ROUTED=0 ;;
    stop-failure|StopFailure)                TYPE="stop_failure";      ROUTED=0 ;;
    elicitation|Elicitation)                 TYPE="elicitation";       ROUTED=0 ;;

    *)                                       TYPE="${1:-unknown}";     ROUTED=0 ;;
esac

SOCK_OK=0
[[ -S "$SOCKET_PATH" ]] && SOCK_OK=1

# One python pass does everything that needs the payload: reads stdin, appends the
# trace line, and prints the outgoing socket message (or nothing). Doing it in a
# single pass means stdin is consumed once — a hook's payload is not replayable.
#
# The message carries session_id (from the payload), pane_id (from $CCMUX_PANE_ID,
# inherited from the tmux window the daemon spawned) and trace_id, so the daemon
# can attribute the event to an exact pane and tie its own trace lines back to this
# one. Extra fields are ignored by the app's driver-mode listener.
MSG=$(TYPE="$TYPE" ROUTED="$ROUTED" SOCK_OK="$SOCK_OK" TRACE_PATH="$TRACE_PATH" \
      RAW_EVENT="${1:-unknown}" SOCKET_PATH="$SOCKET_PATH" python3 -c '
import sys, json, os, datetime

MAX_TRACE_BYTES = 8 << 20  # matches internal/hooktrace: a tail buffer, not history

def trace(line):
    """Append one JSON object. Never raises — a trace must not break a turn."""
    path = os.environ["TRACE_PATH"]
    try:
        os.makedirs(os.path.dirname(path), exist_ok=True)
        # O_APPEND + one write() keeps lines whole when the daemon, the app and
        # several concurrent hooks all append to this file at once.
        flags = os.O_CREAT | os.O_WRONLY | os.O_APPEND
        fd = os.open(path, flags, 0o644)
        try:
            if os.fstat(fd).st_size > MAX_TRACE_BYTES:
                os.ftruncate(fd, 0)
            os.write(fd, (json.dumps(line) + "\n").encode("utf-8"))
        finally:
            os.close(fd)
    except Exception:
        pass

def clip(v, n=160):
    s = str(v or "")
    s = " ".join(s.split())
    return s if len(s) <= n else s[: n - 1] + "…"

try:
    payload = json.load(sys.stdin)
except Exception:
    payload = {}

trace_id = os.urandom(4).hex()
cwd = payload.get("cwd") or ""
routed = os.environ["ROUTED"] == "1"
sock_ok = os.environ["SOCK_OK"] == "1"

# Why this hook did or did not become a ccmux message. Read the log for these.
if not routed:
    decision = "trace-only"
elif not cwd:
    decision = "no-cwd"        # nothing to map to a workspace
elif not sock_ok:
    decision = "no-listener"   # neither the daemon nor the app is bound
else:
    decision = "routed"

line = {
    # Local time with offset, same shape as the daemon writes (RFC 3339), so the
    # file sorts and reads correctly with lines from all four writers mixed in.
    "ts": datetime.datetime.now().astimezone().isoformat(),
    "stage": "hook",
    "trace_id": trace_id,
    "event": os.environ["TYPE"],
    "hook_event_name": payload.get("hook_event_name") or os.environ["RAW_EVENT"],
    "decision": decision,
    "cwd": cwd,
    "session_id": payload.get("session_id") or "",
    "pane_id": os.environ.get("CCMUX_PANE_ID") or "",
    "socket": os.environ["SOCKET_PATH"],
}

# Payload fields that distinguish otherwise identical events. agent_id/agent_type
# say whether a Stop came from a subagent; notification_type + message say whether
# a Notification is a permission prompt or an idle reminder; stop_reason says why
# a turn ended. These are the fields the log exists to expose.
for key in ("notification_type", "agent_id", "agent_type", "stop_reason",
            "tool_name", "permission_mode", "end_reason", "source"):
    if payload.get(key):
        line[key] = clip(payload[key], 80)
if payload.get("message"):
    line["message"] = clip(payload["message"])

# Everything else the payload carried, so the log can show a field nobody thought
# to whitelist. This is not defensive padding: the documented stop_reason turned
# out never to be sent, and the question of whether a Stop means "finished" or
# "stopped talking while background agents run" can only be answered by a field
# we are not yet looking for. Scalars only, clipped — nested objects and the
# transcript path are bulk with no discriminating value.
SKIP = {"cwd", "session_id", "hook_event_name", "transcript_path", "message"}
for key, value in sorted(payload.items()):
    if key in line or key in SKIP:
        continue
    if isinstance(value, bool) or isinstance(value, (int, float)) or isinstance(value, str):
        line[key] = clip(value, 80)

trace(line)

if decision != "routed":
    sys.exit(0)

print(json.dumps({
    "type": os.environ["TYPE"],
    "cwd": cwd,
    "notification_type": payload.get("notification_type") or "",
    "session_id": payload.get("session_id") or "",
    "pane_id": os.environ.get("CCMUX_PANE_ID") or "",
    "trace_id": trace_id,
}))
' 2>/dev/null)

[[ -z "$MSG" ]] && exit 0

printf '%s' "$MSG" | nc -U -w 1 "$SOCKET_PATH" 2>/dev/null

exit 0
