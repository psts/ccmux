package llmproxy

import (
	"net/http"
	"strings"
	"testing"
)

// memStore is the two-method Store the proxy needs, in memory.
type memStore map[string]string

func (m memStore) GetSetting(k string) (string, error) { return m[k], nil }
func (m memStore) SetSetting(k, v string) error        { m[k] = v; return nil }

func TestMeridianKindValidation(t *testing.T) {
	cases := []struct {
		name string
		acc  Account
		want string // substring of the refusal, "" = accepted
	}{
		{"needs token", Account{Name: "m", Kind: KindMeridian, BaseURL: MeridianUpstream}, "setup token"},
		{"loopback ok", Account{Name: "m", Kind: KindMeridian, BaseURL: "http://127.0.0.1:3456", APIKey: "tok"}, ""},
		{"localhost ok", Account{Name: "m", Kind: KindMeridian, BaseURL: "http://localhost:4000", APIKey: "tok"}, ""},
		{"lan refused", Account{Name: "m", Kind: KindMeridian, BaseURL: "http://192.168.1.5:3456", APIKey: "tok"}, "127.0.0.1"},
		{"https refused", Account{Name: "m", Kind: KindMeridian, BaseURL: "https://127.0.0.1:3456", APIKey: "tok"}, "127.0.0.1"},
		{"unknown lists meridian", Account{Name: "m", Kind: "weird", BaseURL: MeridianUpstream}, "or meridian"},
		{"port required", Account{Name: "m", Kind: KindMeridian, BaseURL: "http://localhost", APIKey: "tok"}, "port included"},
	}
	for _, c := range cases {
		got := validateAccounts([]Account{c.acc})
		if c.want == "" && got != "" {
			t.Errorf("%s: refused: %s", c.name, got)
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want it to mention %q", c.name, got, c.want)
		}
	}
}

func TestTwoMeridianAccountsNeedDistinctPorts(t *testing.T) {
	same := []Account{
		{Name: "a", Kind: KindMeridian, BaseURL: "http://127.0.0.1:3456", APIKey: "t1"},
		{Name: "b", Kind: KindMeridian, BaseURL: "http://127.0.0.1:3456", APIKey: "t2"},
	}
	if msg := validateAccounts(same); !strings.Contains(msg, "own port") {
		t.Fatalf("same port should be refused, got %q", msg)
	}
	same[1].BaseURL = "http://localhost:3456" // same socket, other spelling
	if msg := validateAccounts(same); !strings.Contains(msg, "own port") {
		t.Fatalf("localhost and 127.0.0.1 on one port must clash, got %q", msg)
	}
	same[1].BaseURL = "http://127.0.0.1:3457"
	if msg := validateAccounts(same); msg != "" {
		t.Fatalf("distinct ports should be accepted, got %q", msg)
	}
	// The defaulting path must not create the clash silently either.
	two := merged(nil, []Account{{Name: "a", Kind: KindMeridian, APIKey: "t1"}, {Name: "b", Kind: KindMeridian, APIKey: "t2"}})
	if msg := validateAccounts(two); !strings.Contains(msg, "own port") {
		t.Fatalf("two defaulted meridian accounts share %s and must be refused, got %q", MeridianUpstream, msg)
	}
}

func TestMeridianDefaultsURLAndAllowsGlobalRoute(t *testing.T) {
	got := merged(nil, []Account{{Name: "m", Kind: KindMeridian, APIKey: "tok"}})
	if got[0].BaseURL != MeridianUpstream {
		t.Fatalf("empty URL should default to %s, got %q", MeridianUpstream, got[0].BaseURL)
	}
	// A meridian default was refused while every pane followed the global
	// route; a harness pane now routes through its own declared kinds, and
	// kindAllowed still keeps meridian away from harnesses that never asked
	// for it. So the setting itself is no longer the thing to refuse.
	svc := New(memStore{})
	accs := []Account{{Name: "m", Kind: KindMeridian, APIKey: "tok"}}
	for _, route := range []string{"m", ""} {
		if msg := svc.Reject(&accs, &route); msg != "" {
			t.Fatalf("meridian route %q should be accepted, got %q", route, msg)
		}
	}
	if msg := svc.Reject(&accs, ptr("nope")); !strings.Contains(msg, "names no llm account") {
		t.Fatalf("a default naming nothing should still be refused, got %q", msg)
	}
}

func ptr(s string) *string { return &s }

func TestMeridianAuthIsPlaceholderNotToken(t *testing.T) {
	req, _ := http.NewRequest("POST", "http://x/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer pane-login")
	req.Header.Set("x-api-key", "pane-key")
	applyAuth(req, Account{Kind: KindMeridian, APIKey: "sk-setup-token"})
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("bearer should be stripped, got %q", got)
	}
	if got := req.Header.Get("x-api-key"); got != "ccmux" {
		t.Errorf("x-api-key should be the placeholder, got %q", got)
	}
	if needsSystemTurnCompat(Account{Kind: KindMeridian, BaseURL: MeridianUpstream}) {
		t.Error("meridian speaks full Anthropic dialect; no system-turn downgrade")
	}
	if !needsSystemTurnCompat(Account{Kind: "openai", BaseURL: "http://127.0.0.1:11434"}) {
		t.Error("other loopback upstreams keep the downgrade")
	}
}
