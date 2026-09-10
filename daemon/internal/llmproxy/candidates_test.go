package llmproxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// harnessAt wires a fixed harness declaration for one pane; every other pane
// reads as having none, which is the tier-3 case.
func harnessAt(s *Service, pane string, kinds, order []string) {
	s.SetPaneHarness(func(p string) ([]string, []string, bool) {
		if p != pane {
			return nil, nil, false
		}
		return kinds, order, true
	})
}

func poolNames(t *testing.T, s *Service, pane string) []string {
	t.Helper()
	_, names, err := s.PaneStatus(pane)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func configured(t *testing.T, accs []Account, route string) *Service {
	t.Helper()
	s := New(memStore{})
	if msg := s.Reject(&accs, &route); msg != "" {
		t.Fatalf("reject: %s", msg)
	}
	if err := s.Apply(&accs, &route); err != nil {
		t.Fatal(err)
	}
	return s
}

func fourAccounts() []Account {
	return []Account{
		{Name: "keyed", Kind: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "sk-1"},
		{Name: "sub", Kind: "claude", BaseURL: "https://api.anthropic.com", APIKey: "sk-ant-oat01-a"},
		{Name: "local", Kind: "openai", BaseURL: "http://localhost:11434", APIKey: "k"},
		{Name: "cx", Kind: "codex"},
	}
}

// A harness contributes its declared kinds: accounts of other kinds are not
// in the pane's order at all.
func TestHarnessKindsFilterTheOrder(t *testing.T) {
	s := configured(t, fourAccounts(), "")
	harnessAt(s, "p1", []string{"anthropic", "claude"}, nil)
	got := poolNames(t, s, "p1")
	want := []string{"keyed", "sub"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// With no custom order the accounts follow the order they are configured in,
// which is what the Accounts tab's up/down arrows write.
func TestConfiguredOrderIsTheDefaultOrder(t *testing.T) {
	accs := fourAccounts()
	accs[0], accs[2] = accs[2], accs[0] // local first now
	s := configured(t, accs, "")
	harnessAt(s, "p1", []string{"anthropic", "openai", "claude"}, nil)
	if got := poolNames(t, s, "p1"); got[0] != "local" {
		t.Fatalf("order = %v, want the configured order to lead with local", got)
	}
}

// A harness's own order wins over the configured one, and an account it does
// not name still follows as failover rather than being dropped.
func TestHarnessOrderOverridesConfiguredOrder(t *testing.T) {
	s := configured(t, fourAccounts(), "")
	harnessAt(s, "p1", []string{"anthropic", "openai", "claude"}, []string{"local", "sub"})
	got := poolNames(t, s, "p1")
	want := []string{"local", "sub", "keyed"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// Account names rot. A harness order naming an account that is gone keeps
// working, minus that entry.
func TestDanglingOrderEntryIsSkipped(t *testing.T) {
	s := configured(t, fourAccounts(), "")
	harnessAt(s, "p1", []string{"anthropic", "openai"}, []string{"deleted-last-week", "local"})
	got := poolNames(t, s, "p1")
	want := []string{"local", "keyed"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// A pane's own override is always tried first, with the harness order behind
// it as failover.
func TestPaneOverrideHeadsTheOrder(t *testing.T) {
	s := configured(t, fourAccounts(), "")
	harnessAt(s, "p1", []string{"anthropic", "openai", "claude"}, nil)
	if err := s.SetPaneRoute("p1", "local"); err != nil {
		t.Fatal(err)
	}
	got := poolNames(t, s, "p1")
	want := []string{"local", "keyed", "sub"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// Tier 3: a pane ccmux did not start under a named harness follows the
// default account, and fails over only within that account's own kind.
func TestPaneWithNoHarnessFollowsTheDefaultAccount(t *testing.T) {
	accs := append(fourAccounts(), Account{Name: "sub2", Kind: "claude", BaseURL: "https://api.anthropic.com", APIKey: "sk-ant-oat01-b"})
	s := configured(t, accs, "sub")
	got := poolNames(t, s, "shell-pane")
	want := []string{"sub", "sub2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// The synthetic pass-through forwards the pane's OWN login, so it must not
// fail over onto a keyed account: that would silently change whose
// credential answers.
func TestPassthroughDefaultPoolsWithNothing(t *testing.T) {
	s := configured(t, fourAccounts(), "")
	if got := poolNames(t, s, "shell-pane"); len(got) != 1 || got[0] != "anthropic" {
		t.Fatalf("order = %v, want the lone synthetic pass-through", got)
	}
}

// A codex account may be the default now. A pane with no harness routes to
// it, and Anthropic-dialect traffic on it is refused rather than forwarded —
// the guard that keeps a hand-started claude from handing its login to OpenAI.
func TestCodexAsDefaultAccount(t *testing.T) {
	s := configured(t, fourAccounts(), "cx")
	if got := poolNames(t, s, "shell-pane"); len(got) != 1 || got[0] != "cx" {
		t.Fatalf("order = %v, want cx", got)
	}
	p := mount(s)
	defer p.Close()
	resp := call(t, p.URL, "/llm/pane/shell-pane/v1/messages", "the-panes-own-login")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("claude traffic on a codex default = %d, want 502", resp.StatusCode)
	}
	// The message has to name the setting to change. It used to say "clear
	// the pane's llm route" for a pane that has no route to clear, which sent
	// the reader somewhere that changes nothing.
	body := readBody(t, p.URL, "/llm/pane/shell-pane/v1/messages")
	if !strings.Contains(body, "Default account") {
		t.Fatalf("refusal = %q, want it to name the Default account setting", body)
	}
	if strings.Contains(body, "clear the pane") {
		t.Fatalf("refusal tells the user to clear a route the pane does not have: %q", body)
	}
}

// readBody re-issues a call and returns the response text.
func readBody(t *testing.T, proxyURL, path string) string {
	t.Helper()
	req, _ := http.NewRequest("POST", proxyURL+path, strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// The kind checkboxes are user-overridable, so a mixed-dialect declaration is
// possible. The order must still hold one dialect: a Messages request may
// never be replayed onto a codex upstream.
func TestMixedDialectDeclarationIsFilteredToTheHead(t *testing.T) {
	s := configured(t, fourAccounts(), "")
	harnessAt(s, "p1", []string{"codex", "anthropic"}, []string{"cx", "keyed"})
	if got := poolNames(t, s, "p1"); strings.Join(got, ",") != "cx" {
		t.Fatalf("order = %v, want the codex head alone", got)
	}
	harnessAt(s, "p1", []string{"codex", "anthropic"}, []string{"keyed", "cx"})
	if got := poolNames(t, s, "p1"); strings.Join(got, ",") != "keyed" {
		t.Fatalf("order = %v, want the anthropic head alone", got)
	}
}

// A harness whose kinds match nothing configured still gets a working pane:
// it falls back to the default rather than resolving to an empty order.
func TestHarnessWithNoUsableAccountFallsBackToDefault(t *testing.T) {
	s := configured(t, fourAccounts(), "keyed")
	harnessAt(s, "p1", []string{"meridian"}, nil)
	if got := poolNames(t, s, "p1"); strings.Join(got, ",") != "keyed" {
		t.Fatalf("order = %v, want the default account", got)
	}
}

// aliasUpstream records the model each request asked for and can answer with
// a limit, so a failover can be watched from both ends.
func aliasUpstream(t *testing.T, models *[]string, limit bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		*models = append(*models, req.Model)
		if limit {
			w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
}

// The replay must be built for the account it lands on. Two accounts aliasing
// the same requested model differently is the case that proves it: replaying
// the first account's rewritten body would ask the second upstream for a
// model only the first one serves.
func TestReplayRewritesForTheAccountItLandsOn(t *testing.T) {
	var firstModels, secondModels []string
	first := aliasUpstream(t, &firstModels, true)
	defer first.Close()
	second := aliasUpstream(t, &secondModels, false)
	defer second.Close()

	accs := []Account{
		{Name: "a", Kind: "openai", BaseURL: first.URL, APIKey: "k1",
			ModelAliases: []ModelAlias{{From: "claude-*", To: "model-for-a"}}},
		{Name: "b", Kind: "openai", BaseURL: second.URL, APIKey: "k2",
			ModelAliases: []ModelAlias{{From: "claude-*", To: "model-for-b"}}},
	}
	s := configured(t, accs, "")
	harnessAt(s, "p1", []string{"openai"}, []string{"a", "b"})
	p := mount(s)
	defer p.Close()

	req, _ := http.NewRequest("POST", p.URL+"/llm/pane/p1/v1/messages",
		strings.NewReader(`{"model":"claude-opus-5","messages":[]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(firstModels) != 1 || firstModels[0] != "model-for-a" {
		t.Fatalf("first upstream saw %v, want [model-for-a]", firstModels)
	}
	if len(secondModels) != 1 || secondModels[0] != "model-for-b" {
		t.Fatalf("second upstream saw %v, want [model-for-b] — the replay carried the first account's rewrite", secondModels)
	}
}

// A user account may legally be named "anthropic", which is also the name the
// synthetic pass-through resolves to. Matching on the name would pool a
// pane's own login with keyed accounts and bill an API key for traffic the
// user sent to their subscription.
func TestPassthroughIsNotConfusedWithAnAccountNamedAnthropic(t *testing.T) {
	s := configured(t, []Account{
		{Name: "anthropic", Kind: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "k1"},
		{Name: "work", Kind: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "k2"},
	}, "")
	if got := poolNames(t, s, "shell-pane"); strings.Join(got, ",") != "anthropic" {
		t.Fatalf("order = %v, want the pass-through alone", got)
	}
}

// A meridian account runs a Claude Code loop of its own, so a pane with no
// harness must never land on one: the pane's hand-started claude would run
// inside it. Unlike codex this cannot fail loudly downstream — same dialect,
// same path — so the refusal has to be at the setting.
func TestMeridianIsRefusedAsTheDefaultAccount(t *testing.T) {
	s := New(memStore{})
	accs := []Account{{Name: "m", Kind: KindMeridian, BaseURL: MeridianUpstream, APIKey: "tok"}}
	route := "m"
	if msg := s.Reject(&accs, &route); !strings.Contains(msg, "never as the default") {
		t.Fatalf("meridian as default = %q, want a refusal", msg)
	}
}

// A replay must carry the account it lands on, and NOTHING of the account it
// came from. A keyless account is the case that proves it: applyAuth is a
// no-op for one by design, so any credential left on the cloned request would
// travel to it.
func TestReplayDoesNotCarryThePreviousAccountsKey(t *testing.T) {
	var keys []string
	keyed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer keyed.Close()
	keyless := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("x-api-key")+"|"+r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer keyless.Close()

	s := configured(t, []Account{
		{Name: "keyed", Kind: "anthropic", BaseURL: keyed.URL, APIKey: "sk-ant-secret"},
		{Name: "local", Kind: "anthropic", BaseURL: keyless.URL},
	}, "")
	harnessAt(s, "p1", []string{"anthropic"}, []string{"keyed", "local"})
	p := mount(s)
	defer p.Close()

	call(t, p.URL, "/llm/pane/p1/v1/messages", "the-panes-own-login")
	if len(keys) != 1 {
		t.Fatalf("keyless upstream saw %d requests, want the replay", len(keys))
	}
	if strings.Contains(keys[0], "sk-ant-secret") {
		t.Fatalf("replay carried the keyed account's credential: %q", keys[0])
	}
	// A keyless account is a pass-through, so what it SHOULD see is the
	// pane's own login, restored from what the pane sent.
	if keys[0] != "|Bearer the-panes-own-login" {
		t.Fatalf("keyless upstream saw %q, want the pane's own bearer", keys[0])
	}
}

// opencode and pi declare meridian so they spend a Claude SUBSCRIPTION
// through the sidecar rather than a metered key. Ordering them by the account
// list alone would move that spend to the key with nothing announcing it, so
// a meridian account leads for a harness that asked for one.
func TestMeridianLeadsForAHarnessThatDeclaredIt(t *testing.T) {
	accs := []Account{
		{Name: "keyed", Kind: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "k"},
		{Name: "sidecar", Kind: KindMeridian, BaseURL: MeridianUpstream, APIKey: "tok"},
	}
	s := configured(t, accs, "keyed")
	harnessAt(s, "p1", []string{"meridian", "anthropic", "openai"}, nil)
	if got := poolNames(t, s, "p1"); strings.Join(got, ",") != "sidecar,keyed" {
		t.Fatalf("opencode order = %v, want the sidecar first", got)
	}
	// A harness that never asked for meridian cannot reach one at all, and
	// the account order is untouched for it.
	harnessAt(s, "p2", []string{"anthropic", "openai"}, nil)
	if got := poolNames(t, s, "p2"); strings.Join(got, ",") != "keyed" {
		t.Fatalf("claude order = %v, want the keyed account alone", got)
	}
	// And the per-harness order still wins: it is the visible rule.
	harnessAt(s, "p1", []string{"meridian", "anthropic", "openai"}, []string{"keyed"})
	if got := poolNames(t, s, "p1"); strings.Join(got, ",") != "keyed,sidecar" {
		t.Fatalf("explicit order = %v, want the user's order to win", got)
	}
}

// The dialect guard has to hold in BOTH directions. A codex pane whose
// harness matches no codex account falls through to the keyless Anthropic
// pass-through, and applyAuth is a no-op for a keyless account — so without
// the reverse half, the pane's own ChatGPT bearer travelled to Anthropic.
func TestCodexDialectPathIsRefusedOnANonCodexAccount(t *testing.T) {
	var got seen
	up := upstream(t, &got)
	defer up.Close()
	s := configured(t, []Account{{Name: "local", Kind: "anthropic", BaseURL: "http://127.0.0.1:1"}}, "")
	s.defaultUpstream = up.URL
	// The pane runs codex, but nothing configured serves that kind.
	harnessAt(s, "p1", []string{"codex"}, nil)
	p := mount(s)
	defer p.Close()

	resp := call(t, p.URL, "/llm/pane/p1/responses", "chatgpt-oauth-token")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("/responses on an anthropic account = %d, want 502", resp.StatusCode)
	}
	if got.auth != "" {
		t.Fatalf("the pane's bearer reached the upstream: %q", got.auth)
	}
}

// Go's ServeMux cleans the ESCAPED path, so %2e%2e survives routing and the
// decoded rest carried the traversal out to the upstream — past the allowlist
// that exists to keep a Claude bearer off chatgpt.com.
func TestCodexAllowlistRefusesTraversal(t *testing.T) {
	var got seen
	up := upstream(t, &got)
	defer up.Close()
	s := configured(t, []Account{{Name: "cx", Kind: "codex", BaseURL: up.URL}}, "cx")
	p := mount(s)
	defer p.Close()

	for _, path := range []string{
		"/llm/pane/p1/responses/%2e%2e/%2e%2e/v1/messages",
		"/llm/pane/p1/responses/../v1/messages",
		"/llm/pane/p1/v1/messages",
	} {
		got = seen{}
		if resp := call(t, p.URL, path, "claude-oauth-token"); resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("%s = %d, want 502", path, resp.StatusCode)
		}
		if got.auth != "" {
			t.Fatalf("%s: bearer reached the upstream: %q", path, got.auth)
		}
	}
	// The real surface still works, and a sibling name is not a prefix match.
	if resp := call(t, p.URL, "/llm/pane/p1/responses", "tok"); resp.StatusCode != 200 {
		t.Fatalf("/responses = %d, want 200", resp.StatusCode)
	}
	if resp := call(t, p.URL, "/llm/pane/p1/responsesXYZ", "tok"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("/responsesXYZ = %d, want 502", resp.StatusCode)
	}
}

// The messages side was a denylist, so firstSegment's "unparseable" sentinel
// read as "not /responses" and ALLOWED. Both of these reached the upstream
// unescaped for it to normalize back into /responses, carrying the pane's own
// bearer — the traversal leak pointed the other way.
func TestTraversalOntoTheMessagesSideIsRefused(t *testing.T) {
	var got seen
	up := upstream(t, &got)
	defer up.Close()
	s := configured(t, nil, "")
	s.defaultUpstream = up.URL
	harnessAt(s, "p1", []string{"codex"}, nil)
	p := mount(s)
	defer p.Close()

	for _, path := range []string{
		"/llm/pane/p1/x/%2e%2e/responses",
		"/llm/pane/p1/%2fresponses",
		"/llm/pane/p1/responses",
	} {
		got = seen{}
		if resp := call(t, p.URL, path, "chatgpt-oauth-token"); resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("%s = %d, want 502", path, resp.StatusCode)
		}
		if got.auth != "" {
			t.Fatalf("%s: the pane's bearer reached the upstream: %q", path, got.auth)
		}
	}
}

// /models is the one path both dialects serve, and the codex CLI really asks
// for it. On a keyless pass-through that forwards the CALLER's credential, so
// a misrouted codex pane leaked its ChatGPT bearer on the model list even
// once /responses was refused. A keyed account replaces the credential and
// keeps the shared path.
func TestSharedModelsPathIsRefusedOnAKeylessAccount(t *testing.T) {
	var got seen
	up := upstream(t, &got)
	defer up.Close()
	s := configured(t, nil, "")
	s.defaultUpstream = up.URL
	harnessAt(s, "p1", []string{"codex"}, nil)
	p := mount(s)
	defer p.Close()
	if resp := call(t, p.URL, "/llm/pane/p1/models", "chatgpt-oauth-token"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("models on a keyless account = %d, want 502", resp.StatusCode)
	}
	if got.auth != "" {
		t.Fatalf("bearer reached the upstream: %q", got.auth)
	}

	// Keyed: the account's own credential answers, so the shared path stays.
	keyed := configured(t, []Account{{Name: "org", Kind: "anthropic", BaseURL: up.URL, APIKey: "sk-org"}}, "org")
	kp := mount(keyed)
	defer kp.Close()
	got = seen{}
	if resp := call(t, kp.URL, "/llm/pane/p2/models", "the-panes-own-login"); resp.StatusCode != 200 {
		t.Fatalf("models on a keyed account = %d, want 200", resp.StatusCode)
	}
	if got.apiKey != "sk-org" || got.auth != "" {
		t.Fatalf("keyed account sent %q/%q, want its own key and no bearer", got.apiKey, got.auth)
	}
	// And the Anthropic surface is untouched by any of this.
	if resp := call(t, kp.URL, "/llm/pane/p2/v1/messages", "x"); resp.StatusCode != 200 {
		t.Fatalf("v1/messages = %d, want 200", resp.StatusCode)
	}
}

// A harness with NO declared kinds is every user-added registry entry, and
// checkHarnessAccounts returns early for those — so nothing stops one being
// started on a daemon whose only account is a codex one set as the default.
// It falls to tier 3, and the request has to fail with advice that points at
// the setting actually responsible.
func TestKindlessHarnessOnACodexDefaultFailsWithUsableAdvice(t *testing.T) {
	s := configured(t, []Account{{Name: "cx", Kind: "codex"}}, "cx")
	harnessAt(s, "p1", nil, nil) // declared kinds: none
	if got := poolNames(t, s, "p1"); strings.Join(got, ",") != "cx" {
		t.Fatalf("order = %v, want the default account", got)
	}
	p := mount(s)
	defer p.Close()
	if resp := call(t, p.URL, "/llm/pane/p1/v1/messages", "tok"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("= %d, want 502", resp.StatusCode)
	}
	body := readBody(t, p.URL, "/llm/pane/p1/v1/messages")
	if !strings.Contains(body, "Default account") {
		t.Fatalf("refusal = %q, want it to name the Default account setting", body)
	}
}

// Leak 5: every codex account is keyless by validation, so a keyless-based
// rule could never fire on the codex side — bare /models served 200 with the
// caller's bearer reaching chatgpt.com. The guard keys on the PANE's declared
// dialect now, so this is refused whatever the path.
func TestCodexAccountRefusesAForeignPaneOnEveryPath(t *testing.T) {
	var got seen
	up := upstream(t, &got)
	defer up.Close()
	s := configured(t, []Account{{Name: "cx", Kind: "codex", BaseURL: up.URL}}, "cx")
	// A pane whose harness speaks the Anthropic dialect, routed at codex.
	harnessAt(s, "p1", []string{"anthropic", "openai", "claude"}, nil)
	p := mount(s)
	defer p.Close()
	for _, path := range []string{"models", "responses", "v1/messages", "chat/completions"} {
		got = seen{}
		resp := call(t, p.URL, "/llm/pane/p1/"+path, "claude-oauth-token")
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("%s = %d, want 502", path, resp.StatusCode)
		}
		if got.auth != "" {
			t.Fatalf("%s: bearer reached the upstream: %q", path, got.auth)
		}
	}
	// A real codex pane still gets its own surface.
	harnessAt(s, "p2", []string{"codex"}, nil)
	for _, path := range []string{"models", "responses"} {
		if resp := call(t, p.URL, "/llm/pane/p2/"+path, "tok"); resp.StatusCode != 200 {
			t.Fatalf("codex pane %s = %d, want 200", path, resp.StatusCode)
		}
	}
}

// The messages side was a denylist of one lowercase segment, so every other
// OpenAI root reached a keyless pass-through with the caller's bearer.
func TestKeylessPassthroughRefusesForeignDialectPaths(t *testing.T) {
	var got seen
	up := upstream(t, &got)
	defer up.Close()
	s := configured(t, nil, "")
	s.defaultUpstream = up.URL
	harnessAt(s, "p1", []string{"codex"}, nil) // codex pane, tier-3 fallback
	p := mount(s)
	defer p.Close()
	for _, path := range []string{"chat/completions", "v1/responses", "completions", "models"} {
		got = seen{}
		resp := call(t, p.URL, "/llm/pane/p1/"+path, "chatgpt-oauth-token")
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("%s = %d, want 502", path, resp.StatusCode)
		}
		if got.auth != "" {
			t.Fatalf("%s: bearer reached the upstream: %q", path, got.auth)
		}
	}
	// The pass-through's own reason for existing still works: a Claude pane
	// forwarding its own login to Anthropic.
	harnessAt(s, "p2", []string{"anthropic", "claude"}, nil)
	got = seen{}
	if resp := call(t, p.URL, "/llm/pane/p2/v1/messages", "max-oauth"); resp.StatusCode != 200 {
		t.Fatalf("v1/messages = %d, want 200", resp.StatusCode)
	}
	if got.auth != "Bearer max-oauth" {
		t.Fatalf("pass-through sent %q, want the pane's own login", got.auth)
	}
}

// A harness may declare codex ALONGSIDE the Anthropic kinds — both editors
// offer the kinds as independent checkboxes, and the mixed declaration is a
// supported pairing. Reading "contains codex" as "speaks the Responses
// dialect" refused every request such a pane made on a keyless Anthropic
// account, including the tier-3 pass-through.
func TestMixedKindHarnessStillServesTheAnthropicSurface(t *testing.T) {
	var got seen
	up := upstream(t, &got)
	defer up.Close()
	s := configured(t, nil, "")
	s.defaultUpstream = up.URL
	harnessAt(s, "p1", []string{"codex", "anthropic"}, nil)
	p := mount(s)
	defer p.Close()
	if resp := call(t, p.URL, "/llm/pane/p1/v1/messages", "max-oauth"); resp.StatusCode != 200 {
		t.Fatalf("v1/messages on a mixed-kind pane = %d, want 200", resp.StatusCode)
	}
	if got.auth != "Bearer max-oauth" {
		t.Fatalf("pass-through sent %q, want the pane's own login", got.auth)
	}
	// The ambiguity does not reopen the shared paths on a keyless account.
	for _, path := range []string{"responses", "models"} {
		if resp := call(t, p.URL, "/llm/pane/p1/"+path, "tok"); resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("%s = %d, want 502", path, resp.StatusCode)
		}
	}
}

// A mixed declaration says nothing about the dialect, so the pane-dialect
// check cannot fire — which left the path as the only guard, and as a
// denylist of two segments it passed every other OpenAI root to a keyless
// pass-through with the caller's bearer.
func TestMixedKindPaneCannotReachForeignRootsOnAKeylessAccount(t *testing.T) {
	var got seen
	up := upstream(t, &got)
	defer up.Close()
	s := configured(t, nil, "")
	s.defaultUpstream = up.URL
	harnessAt(s, "p1", []string{"codex", "anthropic"}, nil)
	p := mount(s)
	defer p.Close()
	for _, path := range []string{
		"chat/completions", "completions", "v1/responses", "responses", "models",
		"v1/chat/completions", "backend-api/codex/responses",
	} {
		got = seen{}
		resp := call(t, p.URL, "/llm/pane/p1/"+path, "chatgpt-oauth-token")
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("%s = %d, want 502", path, resp.StatusCode)
		}
		if got.auth != "" {
			t.Fatalf("%s: bearer reached the upstream: %q", path, got.auth)
		}
	}
}

// The surfaces are measured, not guessed: 30 days of this daemon's proxy log
// carried exactly these. /api/hello is Claude Code's startup probe and a
// v1-only allowlist would have broken it.
func TestMeasuredSurfacesStillServe(t *testing.T) {
	var got seen
	up := upstream(t, &got)
	defer up.Close()
	s := configured(t, nil, "")
	s.defaultUpstream = up.URL
	harnessAt(s, "p1", []string{"anthropic", "claude"}, nil)
	p := mount(s)
	defer p.Close()
	for _, path := range []string{"v1/messages", "v1/messages/count_tokens", "api/hello"} {
		if resp := call(t, p.URL, "/llm/pane/p1/"+path, "max-oauth"); resp.StatusCode != 200 {
			t.Fatalf("%s = %d, want 200 — this is real traffic", path, resp.StatusCode)
		}
	}
	// And a real codex pane keeps its own two.
	cx := configured(t, []Account{{Name: "cx", Kind: "codex", BaseURL: up.URL}}, "cx")
	harnessAt(cx, "p2", []string{"codex"}, nil)
	cp := mount(cx)
	defer cp.Close()
	for _, path := range []string{"models", "responses"} {
		if resp := call(t, cp.URL, "/llm/pane/p2/"+path, "tok"); resp.StatusCode != 200 {
			t.Fatalf("codex %s = %d, want 200", path, resp.StatusCode)
		}
	}
}

// A path outside a keyless account's surface is a different problem from a
// dialect mismatch, and it needs different advice: the account is the right
// kind, so "pick a different Default account" cannot fix it — with no
// accounts configured there is nothing to pick, and every other keyless
// account of that kind refuses the same path. Name the path instead.
func TestOffSurfaceRefusalNamesThePathNotTheSetting(t *testing.T) {
	s := configured(t, nil, "") // tier-3 keyless pass-through
	p := mount(s)
	defer p.Close()
	body := readBody(t, p.URL, "/llm/pane/p1/v1/models")
	if !strings.Contains(body, "v1/models") {
		t.Fatalf("refusal = %q, want it to name the path", body)
	}
	// The phrasing that cannot be acted on is "pick a DIFFERENT Default
	// account" when the account is the right kind. Naming the Default account
	// as the place to add one is the opposite: it is the action that works.
	if strings.Contains(body, "different Default account") {
		t.Fatalf("refusal gives advice that cannot fix it: %q", body)
	}
	// A codex account still points at the setting: it cannot hold a key, so
	// "give the account a key" would be impossible advice.
	cx := configured(t, []Account{{Name: "cx", Kind: "codex"}}, "cx")
	cp := mount(cx)
	defer cp.Close()
	cxBody := readBody(t, cp.URL, "/llm/pane/p1/v1/messages")
	if !strings.Contains(cxBody, "Default account") {
		t.Fatalf("codex refusal = %q, want it to name the setting", cxBody)
	}
	if strings.Contains(cxBody, "Give the account a key") {
		t.Fatalf("codex refusal suggests a key, which validateKind refuses: %q", cxBody)
	}
}

// The synthetic pass-through is fabricated as {Name:"anthropic"}, a name no
// settings list holds and one a REAL account may legally take. Naming it as
// an account was meaningless with none configured, and a flat contradiction
// when a keyed account of that name is sitting in settings.
func TestPassthroughRefusalDoesNotImpersonateAnAccount(t *testing.T) {
	s := configured(t, []Account{
		{Name: "anthropic", Kind: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "sk-1"},
	}, "") // empty route: the pane is on the pass-through, not on that account
	p := mount(s)
	defer p.Close()
	body := readBody(t, p.URL, "/llm/pane/p1/v1/models")
	if strings.Contains(body, "holds no key") {
		t.Fatalf("refusal claims a keyed account holds no key: %q", body)
	}
	if !strings.Contains(body, "pass-through") || !strings.Contains(body, "v1/models") {
		t.Fatalf("refusal = %q, want it to name the pass-through and the path", body)
	}

	// A genuine keyless account still gets the wording aimed at it.
	real := configured(t, []Account{
		{Name: "local", Kind: "anthropic", BaseURL: "http://localhost:11434"},
	}, "local")
	rp := mount(real)
	defer rp.Close()
	rb := readBody(t, rp.URL, "/llm/pane/p1/v1/models")
	if !strings.Contains(rb, "account local holds no key") {
		t.Fatalf("keyless-account refusal = %q, want it to name that account", rb)
	}
}

// A pane with a recorded harness resolves at TIER 2 — harnessPool runs before
// defaultPool is ever consulted — so an empty Default account says nothing
// about which account it is on. Deciding "is this the pass-through" from the
// route in the handler got that backwards and told a pane sitting on a real
// keyless account to go add one.
func TestHarnessPaneOnAKeylessAccountIsNotCalledThePassthrough(t *testing.T) {
	s := configured(t, []Account{
		{Name: "local", Kind: "anthropic", BaseURL: "http://localhost:11434"},
	}, "") // no Default account picked: the initial state
	harnessAt(s, "p1", []string{"anthropic"}, nil)
	p := mount(s)
	defer p.Close()
	body := readBody(t, p.URL, "/llm/pane/p1/v1/models")
	if !strings.Contains(body, "account local holds no key") {
		t.Fatalf("refusal = %q, want it to name the account actually serving the pane", body)
	}
	if strings.Contains(body, "pass-through") {
		t.Fatalf("refusal calls a tier-2 pane the pass-through: %q", body)
	}
	// And a pane with no harness, same settings, really is on the
	// pass-through — tier 3 with an empty route.
	if got := readBody(t, p.URL, "/llm/pane/shell/v1/models"); !strings.Contains(got, "pass-through") {
		t.Fatalf("tier-3 refusal = %q, want the pass-through wording", got)
	}
}

// A keyless account is a pass-through carrying the PANE's own login, so a 401
// from one means the user's credential died, not the account's. Replaying it
// onto the keyed subscription behind it would hide that they need to log in
// again and bill a different account for the answer.
func TestKeylessUnauthorizedDoesNotFailOver(t *testing.T) {
	keyless := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer keyless.Close()
	keyedHits := 0
	keyed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyedHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer keyed.Close()

	s := configured(t, []Account{
		{Name: "local", Kind: "anthropic", BaseURL: keyless.URL},
		{Name: "keyed", Kind: "anthropic", BaseURL: keyed.URL, APIKey: "sk-ant-secret"},
	}, "")
	harnessAt(s, "p1", []string{"anthropic"}, []string{"local", "keyed"})
	p := mount(s)
	defer p.Close()

	if resp := call(t, p.URL, "/llm/pane/p1/v1/messages", "the-panes-dead-login"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the pane's own rejected login surfaced", resp.StatusCode)
	}
	if keyedHits != 0 {
		t.Fatalf("keyed upstream hits = %d, want the pane's dead login not billed to a subscription", keyedHits)
	}
}

// A keyed account's 401 still fails over: that credential is stored in
// settings and the pane's user cannot fix it from the pane.
func TestKeyedUnauthorizedStillFailsOver(t *testing.T) {
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer rejecting.Close()
	nextHits := 0
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer next.Close()

	s := configured(t, []Account{
		{Name: "dead", Kind: "anthropic", BaseURL: rejecting.URL, APIKey: "sk-ant-dead"},
		{Name: "live", Kind: "anthropic", BaseURL: next.URL, APIKey: "sk-ant-live"},
	}, "")
	harnessAt(s, "p1", []string{"anthropic"}, []string{"dead", "live"})
	p := mount(s)
	defer p.Close()

	if resp := call(t, p.URL, "/llm/pane/p1/v1/messages", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want a stored dead token hidden by failover", resp.StatusCode)
	}
	if nextHits != 1 {
		t.Fatalf("second account hits = %d, want the replay to land", nextHits)
	}
}
