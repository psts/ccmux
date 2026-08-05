#!/bin/sh
# ccmux daemon installer (the "courier").
#
# Downloads the ccmuxd + ccmux-peers binaries for this OS/arch from GitHub
# Releases, verifies their checksum, drops them on PATH, then hands off to
# `ccmuxd install` (the binary's own installer) to write + start the user
# service and prompt for anything it needs.
#
#   curl -fsSL https://raw.githubusercontent.com/psts/ccmux/main/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --hostname boxb --authkey tskey-...
#
# Everything after `sh -s --` is passed straight through to `ccmuxd install`
# (see `ccmuxd install -h`). Prompts read /dev/tty, so they still work when the
# script itself arrives on stdin through the pipe.
#
# Environment overrides:
#   CCMUX_REPO=owner/repo   override the GitHub repo hosting the releases
#   CCMUX_BIN=/path         install dir for the binaries (default ~/.local/bin)
#   CCMUX_VERSION=vX.Y.Z    release to fetch (default: latest)
set -eu

REPO="${CCMUX_REPO:-psts/ccmux}"
BINDIR="${CCMUX_BIN:-$HOME/.local/bin}"
VERSION="${CCMUX_VERSION:-latest}"

die() { echo "install: $*" >&2; exit 1; }

# --- detect platform -------------------------------------------------------
os=$(uname -s)
case "$os" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) die "unsupported OS: $os (need Darwin or Linux)" ;;
esac

arch=$(uname -m)
case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "unsupported arch: $arch" ;;
esac

# macOS builds are Apple Silicon only (matches the app's arm64-only build).
if [ "$os" = darwin ] && [ "$arch" != arm64 ]; then
	die "macOS builds are Apple Silicon (arm64) only; got $arch"
fi

asset="ccmux_${os}_${arch}.tar.gz"
if [ "$VERSION" = latest ]; then
	# Resolve the alias to its concrete tag BEFORE downloading. Two reasons:
	# the user sees which version they are actually getting, and download +
	# checksums are pinned to one release — the latest/download alias is
	# resolved per file, so a release publishing mid-install could otherwise
	# serve the old version consistently (or worse, mixed files).
	loc=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest") \
		|| die "cannot resolve the latest release of $REPO"
	VERSION="${loc##*/}"
	case "$VERSION" in
		v[0-9]*) echo "install: latest release is $VERSION" ;;
		*) die "could not resolve latest release tag (redirect landed on $loc)" ;;
	esac
fi
base="https://github.com/$REPO/releases/download/$VERSION"

# --- tools -----------------------------------------------------------------
command -v curl >/dev/null 2>&1 || die "curl is required"
command -v tar >/dev/null 2>&1 || die "tar is required"
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
	die "need sha256sum or shasum to verify the download"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

# --- download + verify -----------------------------------------------------
echo "install: fetching $asset ($VERSION) from $REPO ..."
curl -fsSL "$base/$asset" -o "$tmp/$asset" || die "download failed: $base/$asset"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" || die "checksums download failed: $base/checksums.txt"

want=$(awk -v f="$asset" '$2 == f {print $1}' "$tmp/checksums.txt")
[ -n "$want" ] || die "no checksum listed for $asset"
got=$(sha256 "$tmp/$asset")
[ "$want" = "$got" ] || die "checksum mismatch for $asset (want $want, got $got)"
echo "install: checksum ok"

# --- unpack + place --------------------------------------------------------
tar -xzf "$tmp/$asset" -C "$tmp"
mkdir -p "$BINDIR"
for b in ccmuxd ccmux-peers; do
	[ -f "$tmp/$b" ] || die "$b missing from $asset"
	if ! install -m 0755 "$tmp/$b" "$BINDIR/$b" 2>/dev/null; then
		cp "$tmp/$b" "$BINDIR/$b" && chmod 0755 "$BINDIR/$b"
	fi
done
echo "install: placed ccmuxd + ccmux-peers in $BINDIR"

case ":$PATH:" in
	*":$BINDIR:"*) ;;
	*) echo "install: note - $BINDIR is not on your PATH; add it so 'ccmuxd' resolves" ;;
esac

# --- hand off to the binary's own installer --------------------------------
echo "install: running ccmuxd install ..."
exec "$BINDIR/ccmuxd" install "$@"
