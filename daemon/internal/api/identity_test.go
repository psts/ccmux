package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
)

// fakeResolver stands in for the tsnet WhoIs backend so tests never touch the
// network or shell out.
type fakeResolver struct {
	login, display string
	ok             bool
}

func (f fakeResolver) Resolve(string) (string, string, bool) { return f.login, f.display, f.ok }

func newIdentityServer(res whoisResolver) *Server {
	s := NewServer(manager.New(context.Background(), nil, nil))
	s.identity = res
	return s
}

func req(remoteAddr, query string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/attach"+query, nil)
	r.RemoteAddr = remoteAddr
	return r
}

func TestResolveIdentity_VerifiedFromWhois(t *testing.T) {
	// A lens over the tailnet resolves to its verified login; the self-declared
	// ?user= is ignored in favor of the verified identity.
	s := newIdentityServer(fakeResolver{login: "carol@example.com", display: "Carol", ok: true})
	id := s.resolveIdentity(req("100.64.0.9:5000", "?user=ignored"))
	if id.Login != "carol@example.com" || id.Email != "carol@example.com" || !id.Verified || id.Display != "Carol" {
		t.Fatalf("whois identity = %+v; want verified Carol", id)
	}
}

func TestResolveIdentity_DisplayFallsBackToLogin(t *testing.T) {
	// A verified user with no display name shows their login as the display.
	s := newIdentityServer(fakeResolver{login: "dave@example.com", ok: true})
	if id := s.resolveIdentity(req("100.64.0.9:5000", "")); id.Display != "dave@example.com" {
		t.Errorf("display = %q, want the login as fallback", id.Display)
	}
}

func TestResolveIdentity_SelfDeclaredFallback(t *testing.T) {
	// Loopback (hooks) or an unresolvable peer → unverified self-declared name.
	s := newIdentityServer(fakeResolver{ok: false})
	id := s.resolveIdentity(req("127.0.0.1:5000", "?user=dave"))
	if id.Verified || id.Email != "" || id.Login != "dave" {
		t.Fatalf("fallback identity = %+v; want unverified dave, no email", id)
	}
	if got := s.resolveIdentity(req("127.0.0.1:5000", "")); got.Login != "anon" {
		t.Errorf("missing user → login %q, want anon", got.Login)
	}
}

func TestValidPushEndpoint(t *testing.T) {
	ok := []string{
		"https://fcm.googleapis.com/fcm/send/abc",
		"https://updates.push.services.mozilla.com/wpush/v2/xyz",
		"https://web.push.apple.com/QABC",
	}
	for _, e := range ok {
		if err := validPushEndpoint(e); err != nil {
			t.Errorf("validPushEndpoint(%q) = %v, want nil", e, err)
		}
	}
	bad := []string{
		"http://fcm.googleapis.com/x", // not https
		"https://localhost/x",         // loopback name
		"https://127.0.0.1/x",         // loopback ip
		"https://10.0.0.5/x",          // private
		"https://192.168.1.9/x",       // private
		"https://169.254.1.1/x",       // link-local
		"https://[::1]/x",             // ipv6 loopback
		"ftp://example.com/x",         // wrong scheme
		"https:///nohost",             // no host
		"::://bogus",                  // unparseable
	}
	for _, e := range bad {
		if err := validPushEndpoint(e); err == nil {
			t.Errorf("validPushEndpoint(%q) = nil, want rejected", e)
		}
	}
}
