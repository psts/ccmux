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
// Informational only — surfaced in /v1/health and the UI, never gated on.
var Build = "dev"
