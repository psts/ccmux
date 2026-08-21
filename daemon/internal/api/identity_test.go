package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
)

// fakeResolver stands in for the tsnet WhoIs backend so tests never touch the
// network or shell out.
type fakeResolver struct {
	login, display string
	ok             bool
}

func (f fakeResolver) Resolve(string) (string, string, bool) { return f.login, f.display, f.ok }

// newIdentityServer builds a server over a real (temp) registry: identity
// resolution reads the alias map out of settings, so a store-less manager isn't
// enough to exercise it.
func newIdentityServer(t *testing.T, res whoisResolver) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := NewServer(manager.New(context.Background(), nil, st))
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
	s := newIdentityServer(t, fakeResolver{login: "carol@example.com", display: "Carol", ok: true})
	id := s.resolveIdentity(req("100.64.0.9:5000", "?user=ignored"))
	if id.Login != "carol@example.com" || id.Email != "carol@example.com" || !id.Verified || id.Display != "Carol" {
		t.Fatalf("whois identity = %+v; want verified Carol", id)
	}
}

func TestResolveIdentity_DisplayFallsBackToLogin(t *testing.T) {
	// A verified user with no display name shows their login as the display.
	s := newIdentityServer(t, fakeResolver{login: "dave@example.com", ok: true})
	if id := s.resolveIdentity(req("100.64.0.9:5000", "")); id.Display != "dave@example.com" {
		t.Errorf("display = %q, want the login as fallback", id.Display)
	}
}

func TestResolveIdentity_SelfDeclaredFallback(t *testing.T) {
	// Loopback (hooks) or an unresolvable peer → unverified self-declared name.
	s := newIdentityServer(t, fakeResolver{ok: false})
	id := s.resolveIdentity(req("127.0.0.1:5000", "?user=dave"))
	if id.Verified || id.Email != "" || id.Login != "dave" {
		t.Fatalf("fallback identity = %+v; want unverified dave, no email", id)
	}
	if got := s.resolveIdentity(req("127.0.0.1:5000", "")); got.Login != "anon" {
		t.Errorf("missing user → login %q, want anon", got.Login)
	}
}

// The owner tier: a host that knows its human treats any caller WhoIs cannot
// name as that human. This is the loopback case — the person at this machine's
// keyboard is exactly who WhoIs declines to identify.
func TestResolveIdentity_OwnerClaimsUnverifiedCallers(t *testing.T) {
	s := newIdentityServer(t, fakeResolver{ok: false})
	if err := s.mgr.SetOwner("sandelin@example.com"); err != nil {
		t.Fatal(err)
	}

	id := s.resolveIdentity(req("127.0.0.1:5000", "?user=Patric%20Sandelin"))
	if id.Login != "sandelin@example.com" {
		t.Errorf("login = %q, want the host owner", id.Login)
	}
	if id.Display != "Patric Sandelin" {
		t.Errorf("display = %q, want the declared name kept", id.Display)
	}
	if id.Verified || id.Email != "" {
		t.Errorf("owner tier must not look verified: %+v", id)
	}

	// Nothing declared at all (hooks): the owner is the best name we have.
	if got := s.resolveIdentity(req("127.0.0.1:5000", "")); got.Login != "sandelin@example.com" || got.Display != "sandelin@example.com" {
		t.Errorf("anon on an owned host = %+v, want the owner", got)
	}
}

