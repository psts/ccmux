#!/bin/bash
# Opt-in, per-repo installer for ccmux's git co-author trailer.
#
# Writes a prepare-commit-msg hook into the target repo that delegates to
# ccmux-coauthor.sh, so a hosted commit is credited to the current session driver.
# Idempotent; refuses to clobber an existing hook (prints the one line to add).
#
# Usage:  install-git-coauthor.sh [repo-path]   (defaults to the current repo)
set -e

REPO="${1:-$PWD}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOOK="$SCRIPT_DIR/ccmux-coauthor.sh"
chmod +x "$HOOK"

GITDIR="$(cd "$REPO" && git rev-parse --git-dir 2>/dev/null)" || {
    echo "not a git repository: $REPO" >&2
    exit 1
}
# git rev-parse --git-dir may be relative to the repo; resolve it.
GITDIR="$(cd "$REPO" && cd "$GITDIR" && pwd)"
DEST="$GITDIR/hooks/prepare-commit-msg"

if [[ -e "$DEST" ]]; then
    if grep -qF "$HOOK" "$DEST"; then
        echo "already installed → $DEST"
        exit 0
    fi
    echo "A prepare-commit-msg hook already exists at:" >&2
    echo "  $DEST" >&2
    echo "Add this line to it to enable ccmux co-author attribution:" >&2
    echo "  \"$HOOK\" \"\$@\"" >&2
    exit 1
fi

mkdir -p "$(dirname "$DEST")"
cat >"$DEST" <<EOF
#!/bin/sh
# ccmux co-author trailer (installed by install-git-coauthor.sh)
exec "$HOOK" "\$@"
EOF
chmod +x "$DEST"
echo "installed ccmux co-author hook → $DEST"
