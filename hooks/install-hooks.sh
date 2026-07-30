#!/bin/bash
# Idempotent installer for ccmux's Claude Code hooks.
#
# Adds `ccmux-notify.sh <event>` to the relevant hook arrays in ~/.claude/settings.json.
# Claude Code runs every command registered for an event, so this coexists with any
# other hooks already there (e.g. claude-deck). Safe to run repeatedly — it never
# duplicates an entry and never touches commands it didn't add.
#
# Two kinds of registration go in:
#
#   ROUTED events drive ccmux attention (a pane flashes, a phone buzzes).
#   TRACE-ONLY events drive nothing. They are registered so that the hook trace
#   (~/Library/Logs/ccmux-hooks.jsonl) shows them beside the notifications they
#   might explain — a subagent finishing, an idle reminder, a permission denial.
#   ccmux-notify.sh logs them and stops; they never reach ccmux's socket.
#
# Run with --routed-only to skip the trace-only events once you've finished
# debugging; it prunes ones already installed.
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NOTIFY="$SCRIPT_DIR/ccmux-notify.sh"
# Overridable so the installer can be exercised against a scratch file instead of
# the settings.json you actually run Claude with.
SETTINGS="${CCMUX_CLAUDE_SETTINGS:-$HOME/.claude/settings.json}"

chmod +x "$NOTIFY"

ROUTED_ONLY=""
[[ "${1:-}" == "--routed-only" ]] && ROUTED_ONLY=1

NOTIFY="$NOTIFY" SETTINGS="$SETTINGS" ROUTED_ONLY="$ROUTED_ONLY" python3 <<'PY'
import json, os, sys, shutil

notify = os.environ["NOTIFY"]
settings_path = os.environ["SETTINGS"]
routed_only = bool(os.environ.get("ROUTED_ONLY"))

# (event name, matcher, ccmux-notify.sh arg) — these drive ccmux attention.
ROUTED = [
    ("Notification",      "",                "notification"),
    ("PermissionRequest", "*",               "permission-request"),
    ("PreToolUse",        "AskUserQuestion",  "ask-user-question"),
    ("Stop",              "",                "stop"),
    ("UserPromptSubmit",  "",                "user-prompt-submit"),
    ("SessionStart",      "",                "session-start"),
    ("SessionEnd",        "",                "session-end"),
    # These two set no attention themselves. They are routed so the daemon can
    # count a session's live background agents and tell a Stop that means
    # "finished" from one that means "stopped talking while agents still run".
    ("SubagentStart",     "",                "subagent-start"),
    ("SubagentStop",      "",                "subagent-stop"),
]

# Logged, never acted on. Chosen because each one can fire close enough to a
# notification to be mistaken for its cause. Deliberately excluded: PostToolUse,
# PostToolBatch, MessageDisplay and the file/config watchers — they fire on a
# cadence that would bury the events you're actually reading the log for.
TRACE_ONLY = [
    ("TaskCompleted",    "",  "task-completed"),
    ("TeammateIdle",     "",  "teammate-idle"),
    ("PermissionDenied", "*", "permission-denied"),
    ("StopFailure",      "",  "stop-failure"),
    ("Elicitation",      "",  "elicitation"),
]

EVENTS = ROUTED if routed_only else ROUTED + TRACE_ONLY

if os.path.exists(settings_path):
    with open(settings_path) as f:
        settings = json.load(f)
    shutil.copy(settings_path, settings_path + ".bak")
else:
    os.makedirs(os.path.dirname(settings_path), exist_ok=True)
    settings = {}

hooks = settings.setdefault("hooks", {})
added = 0

for event, matcher, arg in EVENTS:
    command = f"{notify} {arg}"
    groups = hooks.setdefault(event, [])
    # Find the matcher group (treat "" and "*" as the catch-all for this event).
    group = next((g for g in groups if g.get("matcher", "") == matcher), None)
    if group is None:
        group = {"matcher": matcher, "hooks": []}
        groups.append(group)
    entries = group.setdefault("hooks", [])
    if any(h.get("command") == command for h in entries):
        continue  # already installed
    entries.append({"type": "command", "command": command})
    added += 1

# --routed-only also takes the trace-only registrations back out, so turning the
# extra logging off is the same one command that turned it on. Only entries whose
# command is exactly ours are removed; anything else in the group is left alone,
# and a group emptied of everything is dropped rather than left as a husk.
removed = 0
if routed_only:
    for event, matcher, arg in TRACE_ONLY:
        command = f"{notify} {arg}"
        groups = hooks.get(event)
        if not groups:
            continue
        for group in list(groups):
            entries = group.get("hooks", [])
            kept = [h for h in entries if h.get("command") != command]
            removed += len(entries) - len(kept)
            group["hooks"] = kept
            if not kept:
                groups.remove(group)
        if not groups:
            del hooks[event]

with open(settings_path, "w") as f:
    json.dump(settings, f, indent=2)
    f.write("\n")

present = len(EVENTS) - added
print(f"ccmux hooks: {added} added, {present} already present → {settings_path}")
if routed_only:
    print(f"trace-only hooks: {removed} removed (attention hooks untouched)")
else:
    print(f"  {len(ROUTED)} routed (drive attention), {len(TRACE_ONLY)} trace-only (logged, never acted on)")
PY

echo "Done. Restart any running 'claude' sessions for the hooks to take effect."
echo "Trace:   tail -f \"\${CCMUX_HOOK_TRACE:-\$HOME/Library/Logs/ccmux-hooks.jsonl}\""
echo "Viewer:  $SCRIPT_DIR/tail-hooks.sh"
