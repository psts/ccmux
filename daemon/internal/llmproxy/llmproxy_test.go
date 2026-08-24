package llmproxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeStore map[string]string

func (f fakeStore) GetSetting(key string) (string, error) { return f[key], nil }
func (f fakeStore) SetSetting(key, value string) error    { f[key] = value; return nil }

// brokenStore fails every read — the registry is there but unreadable.
type brokenStore struct{}

func (brokenStore) GetSetting(string) (string, error) { return "", errors.New("disk io error") }
func (brokenStore) SetSetting(string, string) error   { return nil }

// mount wires the service handler the way the api server does: pane-scoped
// wildcard patterns on a 1.22 mux.
func mount(s *Service) *httptest.Server {
	mux := http.NewServeMux()
	h := s.Handler()
	for _, m := range []string{"GET", "POST", "PUT", "DELETE"} {
		mux.Handle(m+" /llm/pane/{pane}/{rest...}", h)
	}
	return httptest.NewServer(mux)
}

type seen struct {
	path, auth, apiKey, host string
}

func upstream(t *testing.T, got *seen) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = seen{path: r.URL.Path, auth: r.Header.Get("Authorization"), apiKey: r.Header.Get("x-api-key"), host: r.Host}
		w.WriteHeader(200)
	}))
}

