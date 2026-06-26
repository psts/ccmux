#!/bin/bash
# Idempotent installer for ccmux's Claude Code hooks.
#
# Adds `ccmux-notify.sh <event>` to the relevant hook arrays in ~/.claude/settings.json.
# Claude Code runs every command registered for an event, so this coexists with any
# other hooks already there (e.g. claude-deck). Safe to run repeatedly — it never
# duplicates an entry and never touches commands it didn't add.
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NOTIFY="$SCRIPT_DIR/ccmux-notify.sh"
SETTINGS="$HOME/.claude/settings.json"

chmod +x "$NOTIFY"

NOTIFY="$NOTIFY" SETTINGS="$SETTINGS" python3 <<'PY'
import json, os, sys, shutil

notify = os.environ["NOTIFY"]
settings_path = os.environ["SETTINGS"]

# (event name, matcher, ccmux-notify.sh arg)
EVENTS = [
    ("Notification",      "",                "notification"),
    ("PermissionRequest", "*",               "permission-request"),
    ("PreToolUse",        "AskUserQuestion",  "ask-user-question"),
    ("Stop",              "",                "stop"),
    ("UserPromptSubmit",  "",                "user-prompt-submit"),
    ("SessionEnd",        "",                "session-end"),
]

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

with open(settings_path, "w") as f:
    json.dump(settings, f, indent=2)
    f.write("\n")

print(f"ccmux hooks: {added} added, {len(EVENTS) - added} already present → {settings_path}")
PY

echo "Done. Restart any running 'claude' sessions for the hooks to take effect."