// An alias is a deliberate per-name mapping; the owner is a blanket default.
// The specific rule must beat the general one, or a configured guest alias on
// an owned host would misattribute the guest to the owner.
func TestResolveIdentity_AliasBeatsOwner(t *testing.T) {
	s := newIdentityServer(t, fakeResolver{ok: false})
	if err := s.mgr.SetOwner("sandelin@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.mgr.SetIdentityAliases(map[string]string{"dasha": "dasha@example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := s.resolveIdentity(req("127.0.0.1:5000", "?user=dasha")).Login; got != "dasha@example.com" {
		t.Errorf("login = %q, want the alias, not the owner", got)
	}
}

// The owner tier is bounded to the machine's own keyboard: a NON-loopback
// caller WhoIs cannot name (a tagged member daemon, most importantly) must not
// resolve to the host's human — a hub proxying to a member would otherwise
// authenticate as the member's owner and walk through its archive guard.
func TestResolveIdentity_OwnerDoesNotClaimRemoteCallers(t *testing.T) {
	s := newIdentityServer(t, fakeResolver{ok: false})
	if err := s.mgr.SetOwner("sandelin@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := s.resolveIdentity(req("100.64.0.7:5000", "?user=machine")).Login; got != "machine" {
		t.Errorf("remote unnameable caller = %q, want its self-declared name, not the owner", got)
	}
}

// A verified tailnet caller is never re-attributed to the host owner.
func TestResolveIdentity_OwnerNeverOverridesVerified(t *testing.T) {
	s := newIdentityServer(t, fakeResolver{login: "carol@example.com", ok: true})
	if err := s.mgr.SetOwner("sandelin@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := s.resolveIdentity(req("100.64.0.9:5000", "")).Login; got != "carol@example.com" {
		t.Errorf("login = %q, want the verified caller", got)
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

// The bug this exists for: the Mac app reaches the daemon over loopback, so WhoIs
// declines and it is known by NSFullUserName(); the same person's phone arrives
// over the tailnet and is known by its verified login. Push suppression compares
// those two strings, so without an alias a dev sitting at the Mac still gets buzzed.
func TestResolveIdentity_AliasCollapsesSelfDeclaredNameOntoVerifiedLogin(t *testing.T) {
	s := newIdentityServer(t, fakeResolver{ok: false})
	if err := s.mgr.SetIdentityAliases(map[string]string{"Patric Sandelin": "sandelin@example.com"}); err != nil {
		t.Fatal(err)
	}

	id := s.resolveIdentity(req("127.0.0.1:5000", "?user=Patric%20Sandelin"))

	if id.Login != "sandelin@example.com" {
		t.Errorf("login = %q, want the aliased login so suppression matches the phone's subscription", id.Login)
	}
	if id.Display != "Patric Sandelin" {
		t.Errorf("display = %q, want the declared name — the alias changes the key, not what presence shows", id.Display)
	}
	if id.Verified {
		t.Error("an alias must not make an unverified caller look verified")
	}
	if id.Email != "" {
		t.Error("an alias must not populate the git-attribution email, which means WhoIs vouched for it")
	}
}

// Names come from a macOS account or a browser prompt. Neither is a stable
// identifier, so capitalisation must not decide whether your phone buzzes.
func TestResolveIdentity_AliasIgnoresCaseAndSpace(t *testing.T) {
	s := newIdentityServer(t, fakeResolver{ok: false})
	if err := s.mgr.SetIdentityAliases(map[string]string{"  Patric Sandelin  ": "sandelin@example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, declared := range []string{"patric sandelin", "PATRIC SANDELIN", "Patric Sandelin"} {
		if got := s.resolveIdentity(req("127.0.0.1:5000", "?user="+url.QueryEscape(declared))).Login; got != "sandelin@example.com" {
			t.Errorf("user=%q resolved to %q, want the aliased login", declared, got)
		}
	}
}

// A verified login is already the canonical key. Rewriting it would let a
// self-declared alias hijack somebody else's verified identity.
func TestResolveIdentity_AliasNeverRewritesAVerifiedLogin(t *testing.T) {
	s := newIdentityServer(t, fakeResolver{login: "carol@example.com", display: "Carol", ok: true})
	if err := s.mgr.SetIdentityAliases(map[string]string{"carol@example.com": "attacker@example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := s.resolveIdentity(req("100.64.0.9:5000", "")).Login; got != "carol@example.com" {
		t.Errorf("verified login rewritten to %q", got)
	}
}

func TestResolveIdentity_UnaliasedNamePassesThrough(t *testing.T) {
	s := newIdentityServer(t, fakeResolver{ok: false})
	if err := s.mgr.SetIdentityAliases(map[string]string{"someone else": "else@example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := s.resolveIdentity(req("127.0.0.1:5000", "?user=dave")).Login; got != "dave" {
		t.Errorf("login = %q, want the declared name untouched", got)
	}
}
