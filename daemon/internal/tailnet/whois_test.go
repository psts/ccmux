package tailnet

import (
	"errors"
	"testing"
)

func TestParseWhoisProfile(t *testing.T) {
	// Shape from a real `tailscale whois --json`.
	data := []byte(`{"Node":{"ID":1},"UserProfile":{"ID":42,"LoginName":"sandelin@gmail.com","DisplayName":"Patric Sandelin"}}`)
	login, display, err := parseWhoisProfile(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if login != "sandelin@gmail.com" || display != "Patric Sandelin" {
		t.Fatalf("got (%q,%q)", login, display)
	}
}

// A tagged node (the fleet's own daemons carry tag:ccmux) is a machine, never a
// person: Tailscale reports the synthetic "tagged-devices" user for it, and
// accepting that as verified would record daemon→daemon calls as a human.
func TestParseWhoisProfile_TaggedNodeIsNotAPerson(t *testing.T) {
	for name, data := range map[string]string{
		"tags on node":   `{"Node":{"ID":2,"Tags":["tag:ccmux"]},"UserProfile":{"LoginName":"someone@example.com","DisplayName":"Someone"}}`,
		"synthetic user": `{"Node":{"ID":2},"UserProfile":{"LoginName":"tagged-devices","DisplayName":"Tagged Devices"}}`,
		"tags and synth": `{"Node":{"Tags":["tag:ccmux","tag:ccmux-hub"]},"UserProfile":{"LoginName":"tagged-devices"}}`,
	} {
		if login, _, err := parseWhoisProfile([]byte(data)); err != nil || login != "" {
			t.Errorf("%s: parse = (%q, err %v), want no login", name, login, err)
		}
	}
}

func TestResolve_TaggedCallerFallsBack(t *testing.T) {
	r := NewResolver()
	r.run = func(string) ([]byte, error) {
		return []byte(`{"Node":{"Tags":["tag:ccmux"]},"UserProfile":{"LoginName":"tagged-devices"}}`), nil
	}
	if _, _, ok := r.Resolve("100.64.0.7:1"); ok {
		t.Error("tagged caller resolved as a verified person")
	}
}

func TestResolve_LoopbackIsLocal(t *testing.T) {
	r := NewResolver()
	called := false
	r.run = func(string) ([]byte, error) { called = true; return nil, nil }
	for _, addr := range []string{"127.0.0.1:5555", "[::1]:5555", "localhost:80"} {
		if _, _, ok := r.Resolve(addr); ok {
			t.Errorf("loopback %s resolved as tailnet identity", addr)
		}
	}
	if called {
		t.Error("whois should not run for loopback addresses")
	}
}

func TestResolve_TailnetIdentityAndCache(t *testing.T) {
	r := NewResolver()
	calls := 0
	r.run = func(addr string) ([]byte, error) {
		calls++
		return []byte(`{"UserProfile":{"LoginName":"bob@example.com","DisplayName":"Bob"}}`), nil
	}
	login, display, ok := r.Resolve("100.64.0.9:41234")
	if !ok || login != "bob@example.com" || display != "Bob" {
		t.Fatalf("got (%q,%q,%v)", login, display, ok)
	}
	// Second call for the same IP must hit the cache (no extra whois).
	r.Resolve("100.64.0.9:55555")
	if calls != 1 {
		t.Fatalf("expected 1 whois call (cached), got %d", calls)
	}
}

func TestResolve_ErrorFallsBack(t *testing.T) {
	r := NewResolver()
	r.run = func(string) ([]byte, error) { return nil, errors.New("peer not found") }
	if _, _, ok := r.Resolve("100.64.0.9:1"); ok {
		t.Error("whois error should yield ok=false (fall back to self-declared name)")
	}
}
