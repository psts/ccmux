package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
)

// fakeResolver stands in for tailscale whois so tests never shell out.
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

func req(remoteAddr string, headers map[string]string, query string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/attach"+query, nil)
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestResolveIdentity_TrustsServeHeadersFromLoopback(t *testing.T) {
	// Behind `tailscale serve` the request is loopback and carries injected
	// identity headers → trusted as the verified login.
	s := newIdentityServer(fakeResolver{ok: false})
	id := s.resolveIdentity(req("127.0.0.1:5000",
		map[string]string{"Tailscale-User-Login": "patric@example.com", "Tailscale-User-Name": "Patric Sandelin"},
		"?user=spoofed"))
	if id.Login != "patric@example.com" || id.Email != "patric@example.com" || !id.Verified {
		t.Fatalf("serve-header identity = %+v; want verified patric@example.com", id)
	}
	if id.Display != "Patric Sandelin" {
		t.Errorf("display = %q, want the tailnet name", id.Display)
	}
}

func TestResolveIdentity_IgnoresSpoofedHeadersFromNonLoopback(t *testing.T) {
	// A DIRECT tailnet client (non-loopback) must NOT be able to set the identity
	// header itself — those headers are only trusted from the serve proxy. Here
	// whois fails, so it falls back to the self-declared name, NOT the header.
	s := newIdentityServer(fakeResolver{ok: false})
	id := s.resolveIdentity(req("100.64.0.9:5000",
		map[string]string{"Tailscale-User-Login": "attacker@evil.com"},
		"?user=bob"))
	if id.Verified {
		t.Fatalf("must not verify a spoofed header from a direct client: %+v", id)
	}
	if id.Login != "bob" {
		t.Errorf("login = %q, want the self-declared bob (header ignored)", id.Login)
	}
}

func TestResolveIdentity_WhoisForDirectTailnetClient(t *testing.T) {
	s := newIdentityServer(fakeResolver{login: "carol@example.com", display: "Carol", ok: true})
	id := s.resolveIdentity(req("100.64.0.9:5000", nil, "?user=ignored"))
	if id.Login != "carol@example.com" || id.Email != "carol@example.com" || !id.Verified || id.Display != "Carol" {
		t.Fatalf("whois identity = %+v; want verified Carol", id)
	}
}

func TestResolveIdentity_SelfDeclaredFallback(t *testing.T) {
	s := newIdentityServer(fakeResolver{ok: false})
	id := s.resolveIdentity(req("100.64.0.9:5000", nil, "?user=dave"))
	if id.Verified || id.Email != "" || id.Login != "dave" {
		t.Fatalf("fallback identity = %+v; want unverified dave, no email", id)
	}
	// No user param → anon.
	if got := s.resolveIdentity(req("100.64.0.9:5000", nil, "")); got.Login != "anon" {
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
