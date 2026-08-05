#!/bin/bash
set -e

# Build ccmuxd + ccmux-peers from this checkout and go live: binaries land in
# the same place the installer puts them (~/.local/bin), and the user service
# is restarted the OS's way. Hosted tmux sessions survive the bounce; running
# Claude sessions keep their old ccmux-peers until they restart.
#
# The build is stamped with `git describe`, so `ccmuxd version` and /v1/health
# tell a source build from a release. Heads up: `ccmuxd upgrade` compares that
# stamp against the latest release and would happily "upgrade" a source build
# back to the release binaries — on a machine that goes live from source, this
# script IS the upgrade path.

BINDIR="${CCMUX_BIN:-$HOME/.local/bin}"
# Strip the tag's leading v to match goreleaser's stamp ({{ .Version }}): a
# source build of exactly a release tag then reads identically to the release,
# and `ccmuxd upgrade` correctly reports it as already up to date.
BUILD="$(git -C "$(dirname "$0")" describe --tags --dirty --always 2>/dev/null || echo dev)"
BUILD="${BUILD#v}"

cd "$(dirname "$0")/daemon"
echo "Building ccmuxd + ccmux-peers ($BUILD) → $BINDIR"
mkdir -p "$BINDIR"
go build -ldflags "-X ccmux.dev/ccmuxd/internal/version.Build=$BUILD" -o "$BINDIR/ccmuxd" ./cmd/ccmuxd
go build -o "$BINDIR/ccmux-peers" ./cmd/ccmux-peers

echo "Restarting ccmuxd..."
case "$(uname -s)" in
	Darwin) launchctl kickstart -k "gui/$(id -u)/com.ccmux.ccmuxd" ;;
	Linux) systemctl --user restart ccmuxd ;;
	*) echo "unknown OS $(uname -s) — restart the service yourself" ;;
esac

echo "Health:"
sleep 1
HEALTH=$(curl -sf --max-time 5 http://127.0.0.1:7900/v1/health) || { echo "(no healthy response yet — check the service logs)"; exit 0; }
echo "$HEALTH"
# The build only went live if the RUNNING daemon reports the version we just
# built — a service still pointing at another binary path (e.g. a pre-break
# ~/bin install) restarts the OLD build and everything above was a no-op.
LIVE=$(printf '%s' "$HEALTH" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
if [ "$LIVE" != "$BUILD" ]; then
	echo "WARNING: the running service reports $LIVE, not $BUILD." >&2
	echo "Its service file points at a different binary — run 'ccmuxd install' once to repoint it at $BINDIR." >&2
	exit 1
fi
