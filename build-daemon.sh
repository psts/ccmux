#!/bin/bash
set -e

# Build ccmuxd + ccmux-peers from this checkout and go live: binaries land in
# the same place the installer puts them (~/.local/bin), and the user service
# is restarted the OS's way. Hosted tmux sessions survive the bounce; running
# Claude sessions keep their old ccmux-peers until they restart.
#
# What restarts: ccmuxd, always. The LLM proxy is its own service on Linux
# (ccmux-llm-proxy, the same ccmuxd binary run as `ccmuxd llm-proxy`), and it
# restarts ONLY when the proxy's own code changed, so a deploy does not cut
# the replies sessions are streaming through it. This script does not decide
# that: it stamps the binary with a hash of the proxy's code, and the daemon
# it starts compares that with what the running service reports and restarts
# the service on a difference (cmd/ccmuxd/llmproxyunit.go). Below, the script
# only waits to see the service answer with this build's hash.
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
# The hash of the LLM proxy's own code (see scripts/llm-proxy-hash.sh for what
# goes into it). Same code, same hash, whatever else in the daemon changed.
PROXY_HASH="$(bash scripts/llm-proxy-hash.sh)"
echo "Building ccmuxd + ccmux-peers ($BUILD, llm proxy code $PROXY_HASH) → $BINDIR"
mkdir -p "$BINDIR"

# install_built NAME PACKAGE LDFLAGS: build beside the target, then rename
# over it. The old binary is still running when this happens, as the proxy
# service and as every session's runner, long after the daemon itself has
# restarted. A rename gives the name a new file and leaves the running one
# its own (the kernel keeps a replaced file for whoever has it open); writing
# into the file in place is what must never happen, and a rename cannot.
install_built() {
	go build -ldflags "$3" -o "$BINDIR/.$1.new" "$2"
	mv -f "$BINDIR/.$1.new" "$BINDIR/$1"
}
install_built ccmuxd ./cmd/ccmuxd "-X ccmux.dev/ccmuxd/internal/version.Build=$BUILD -X ccmux.dev/ccmuxd/internal/llmserve.codeHash=$PROXY_HASH"
install_built ccmux-peers ./cmd/ccmux-peers ""

echo "Restarting ccmuxd..."
case "$(uname -s)" in
	Darwin) launchctl kickstart -k "gui/$(id -u)/com.ccmux.ccmuxd" ;;
	Linux) systemctl --user restart ccmuxd ;;
	*) echo "unknown OS $(uname -s) — restart the service yourself" ;;
esac

echo "Health:"
# Up to 15s: a -tsnet -hub start can take a few seconds to bind. A daemon that
# never answers is a FAILED deploy, not a pending one: /ship runs this script
# as its deploy step (.claude/ship-after-release) and only a non-zero exit
# stops it from reporting a release that is not running.
HEALTH=""
for _ in $(seq 15); do
	HEALTH=$(curl -sf --max-time 2 http://127.0.0.1:7900/v1/health) && break
	sleep 1
done
if [ -z "$HEALTH" ]; then
	echo "ccmuxd did not answer /v1/health within 15s of the restart — see: journalctl --user -u ccmuxd" >&2
	exit 1
fi
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

# The LLM proxy. Panes cannot reach their models without it, so a proxy that
# is not serving this build's proxy code is a FAILED deploy too. The daemon
# says where the proxy runs; when that is its own service, wait for it: the
# daemon restarts it in the background when its code changed, and the old
# one is given up to 10s to finish what it is streaming.
case "$HEALTH" in
*'"mode":"service"'*)
	PROXY_URL=$(printf '%s' "$HEALTH" | sed -n 's/.*"url":"\([^"]*\)".*/\1/p')
	if [ -z "$PROXY_URL" ]; then
		echo "the daemon runs the LLM proxy as a service but its health names no address for it:" >&2
		echo "$HEALTH" >&2
		exit 1
	fi
	PROXY=""
	for _ in $(seq 40); do
		PROXY=$(curl -sf --max-time 2 "$PROXY_URL/health") || PROXY=""
		case "$PROXY" in *"\"codeHash\":\"$PROXY_HASH\""*) break ;; esac
		PROXY=""
		sleep 1
	done
	if [ -z "$PROXY" ]; then
		echo "the LLM proxy at $PROXY_URL is not serving this build's proxy code ($PROXY_HASH) after 40s." >&2
		echo "Panes cannot reach their models. See: journalctl --user -u ccmux-llm-proxy, and journalctl --user -u ccmuxd | grep 'llm proxy'" >&2
		curl -s --max-time 2 http://127.0.0.1:7900/v1/health >&2 || true
		exit 1
	fi
	# What the daemon did to it, in its own words ("left running: ..." or
	# "restarted: ..."). It records that a moment after the service answers.
	sleep 1
	ACTION=$(curl -sf --max-time 2 http://127.0.0.1:7900/v1/health | sed -n 's/.*"action":"\([^"]*\)".*/\1/p') || ACTION=""
	echo "LLM proxy ($PROXY_URL, code $PROXY_HASH): ${ACTION:-serving}"
	;;
*'"mode":"in-process"'*)
	# Normal on macOS. On Linux it means the service is switched off or was
	# refused (its unit belongs to another daemon, say), and every deploy cuts
	# the replies being streamed: say why, in the daemon's words.
	WHY=$(printf '%s' "$HEALTH" | sed -n 's/.*"why":"\([^"]*\)".*/\1/p')
	echo "LLM proxy: runs INSIDE the daemon, so it restarted with it and replies being streamed were cut."
	echo "  why: ${WHY:-the daemon did not say}"
	;;
esac
