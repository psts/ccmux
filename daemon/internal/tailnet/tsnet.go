package tailnet

import (
	"context"
	"time"

	"tailscale.com/client/tailscale/apitype"
)

// WhoIser resolves a tailnet peer's identity from its remote address. A tsnet
// server's *local.Client satisfies it; it is the in-process replacement for the
// `tailscale whois` CLI once the daemon runs as its own tailnet node (so identity
// no longer depends on the host's tailscaled or on `tailscale serve` headers).
type WhoIser interface {
	WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)
}

// LocalResolver resolves identity via a tsnet LocalClient's WhoIs. It satisfies
// the same Resolve(remoteAddr) (login, display, ok) contract as the CLI Resolver,
// so the API layer is agnostic to which backs it.
type LocalResolver struct {
	who WhoIser
}

// NewLocalResolver returns a resolver backed by a tsnet LocalClient.
func NewLocalResolver(w WhoIser) *LocalResolver { return &LocalResolver{who: w} }

// Resolve returns the verified login (email) and display name for a connection's
// remote address. ok is false for loopback (the on-host hooks listener has no
// tailnet identity) or when identity cannot be determined.
func (r *LocalResolver) Resolve(remoteAddr string) (login, display string, ok bool) {
	if isLocal(hostOf(remoteAddr)) {
		return "", "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	who, err := r.who.WhoIs(ctx, remoteAddr)
	if err != nil || who == nil || who.UserProfile == nil || who.UserProfile.LoginName == "" {
		return "", "", false
	}
	// A tagged node is a machine, never a person (see taggedDevicesLogin): the
	// fleet's own daemons carry tag:ccmux, and their calls must not resolve to
	// the synthetic tagged-devices user as if WhoIs had vouched for a human.
	if (who.Node != nil && len(who.Node.Tags) > 0) || who.UserProfile.LoginName == taggedDevicesLogin {
		return "", "", false
	}
	return who.UserProfile.LoginName, who.UserProfile.DisplayName, true
}
