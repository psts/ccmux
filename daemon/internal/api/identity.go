package api

import (
	"net"
	"net/http"
)

// identity is a resolved caller identity. Login is the key subscriptions and
// push-suppression match on (the verified tailnet login/email when known, else
// the self-declared name). Email is the verified email only — empty when
// unverified — and is used for git attribution (the co-author trailer).
type identity struct {
	Login    string
	Display  string
	Email    string
	Verified bool
}

// resolveIdentity determines who is calling, handling both daemon deployments:
//
//   - Behind `tailscale serve` (the runbook's setup) the request arrives from
//     loopback and Tailscale injects Tailscale-User-Login/-Name headers. We trust
//     them ONLY for loopback connections — a direct tailnet client must not be
//     able to spoof them by setting the header itself.
//   - Bound directly on the tailnet, we whois the peer IP (Phase 3 path).
//   - Otherwise (LAN, unknown) we fall back to the self-declared ?user= name,
//     unverified.
//
// This is the single identity source for attach presence, git attribution, and
// push subscription/suppression keying, so all three agree on who a caller is.
func (s *Server) resolveIdentity(r *http.Request) identity {
	if isLoopbackAddr(r.RemoteAddr) {
		if login := r.Header.Get("Tailscale-User-Login"); login != "" {
			name := r.Header.Get("Tailscale-User-Name")
			return identity{Login: login, Display: orDefault(name, login), Email: login, Verified: true}
		}
	} else if login, display, ok := s.identity.Resolve(r.RemoteAddr); ok {
		return identity{Login: login, Display: orDefault(display, login), Email: login, Verified: true}
	}
	u := orDefault(r.URL.Query().Get("user"), "anon")
	return identity{Login: u, Display: u, Verified: false}
}

func isLoopbackAddr(remoteAddr string) bool {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
