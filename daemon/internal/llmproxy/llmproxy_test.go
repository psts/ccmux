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
		// Codex: keyless to chatgpt.com is THE configuration (empty baseURL
		// defaults there); a key is meaningless and refused; the chatgpt.com
		// allowance is codex-only, so a Claude login can't be routed at it;
		// and codex pairing is per pane, so it can never be the global route.
		{[]Account{{Name: "cx", Kind: "codex"}}, "cx", "never global"},
		{[]Account{{Name: "cx", Kind: "codex", BaseURL: "https://chatgpt.com/backend-api"}}, "", ""},
		{[]Account{{Name: "cx", Kind: "codex", APIKey: "k"}}, "", "no key"},
		{[]Account{{Name: "a", Kind: "anthropic", BaseURL: "https://chatgpt.com/backend-api"}}, "", "forwarded"},
		{[]Account{{Name: "cx", Kind: "codex", BaseURL: "https://attacker.example"}}, "", "forwarded"},
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

// A pane override beats the global route; other panes keep following it; a
// cleared override falls back.
func TestPaneRoutePrecedence(t *testing.T) {
	var got seen
	up1 := upstream(t, &got)
	defer up1.Close()
	var got2 seen
	up2 := upstream(t, &got2)
	defer up2.Close()
	s := New(fakeStore{})
	accs := []Account{{Name: "one", BaseURL: up1.URL}, {Name: "two", BaseURL: up2.URL}}
	route := "one"
	_ = s.Apply(&accs, &route)
	if err := s.SetPaneRoute("p2", "two"); err != nil {
		t.Fatal(err)
	}
	p := mount(s)
	defer p.Close()

	call(t, p.URL, "/llm/pane/p1/v1/messages", "")
	if got.path == "" || got2.path != "" {
		t.Fatalf("pane without override went to %q/%q, want global account", got.path, got2.path)
	}
	got, got2 = seen{}, seen{}
	call(t, p.URL, "/llm/pane/p2/v1/messages", "")
	if got2.path == "" || got.path != "" {
		t.Fatal("pane override did not win over the global route")
	}
	if err := s.SetPaneRoute("p2", ""); err != nil {
		t.Fatal(err)
	}
	got, got2 = seen{}, seen{}
	call(t, p.URL, "/llm/pane/p2/v1/messages", "")
	if got.path == "" {
		t.Fatal("cleared override did not fall back to the global route")
	}
}

func TestSetPaneRouteUnknownAccount(t *testing.T) {
	s := New(fakeStore{})
	if err := s.SetPaneRoute("p1", "ghost"); !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("err = %v, want ErrUnknownAccount", err)
	}
}

// Removing an account releases its panes back to the global route instead of
// leaving a dangling override that 502s the pane.
func TestAccountRemovalPrunesPaneRoutes(t *testing.T) {
	s := New(fakeStore{})
	accs := []Account{{Name: "keep", BaseURL: "http://127.0.0.1"}, {Name: "gone", BaseURL: "http://127.0.0.2"}}
	_ = s.Apply(&accs, nil)
	_ = s.SetPaneRoute("p1", "gone")
	_ = s.SetPaneRoute("p2", "keep")
	kept := []Account{{Name: "keep", BaseURL: "http://127.0.0.1"}}
	if err := s.Apply(&kept, nil); err != nil {
		t.Fatal(err)
	}
	routes, err := s.PaneRoutes()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := routes["p1"]; ok {
		t.Fatal("route to removed account survived the removal")
	}
	if routes["p2"] != "keep" {
		t.Fatalf("unrelated pane route was dropped: %v", routes)
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

// A codex account is a pass-through: the harness's own ChatGPT bearer travels
// untouched, and an empty baseURL persists as the ChatGPT backend.
func TestCodexAccountPassesBearerThrough(t *testing.T) {
	var got seen
	up := upstream(t, &got)
	defer up.Close()
	s := New(fakeStore{})
	accs := []Account{{Name: "cx", Kind: "codex", BaseURL: up.URL}}
	if msg := s.Reject(&accs, nil); msg != "" {
		t.Fatalf("reject: %s", msg)
	}
	_ = s.Apply(&accs, nil)
	// Per pane, the way codex pairing really routes (global codex is refused).
	if err := s.SetPaneRoute("p1", "cx"); err != nil {
		t.Fatal(err)
	}
	p := mount(s)
	defer p.Close()

	resp := call(t, p.URL, "/llm/pane/p1/responses", "chatgpt-bearer")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got.path != "/responses" || got.auth != "Bearer chatgpt-bearer" {
		t.Fatalf("upstream saw %q/%q, want the codex path and bearer untouched", got.path, got.auth)
	}
}

// An Anthropic-dialect request on a codex-routed pane is refused BEFORE the
// upstream: forwarding it would hand a Claude login to the ChatGPT backend.
func TestCodexAccountRefusesAnthropicDialect(t *testing.T) {
	var got seen
	up := upstream(t, &got)
	defer up.Close()
	s := New(fakeStore{})
	accs := []Account{{Name: "cx", Kind: "codex", BaseURL: up.URL}}
	route := "cx"
	_ = s.Apply(&accs, &route)
	p := mount(s)
	defer p.Close()

	resp := call(t, p.URL, "/llm/pane/p1/v1/messages", "claude-oauth")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 refusal", resp.StatusCode)
	}
	if got.path != "" {
		t.Fatalf("upstream saw %q, want the request never forwarded", got.path)
	}
}

// Empty baseURL on a codex account persists as the ChatGPT backend — creating
// one is just a name and a kind.
func TestCodexAccountDefaultsBaseURL(t *testing.T) {
	s := New(fakeStore{})
	accs := []Account{{Name: "cx", Kind: "codex"}}
	_ = s.Apply(&accs, nil)
	stored, err := s.Accounts()
	if err != nil {
		t.Fatal(err)
	}
	if stored[0].BaseURL != CodexUpstream {
		t.Fatalf("baseURL = %q, want %q", stored[0].BaseURL, CodexUpstream)
	}
}

// AccountNameForKind is the harness-pairing lookup; KindOf answers what a
// route override points at. Both error on an unreadable store rather than
// answering from a phantom empty list.
func TestKindLookups(t *testing.T) {
	s := New(fakeStore{})
	accs := []Account{
		{Name: "max", Kind: "anthropic", BaseURL: "https://api.anthropic.com"},
		{Name: "cx", Kind: "codex"},
	}
	_ = s.Apply(&accs, nil)
	if name, err := s.AccountNameForKind("codex"); err != nil || name != "cx" {
		t.Fatalf("AccountNameForKind = %q, %v", name, err)
	}
	if _, err := s.AccountNameForKind("openai"); err == nil || !strings.Contains(err.Error(), "no openai llm account") {
		t.Fatalf("missing kind must name what to configure, got %v", err)
	}
	if k, err := s.KindOf("cx"); err != nil || k != "codex" {
		t.Fatalf("KindOf(cx) = %q, %v", k, err)
	}
	if k, err := s.KindOf("nobody"); err != nil || k != "" {
		t.Fatalf("KindOf(unknown) = %q, %v", k, err)
	}
	broken := New(brokenStore{})
	if _, err := broken.AccountNameForKind("codex"); err == nil {
		t.Fatal("AccountNameForKind on broken store returned no error")
	}
	if _, err := broken.KindOf("cx"); err == nil {
		t.Fatal("KindOf on broken store returned no error")
	}
}
