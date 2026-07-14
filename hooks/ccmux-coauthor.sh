#!/bin/bash
# ccmux prepare-commit-msg hook.
#
# On a shared/hosted checkout every pane commits as the same neutral station
# identity, so the real human is invisible. This hook attributes the commit to the
# person currently *driving* this pane's ccmux session by appending a
# Co-Authored-By trailer — the committer stays the station account, the driver gets
# credit.
#
# Installed per-repo (opt-in) by install-git-coauthor.sh. Reads:
#   CCMUX_PANE_ID    — set by the daemon in every hosted pane (required)
#   CCMUX_DAEMON_URL — daemon base URL, also injected by the daemon
#                      (falls back to http://127.0.0.1:7890)
#
# No-ops silently whenever anything is missing or nobody is driving, so it never
# blocks or fails a commit.
#
# Args (git prepare-commit-msg): $1 = commit message file, $2 = source.

MSG_FILE="$1"
SOURCE="$2" # message | template | merge | squash | commit | <empty>

# Only touch human-authored commits; leave merges/squashes/amends alone.
case "$SOURCE" in
    merge | squash | commit) exit 0 ;;
esac

[[ -n "$MSG_FILE" && -n "$CCMUX_PANE_ID" ]] || exit 0
command -v curl >/dev/null 2>&1 || exit 0
command -v python3 >/dev/null 2>&1 || exit 0

URL="${CCMUX_DAEMON_URL:-http://127.0.0.1:7890}/v1/panes/$CCMUX_PANE_ID/driver"
RESP=$(curl -sf -m 2 "$URL" 2>/dev/null) || exit 0 # 204/404/unreachable → no trailer
[[ -n "$RESP" ]] || exit 0

TRAILER=$(RESP="$RESP" python3 -c '
import os, json, sys
try:
    d = json.loads(os.environ["RESP"])
except Exception:
    sys.exit(0)
user = (d.get("user") or "").strip()
if not user:
    sys.exit(0)
email = (d.get("email") or "").strip()
if not email:
    email = user.lower().replace(" ", "-") + "@ccmux.local"
print(f"Co-Authored-By: {user} <{email}>")
' 2>/dev/null)

[[ -n "$TRAILER" ]] || exit 0

# Idempotent: skip if this exact trailer is already present.
grep -qiF "$TRAILER" "$MSG_FILE" 2>/dev/null && exit 0

# Append as a trailer block (blank line before it when the file has content).
if [[ -s "$MSG_FILE" ]]; then
    printf '\n%s\n' "$TRAILER" >>"$MSG_FILE"
else
    printf '%s\n' "$TRAILER" >>"$MSG_FILE"
fi
exit 0
