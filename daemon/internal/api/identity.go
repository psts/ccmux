package api

import "net/http"

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

// resolveIdentity determines who is calling. When the daemon runs as its own
// tsnet node, s.identity is backed by that node's in-process WhoIs, so a lens
// connecting over the tailnet resolves to its verified login; the on-host loopback
// listener (hooks) resolves to nothing and falls back to the self-declared ?user=
// name. This is the single identity source for attach presence, git attribution,
// and push subscription/suppression keying, so all three agree on who a caller is.
func (s *Server) resolveIdentity(r *http.Request) identity {
	if login, display, ok := s.identity.Resolve(r.RemoteAddr); ok {
		return identity{Login: login, Display: orDefault(display, login), Email: login, Verified: true}
	}
	u := orDefault(r.URL.Query().Get("user"), "anon")
	return identity{Login: u, Display: u, Verified: false}
}