func call(t *testing.T, proxyURL, path, bearer string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", proxyURL+path, strings.NewReader("{}"))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// No route configured: pure pass-through to the default upstream, path prefix
// stripped, the client's own bearer (the Max OAuth case) untouched.
func TestDefaultRouteIsPassthrough(t *testing.T) {
	var got seen
	up := upstream(t, &got)
	defer up.Close()
	s := New(fakeStore{})
	s.defaultUpstream = up.URL
	p := mount(s)
	defer p.Close()

	resp := call(t, p.URL, "/llm/pane/p1/v1/messages", "oauth-token")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got.path != "/v1/messages" {
		t.Fatalf("upstream path = %q, want pane prefix stripped", got.path)
	}
	if got.auth != "Bearer oauth-token" || got.apiKey != "" {
		t.Fatalf("auth = %q/%q, want the client's bearer untouched", got.auth, got.apiKey)
	}
}

// A routed anthropic-kind account with a key replaces the client's auth with
// x-api-key, so panes never hold provider secrets.
func TestKeyedAnthropicAccountInjectsKey(t *testing.T) {
	var got seen
	up := upstream(t, &got)
	defer up.Close()
	s := New(fakeStore{})
	accs := []Account{{Name: "work", Kind: "anthropic", BaseURL: up.URL, APIKey: "sk-real"}}
	route := "work"
	if msg := s.Reject(&accs, &route); msg != "" {
		t.Fatalf("reject: %s", msg)
	}
	if err := s.Apply(&accs, &route); err != nil {
		t.Fatal(err)
	}
	p := mount(s)
	defer p.Close()

	call(t, p.URL, "/llm/pane/p1/v1/messages", "client-oauth")
	if got.apiKey != "sk-real" {
		t.Fatalf("x-api-key = %q, want the account key", got.apiKey)
	}
	if got.auth != "" {
		t.Fatalf("Authorization = %q, want the client's auth stripped", got.auth)
	}
}

// An openai-kind account (OpenRouter, Ollama's OpenAI face) carries its key
// as a bearer instead.
func TestOpenAIKindUsesBearer(t *testing.T) {
	var got seen
	up := upstream(t, &got)
	defer up.Close()
	s := New(fakeStore{})
	accs := []Account{{Name: "openrouter", Kind: "openai", BaseURL: up.URL, APIKey: "or-key"}}
	route := "openrouter"
	_ = s.Apply(&accs, &route)
	p := mount(s)
	defer p.Close()

	call(t, p.URL, "/llm/pane/p1/v1/chat/completions", "client-oauth")
	if got.auth != "Bearer or-key" || got.apiKey != "" {
		t.Fatalf("auth = %q/%q, want the account bearer only", got.auth, got.apiKey)
	}
}

// A keyless routed account (local Ollama) is also a pass-through — Ollama
// ignores the forwarded bearer.
func TestKeylessRoutedAccountPassesAuthThrough(t *testing.T) {
	var got seen
	up := upstream(t, &got)
	defer up.Close()
	s := New(fakeStore{})
	accs := []Account{{Name: "ollama", BaseURL: up.URL}}
	route := "ollama"
	_ = s.Apply(&accs, &route)
	p := mount(s)
	defer p.Close()

	call(t, p.URL, "/llm/pane/p1/v1/messages", "client-oauth")
	if got.auth != "Bearer client-oauth" {
		t.Fatalf("auth = %q, want pass-through", got.auth)
	}
}

// A route naming a vanished account fails loudly, never falls back somewhere
// that silently burns tokens.
func TestDanglingRouteIs502(t *testing.T) {
	s := New(fakeStore{settingRoute: "gone"})
	p := mount(s)
	defer p.Close()
	resp := call(t, p.URL, "/llm/pane/p1/v1/messages", "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestRejectValidation(t *testing.T) {
	s := New(fakeStore{})
	cases := []struct {
		accs  []Account
		route string
		want  string // substring of the rejection, "" = accepted
	}{
		{[]Account{{Name: "a", BaseURL: "http://192.168.1.10:11434"}}, "a", ""},
		{[]Account{{Name: "", BaseURL: "http://127.0.0.1"}}, "", "empty name"},
		{[]Account{{Name: "a", BaseURL: "http://127.0.0.1"}, {Name: "a", BaseURL: "http://10.0.0.2"}}, "", "duplicate"},
		{[]Account{{Name: "a", BaseURL: "not a url"}}, "", "http(s)"},
		{[]Account{{Name: "a", Kind: "weird", BaseURL: "http://127.0.0.1"}}, "", "unknown kind"},
		{[]Account{{Name: "a", BaseURL: "http://127.0.0.1"}}, "missing", "names no llm account"},
		// Keyless = the pane's own login is forwarded; a public host must not
		// be able to receive it, keyed accounts can go anywhere.
		{[]Account{{Name: "a", BaseURL: "https://attacker.example"}}, "", "forwarded"},
		{[]Account{{Name: "a", BaseURL: "http://8.8.8.8"}}, "", "forwarded"},
		{[]Account{{Name: "a", BaseURL: "https://api.anthropic.com"}}, "", ""},
		{[]Account{{Name: "a", BaseURL: "http://localhost:11434"}}, "", ""},
		{[]Account{{Name: "a", BaseURL: "http://100.99.1.2"}}, "", ""}, // tailnet
		{[]Account{{Name: "a", Kind: "openai", BaseURL: "https://openrouter.ai/api", APIKey: "k"}}, "", ""},
	}
	for i, c := range cases {
		msg := s.Reject(&c.accs, &c.route)
		if c.want == "" && msg != "" {
			t.Errorf("case %d rejected: %s", i, msg)
		}
		if c.want != "" && !strings.Contains(msg, c.want) {
			t.Errorf("case %d = %q, want %q", i, msg, c.want)
		}
	}
}

// Removing the routed account in the same request must be refused: the
// proposal is validated as the state it produces.
func TestRejectOrphanedRoute(t *testing.T) {
	s := New(fakeStore{})
	accs := []Account{{Name: "work", BaseURL: "http://127.0.0.1"}}
	route := "work"
	_ = s.Apply(&accs, &route)
	empty := []Account{}
	if msg := s.Reject(&empty, nil); !strings.Contains(msg, "names no llm account") {
		t.Fatalf("reject = %q, want orphaned-route refusal", msg)
	}
}

// A keyed account resubmitted from the redacted view (empty apiKey) inherits
// its stored key, so validation must not mistake it for a keyless
// pass-through to a public host.
func TestRejectSeesInheritedKeys(t *testing.T) {
	s := New(fakeStore{})
	accs := []Account{{Name: "or", Kind: "openai", BaseURL: "https://openrouter.ai/api", APIKey: "k"}}
	_ = s.Apply(&accs, nil)
	resub := []Account{{Name: "or", Kind: "openai", BaseURL: "https://openrouter.ai/api"}}
	if msg := s.Reject(&resub, nil); msg != "" {
		t.Fatalf("redacted resubmit rejected: %s", msg)
	}
}

// Unreadable is not empty: a failing store must refuse loudly everywhere —
// no silent default route, no blank settings page, no write that would
// clobber stored keys against a phantom empty list.
func TestBrokenStoreRefusesEverywhere(t *testing.T) {
	s := New(brokenStore{})
	p := mount(s)
	defer p.Close()
	if resp := call(t, p.URL, "/llm/pane/p1/v1/messages", ""); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("proxy on broken store = %d, want 502", resp.StatusCode)
	}
	if _, _, err := s.Snapshot(); err == nil {
		t.Fatal("Snapshot on broken store returned no error")
	}
	accs := []Account{}
	if err := s.Apply(&accs, nil); err == nil {
		t.Fatal("Apply on broken store wrote against a phantom empty list")
	}
	if msg := s.Reject(&accs, nil); msg == "" {
		t.Fatal("Reject on broken store accepted the change")
	}
}

// Re-applying an account with an empty key keeps the stored key — the settings
// UI round-trips the redacted list, and that must not wipe secrets.
func TestApplyKeepsKeyOnEmptyResubmit(t *testing.T) {
	s := New(fakeStore{})
	accs := []Account{{Name: "work", BaseURL: "http://x", APIKey: "sk-real"}}
	_ = s.Apply(&accs, nil)
	resub := []Account{{Name: "work", BaseURL: "http://x"}}
	_ = s.Apply(&resub, nil)
	accs, err := s.Accounts()
	if err != nil {
		t.Fatal(err)
	}
	if got := accs[0].APIKey; got != "sk-real" {
		t.Fatalf("key after redacted resubmit = %q, want kept", got)
	}
	red, _, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !red[0].APIKeySet {
		t.Fatal("redacted view should report the key as set")
	}
}
