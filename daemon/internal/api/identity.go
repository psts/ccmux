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
	// Vouched: the Login is trustworthy enough to KEY decisions on — WhoIs
	// said so (Verified), or the host's owner setting did (the loopback tier).
	// The self-declared ?user= path is never vouched. Weaker than Verified,
	// which additionally gates git attribution.
	Vouched bool
}

// resolveIdentity determines who is calling. When the daemon runs as its own
// tsnet node, s.identity is backed by that node's in-process WhoIs, so a lens
// connecting over the tailnet resolves to its verified login; the on-host loopback
// listener (hooks) resolves to nothing and falls back to the self-declared ?user=
// name. This is the single identity source for attach presence, git attribution,
// and push subscription/suppression keying, so all three agree on who a caller is.
//
// "Agree" only held when every client reached the daemon the same way. The Mac
// app connects to 127.0.0.1, so WhoIs declines and it is known by NSFullUserName();
// a phone necessarily arrives over the tailnet and is known by its verified login.
// Push suppression compares those two strings and never matched, so a dev sitting
// at the Mac still got buzzed. A configured alias rewrites the self-declared name
// to the login it belongs to, which is what makes the two paths agree again.
//
// Display keeps the name the client declared: the alias is about who the daemon
// keys on, not about what presence shows a human.
//
// Between "verified" and "self-declared" sits the host-owner tier: a machine
// with a configured owner login treats any caller WhoIs cannot name as that
// owner. Loopback is the case that matters — the person at this machine's
// keyboard is exactly the one WhoIs declines to identify. An alias still wins
// over the owner (it is a deliberate per-name mapping; the owner is a blanket
// default), and neither ever claims Verified or an attribution Email — those
// remain WhoIs-vouched only.
func (s *Server) resolveIdentity(r *http.Request) identity {
	if login, display, ok := s.identity.Resolve(r.RemoteAddr); ok {
		return identity{Login: login, Display: orDefault(display, login), Email: login, Verified: true, Vouched: true}
	}
	u := orDefault(r.URL.Query().Get("user"), "anon")
	if login := s.mgr.ResolveAlias(u); login != u {
		return identity{Login: login, Display: u, Verified: false}
	}
	if owner := s.mgr.Owner(); owner != "" && loopback(r.RemoteAddr) {
		display := u
		if display == "anon" {
			display = owner // nothing was declared; the owner IS the best name we have
		}
		return identity{Login: owner, Display: display, Verified: false, Vouched: true}
	}
	return identity{Login: u, Display: u, Verified: false}
}

// loopback bounds the owner tier to the machine's own keyboard. Without it,
// every caller WhoIs declines would resolve to the owner — including tagged
// tailnet nodes, which the tagged-caller guard deliberately makes unnameable:
// a hub proxying to a member must not authenticate as the member's human.
func loopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
