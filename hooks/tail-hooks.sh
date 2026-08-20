#!/bin/bash
# Readable view of the ccmux hook trace.
#
# The trace (~/Library/Logs/ccmux-hooks.jsonl) is written by four processes:
# ccmux-notify.sh logs every hook Claude Code fires, the daemon logs where each one
# routed and which phones it pushed, and the native app logs the macOS alerts it
# posted or suppressed. This prints them as one line each, in order, so a
# notification can be read back to the hook that caused it.
#
# Usage:
#   tail-hooks.sh                follow live (the usual case)
#   tail-hooks.sh -n 200         print the last 200 lines and follow
#   tail-hooks.sh --no-follow    print and exit
#   tail-hooks.sh --alerts-only  hide the hooks that can never alert you
#
# Reading it: a hook line and the route/push/local lines under it share a trace_id
# (shown as the last column) wherever the id survives the hop. Push lines carry no
# id — the daemon's push notifier sees a workspace and an attention state, not a
# hook — so correlate those by the timestamp and workspace directly above them.
set -u

TRACE_PATH="${CCMUX_HOOK_TRACE:-$HOME/Library/Logs/ccmux-hooks.jsonl}"
LINES=40
FOLLOW=1
FILTER=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        # ${2:-} rather than $2: set -u turns a bare `-n` at the end of the line
        # into an unbound-variable death instead of a usage message.
        -n)          LINES="${2:-}"
                     [[ "$LINES" =~ ^[0-9]+$ ]] || { echo "-n needs a number of lines" >&2; exit 2; }
                     shift 2 ;;
        --no-follow) FOLLOW=0; shift ;;
        --alerts-only) FILTER="alerts"; shift ;;
        -h|--help)   sed -n '2,19{s/^# \{0,1\}//;p;}' "$0"; exit 0 ;;
        *)           echo "unknown option: $1" >&2; exit 2 ;;
    esac
done

if [[ ! -f "$TRACE_PATH" ]]; then
    echo "No trace yet at $TRACE_PATH" >&2
    echo "Install the hooks first: $(dirname "$0")/install-hooks.sh" >&2
    exit 1
fi

format() {
    FILTER="$FILTER" python3 -u -c '
import sys, json, os

FILTER = os.environ.get("FILTER") or ""

# One colour per writer, so the three-or-four lines that make up one hooks story
# are visually grouped without having to read the stage column.
COLOR = {
    "hook":  "\033[36m",  # cyan   — Claude Code fired something
    "route": "\033[35m",  # purple — the daemon decided what it meant
    "push":  "\033[33m",  # yellow — a phone was or was not buzzed
    "local": "\033[32m",  # green  — the Mac alert
}
# The decisions worth spotting at a glance: something reached a human, or a
# fault did. QUIET is the routine half — deliberate drops that are working as
# intended. The three subagent faults are bold because each one means the hold
# quietly stopped working, and a silent hold looks exactly like a healthy one.
LOUD = {"posted", "sent", "routed",
        "agent-unattributed", "hold-unchecked", "agent-expired"}
QUIET = {"suppressed", "ignored", "trace-only", "no-push", "unresolved",
         "session-unresolved", "no-cwd", "no-listener", "held", "agent-unknown"}
RESET, BOLD, DIM = "\033[0m", "\033[1m", "\033[2m"

# --alerts-only drops the hooks that are pure lifecycle bookkeeping. Everything
# else stays, including the subagent pair and permission_denied: they alert
# nobody themselves, but they are the events most likely to be sitting next to
# the notification you are trying to explain — and a subagent_start is now the
# reason an idle reminder never became one ("held").
NEVER_ALERTS = {"user_prompt_submit", "session_start", "session_end"}

def detail_for(stage, d):
    """The one thing you need to know about this line, per stage."""
    bits = []
    if stage == "hook":
        for k in ("notification_type", "agent_type", "agent_id", "stop_reason", "tool_name"):
            if d.get(k):
                bits.append("%s=%s" % (k, d[k]))
        if d.get("message"):
            bits.append("%s" % d["message"])
    elif stage == "route":
        if d.get("attention"):     bits.append("attention=" + d["attention"])
        if d.get("session_signal"): bits.append("session=" + d["session_signal"])
        if d.get("resolved_pane"):  bits.append("pane=" + d["resolved_pane"][:8])
    elif stage == "push":
        if d.get("login"):        bits.append(d["login"])
        if d.get("attention"):    bits.append("attention=" + d["attention"])
        if d.get("workspace_id"): bits.append("ws=" + d["workspace_id"][:8])
    elif stage == "local":
        if d.get("attention"):    bits.append("attention=" + d["attention"])
        if d.get("workspace_id"): bits.append("ws=" + d["workspace_id"][:8])
    if d.get("detail"):
        bits.append(d["detail"])
    return "  ".join(bits)

for raw in sys.stdin:
    raw = raw.strip()
    if not raw:
        continue
    try:
        d = json.loads(raw)
    except Exception:
        continue

    stage = d.get("stage", "?")
    event = d.get("event") or d.get("hook_event_name") or ""
    decision = d.get("decision", "")

    if FILTER == "alerts" and event in NEVER_ALERTS:
        continue

    ts = (d.get("ts") or "")[11:23]  # HH:MM:SS.mmm
    color = COLOR.get(stage, "")
    mark = BOLD if decision in LOUD else (DIM if decision in QUIET else "")

    print("%s%-12s %s%-6s%s %-22s %s%-18s%s %s%-8s%s %s%s%s" % (
        DIM, ts, color, stage, RESET,
        event[:22],
        mark, decision, RESET,
        DIM, d.get("trace_id") or "-", RESET,
        DIM, detail_for(stage, d), RESET,
    ))
'
}

if [[ "$FOLLOW" == "1" ]]; then
    tail -n "$LINES" -F "$TRACE_PATH" | format
else
    tail -n "$LINES" "$TRACE_PATH" | format
fi
