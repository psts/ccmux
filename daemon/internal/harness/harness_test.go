package harness

import (
	"errors"
	"strings"
	"testing"
)

type fakeStore map[string]string

func (f fakeStore) GetSetting(key string) (string, error) { return f[key], nil }
func (f fakeStore) SetSetting(key, value string) error    { f[key] = value; return nil }

type brokenStore struct{}

func (brokenStore) GetSetting(string) (string, error) { return "", errors.New("disk io error") }
func (brokenStore) SetSetting(string, string) error   { return nil }

func testService(st Store) *Service {
	s := New(st, func() string { return "env -u TMUX claude --dangerously-load-development-channels server:claude-peers" })
	// Tests must not depend on what the build host has installed.
	s.lookPath = func(string) (string, error) { return "", errors.New("not installed") }
	return s
}

// An installed known program IS a harness — no configuration — and a user
// entry with its name takes over completely.
func TestDetectedHarnesses(t *testing.T) {
	s := testService(fakeStore{})
	s.lookPath = func(name string) (string, error) {
		if name == "opencode" {
			return "/usr/local/bin/opencode", nil
		}
		return "", errors.New("not installed")
	}
	hs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 2 || hs[0].Name != "claude" || hs[1].Name != "opencode" {
		t.Fatalf("list = %+v, want claude + detected opencode", hs)
	}
	if hs[0].Source != "builtin" || hs[1].Source != "detected" {
		t.Fatalf("sources = %q/%q", hs[0].Source, hs[1].Source)
	}
	// Overriding the detected entry replaces it; Source never persists.
	_ = s.Apply([]Harness{{Name: "opencode", Command: "opencode --model x", Source: "detected"}})
	hs, _ = s.List()
	if len(hs) != 2 || hs[1].Command != "opencode --model x" || hs[1].Source != "" {
		t.Fatalf("after override = %+v", hs)
	}
}

// An unconfigured daemon still has the claude harness, wired to the
// configured startup command, with autoconfirm armed.
func TestBuiltinClaudeAlwaysPresent(t *testing.T) {
	s := testService(fakeStore{})
	hs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 1 || hs[0].Name != "claude" || !hs[0].Autoconfirm {
		t.Fatalf("list = %+v, want the built-in claude", hs)
	}
	if !strings.Contains(hs[0].Command, "claude") {
		t.Fatalf("builtin command = %q", hs[0].Command)
	}
	h, err := s.Resolve("claude")
	if err != nil || h.Name != "claude" {
		t.Fatalf("resolve claude = %+v, %v", h, err)
	}
	if _, err := s.Resolve("pi"); err == nil {
		t.Fatal("resolving an unknown harness must error")
	}
}

// A user entry named "claude" replaces the built-in wholesale; other entries
// append after it.
func TestUserListAndClaudeOverride(t *testing.T) {
	s := testService(fakeStore{})
	user := []Harness{
		{Name: "pi", Icon: "π", Command: "pi"},
		{Name: "claude", Command: "claude --continue", Autoconfirm: false},
	}
	if msg := s.Reject(user); msg != "" {
		t.Fatalf("reject: %s", msg)
	}
	if err := s.Apply(user); err != nil {
		t.Fatal(err)
	}
	hs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 2 {
		t.Fatalf("list = %+v, want no duplicate claude", hs)
	}
	c, _ := s.Resolve("claude")
	if c.Command != "claude --continue" || c.Autoconfirm {
		t.Fatalf("override lost: %+v", c)
	}
	if p, _ := s.Resolve("pi"); p.Command != "pi" {
		t.Fatalf("pi = %+v", p)
	}
}

func TestRejectValidation(t *testing.T) {
	s := testService(fakeStore{})
	cases := []struct {
		hs   []Harness
		want string
	}{
		{[]Harness{{Name: "pi", Command: "pi"}}, ""},
		{[]Harness{{Name: "", Command: "x"}}, "empty name"},
		{[]Harness{{Name: "a", Command: "x"}, {Name: "a", Command: "y"}}, "duplicate"},
		{[]Harness{{Name: "a", Command: "  "}}, "no command"},
	}
	for i, c := range cases {
		msg := s.Reject(c.hs)
		if c.want == "" && msg != "" {
			t.Errorf("case %d rejected: %s", i, msg)
		}
		if c.want != "" && !strings.Contains(msg, c.want) {
			t.Errorf("case %d = %q, want %q", i, msg, c.want)
		}
	}
}

// Unreadable is not empty: a failing store errors instead of quietly
// shrinking the list to just the built-in.
func TestBrokenStoreErrors(t *testing.T) {
	s := testService(brokenStore{})
	if _, err := s.List(); err == nil {
		t.Fatal("List on broken store returned no error")
	}
	if _, err := s.Resolve("claude"); err == nil {
		t.Fatal("Resolve on broken store returned no error")
	}
}

// An installed codex is detected like any known harness, and its command
// routes model calls through the pane's llm proxy URL — provider overrides,
// ChatGPT auth kept, websocket off so the reverse proxy can carry the stream.
func TestDetectedCodexCommand(t *testing.T) {
	s := testService(fakeStore{})
	s.lookPath = func(name string) (string, error) {
		if name == "codex" {
			return "/usr/local/bin/codex", nil
		}
		return "", errors.New("not installed")
	}
	h, err := s.Resolve("codex")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"$ANTHROPIC_BASE_URL",
		"requires_openai_auth=true",
		"supports_websockets=false",
		"wire_api=responses",
		"model_provider=ccmux",
	} {
		if !strings.Contains(h.Command, want) {
			t.Errorf("codex command misses %q: %s", want, h.Command)
		}
	}
	if h.Autoconfirm {
		t.Fatal("codex must not arm autoconfirm — its first-run prompts include login choices")
	}
}

// AccountKinds resolves like Source: an entry that says nothing inherits its
// name's default, so overriding the codex command never drops its pairing.
func TestAccountKindsStamping(t *testing.T) {
	s := testService(fakeStore{})
	s.lookPath = func(name string) (string, error) {
		if name == "codex" {
			return "/usr/local/bin/codex", nil
		}
		return "", errors.New("not installed")
	}
	c, err := s.Resolve("claude")
	if err != nil || len(c.AccountKinds) != 2 || c.AccountKinds[0] != "anthropic" || c.AccountKinds[1] != "claude" {
		t.Fatalf("claude kinds = %+v, %v", c.AccountKinds, err)
	}
	// A user override of codex (new command, field left empty) keeps codex's
	// pairing; an explicit field wins over the default.
	_ = s.Apply([]Harness{
		{Name: "codex", Command: "codex --profile x"},
		{Name: "mycustom", Command: "mycustom", AccountKinds: []string{"openai"}},
	})
	cx, _ := s.Resolve("codex")
	if len(cx.AccountKinds) != 1 || cx.AccountKinds[0] != "codex" {
		t.Fatalf("overridden codex kinds = %+v, want inherited default", cx.AccountKinds)
	}
	my, _ := s.Resolve("mycustom")
	if len(my.AccountKinds) != 1 || my.AccountKinds[0] != "openai" {
		t.Fatalf("custom kinds = %+v, want the explicit value", my.AccountKinds)
	}
	if msg := s.Reject([]Harness{{Name: "x", Command: "x", AccountKinds: []string{"weird"}}}); !strings.Contains(msg, "unknown account kind") {
		t.Fatalf("reject = %q, want unknown-kind refusal", msg)
	}
}
