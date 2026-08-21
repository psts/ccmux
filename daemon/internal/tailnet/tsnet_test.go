package tailnet

import (
	"context"
	"errors"
	"testing"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

type fakeWhoIser struct {
	resp *apitype.WhoIsResponse
	err  error
}

func (f fakeWhoIser) WhoIs(context.Context, string) (*apitype.WhoIsResponse, error) {
	return f.resp, f.err
}

func whois(login, display string) *apitype.WhoIsResponse {
	return &apitype.WhoIsResponse{UserProfile: &tailcfg.UserProfile{LoginName: login, DisplayName: display}}
}

func TestLocalResolver_VerifiedPeer(t *testing.T) {
	r := NewLocalResolver(fakeWhoIser{resp: whois("erin@example.com", "Erin")})
	login, display, ok := r.Resolve("100.64.0.5:41000")
	if !ok || login != "erin@example.com" || display != "Erin" {
		t.Fatalf("Resolve = (%q,%q,%v); want verified Erin", login, display, ok)
	}
}

func TestLocalResolver_LoopbackHasNoIdentity(t *testing.T) {
	// The on-host hooks listener is loopback and must never resolve to a tailnet
	// identity — WhoIs isn't even consulted.
	called := false
	r := NewLocalResolver(whoIserFunc(func() (*apitype.WhoIsResponse, error) {
		called = true
		return whois("x@y.com", "X"), nil
	}))
	if _, _, ok := r.Resolve("127.0.0.1:5000"); ok {
		t.Error("loopback resolved to an identity")
	}
	if called {
		t.Error("WhoIs was called for a loopback address")
	}
}

func TestLocalResolver_WhoisErrorOrEmpty(t *testing.T) {
	for name, w := range map[string]fakeWhoIser{
		"error":        {err: errors.New("no peer")},
		"nil response": {resp: nil},
		"nil profile":  {resp: &apitype.WhoIsResponse{}},
		"empty login":  {resp: whois("", "")},
	} {
		if _, _, ok := NewLocalResolver(w).Resolve("100.64.0.5:41000"); ok {
			t.Errorf("%s: Resolve ok=true, want false", name)
		}
	}
}

// A tagged node is a machine, never a person — same rule as the CLI resolver.
func TestLocalResolver_TaggedCallerFallsBack(t *testing.T) {
	tagged := whois("someone@example.com", "Someone")
	tagged.Node = &tailcfg.Node{Tags: []string{"tag:ccmux"}}
	for name, w := range map[string]fakeWhoIser{
		"node tags":      {resp: tagged},
		"synthetic user": {resp: whois("tagged-devices", "Tagged Devices")},
	} {
		if _, _, ok := NewLocalResolver(w).Resolve("100.64.0.5:41000"); ok {
			t.Errorf("%s: tagged caller resolved as a verified person", name)
		}
	}
}

// whoIserFunc adapts a func into a WhoIser for the loopback test.
type whoIserFunc func() (*apitype.WhoIsResponse, error)

func (f whoIserFunc) WhoIs(context.Context, string) (*apitype.WhoIsResponse, error) { return f() }
