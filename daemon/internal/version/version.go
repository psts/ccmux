// Package version carries the daemon's build identity and the wire-contract
// version the multi-host federation handshake gates on.
package version

// Contract is the /v1 wire-protocol version. Bump it ONLY on a breaking change
// to the REST+WS surface: the hub gates host compatibility on this integer, so a
// non-breaking release MUST keep it stable, and a breaking one MUST raise it.
// See daemon/docs/multihost-plan.md ("Version-skew handshake").
const Contract = 1

// Build is the human-readable build identity (e.g. a git-describe string), set at
// link time via -ldflags "-X ccmux.dev/ccmuxd/internal/version.Build=...".
// Surfaced in /v1/health and the UI; the upgrade paths read it too (a source
// build refuses a remote upgrade, and only a plain X.Y.Z upgrades itself).
var Build = "dev"

// Release is "true" only in a binary GoReleaser built for a v* tag
// (.goreleaser.yaml). Build alone cannot tell: build-daemon.sh stamps a clean
// checkout of a tag with the same "0.1.80" a release carries, on purpose. The
// daemon's self-upgrade (cmd/ccmuxd/autoupgrade.go) runs only when this is set,
// so a source build, tag or not, is never replaced underneath someone.
var Release = ""
