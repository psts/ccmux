// Package llmproxy routes LLM API traffic from hosted panes to a configured
// upstream. Every pane's shell gets a stable loopback ANTHROPIC_BASE_URL of
// the form <daemon>/llm/pane/<paneID>; WHICH upstream answers is resolved
// fresh on every request from the settings table. The URL is deliberately
// decision-free: pane env freezes at tmux session creation and tmux sessions
// outlive daemon restarts (see cmd/ccmuxd/main.go on the retired
// CCMUX_PEERS_URL), so a routing choice baked into it could never be changed
// without killing the pane.
//
// With no route configured the proxy is a pure pass-through to Anthropic:
// auth headers travel untouched, so a Claude Max OAuth login works exactly as
// it does without the proxy (verified end to end against a 20x Max account).
// A keyed account instead REPLACES the client's auth with the stored key, so
// panes never have to hold provider secrets.
package llmproxy

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Store is the slice of the registry the proxy needs (satisfied by *store.SQLite).
type Store interface {
	GetSetting(key string) (string, error)
	SetSetting(key, value string) error
}

// Account is one configured upstream: a place LLM requests can be sent, with
// the credential to use there. Kind picks the auth header the key rides in —
// "anthropic" (x-api-key) or "openai" (Authorization: Bearer). An account
// with no key is a pass-through: the client's own auth (e.g. the Claude Max
// OAuth token) travels to the upstream untouched.
type Account struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	BaseURL string `json:"baseURL"`
	// APIKey is write-only through the settings API. Setting an account whose
	// name already exists with an EMPTY key keeps the stored key — that is what
	// lets the settings UI round-trip the redacted list without wiping secrets.
	// Deleting the account is what clears its key.
	APIKey string `json:"apiKey,omitempty"`
}

// RedactedAccount is what GET /v1/settings reports: presence, never the key.
type RedactedAccount struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	BaseURL   string `json:"baseURL"`
	APIKeySet bool   `json:"apiKeySet"`
}

const (
	settingAccounts = "llm_accounts"
	settingRoute    = "llm_route"
)

// DefaultUpstream answers when no route is configured: straight to Anthropic,
// byte-for-byte what the pane would have done with no proxy at all.
const DefaultUpstream = "https://api.anthropic.com"

type Service struct {
	store Store
	// defaultUpstream is DefaultUpstream in production; tests point it at a
	// local httptest server.
	defaultUpstream string
}

func New(store Store) *Service {
	return &Service{store: store, defaultUpstream: DefaultUpstream}
}

// Accounts returns the configured accounts (empty when unset or unreadable).
func (s *Service) Accounts() []Account {
	raw, err := s.store.GetSetting(settingAccounts)
	if err != nil || raw == "" {
		return []Account{}
	}
	var accs []Account
	if json.Unmarshal([]byte(raw), &accs) != nil {
		return []Account{}
	}
	return accs
}

// Redacted reports the accounts for the settings surface: key presence only.
func (s *Service) Redacted() []RedactedAccount {
	accs := s.Accounts()
	out := make([]RedactedAccount, 0, len(accs))
	for _, a := range accs {
		out = append(out, RedactedAccount{Name: a.Name, Kind: a.Kind, BaseURL: a.BaseURL, APIKeySet: a.APIKey != ""})
	}
	return out
}

// Route returns the global route: the name of the account LLM traffic goes to,
// "" for the built-in Anthropic pass-through.
func (s *Service) Route() string {
	v, _ := s.store.GetSetting(settingRoute)
	return v
}

// Reject validates a proposed settings change before anything persists. Either
// field may be nil ("leave alone"); the proposal is checked as the state it
// would produce, so a request cannot route to an account it also removes.
// Returns the message to refuse with, or "" to accept.
func (s *Service) Reject(accs *[]Account, route *string) string {
	effAccs := s.Accounts()
	if accs != nil {
		effAccs = *accs
	}
	effRoute := s.Route()
	if route != nil {
		effRoute = strings.TrimSpace(*route)
	}
	if accs != nil {
		if msg := validateAccounts(effAccs); msg != "" {
			return msg
		}
	}
	if effRoute != "" && findAccount(effAccs, effRoute) == nil {
		return fmt.Sprintf("llmRoute %q names no llm account", effRoute)
	}
	return ""
}

func validateAccounts(accs []Account) string {
	seen := map[string]bool{}
	for _, a := range accs {
		name := strings.TrimSpace(a.Name)
		if name == "" {
			return "llm account with an empty name"
		}
		if seen[name] {
			return fmt.Sprintf("duplicate llm account %q", name)
		}
		seen[name] = true
		if a.Kind != "" && a.Kind != "anthropic" && a.Kind != "openai" {
			return fmt.Sprintf("llm account %q: unknown kind %q (anthropic or openai)", name, a.Kind)
		}
		u, err := url.Parse(strings.TrimSpace(a.BaseURL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Sprintf("llm account %q: baseURL must be an http(s) URL", name)
		}
	}
	return ""
}

// Apply persists the change Reject accepted. A nil field is left alone. An
// incoming account with an empty key inherits the stored key for its name
// (see Account.APIKey); normalization mirrors validation.
func (s *Service) Apply(accs *[]Account, route *string) error {
	if accs != nil {
		old := s.Accounts()
		next := make([]Account, 0, len(*accs))
		for _, a := range *accs {
			a.Name = strings.TrimSpace(a.Name)
			a.BaseURL = strings.TrimRight(strings.TrimSpace(a.BaseURL), "/")
			if a.Kind == "" {
				a.Kind = "anthropic"
			}
			if a.APIKey == "" {
				if prev := findAccount(old, a.Name); prev != nil {
					a.APIKey = prev.APIKey
				}
			}
			next = append(next, a)
		}
		b, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if err := s.store.SetSetting(settingAccounts, string(b)); err != nil {
			return err
		}
	}
	if route != nil {
		return s.store.SetSetting(settingRoute, strings.TrimSpace(*route))
	}
	return nil
}

func findAccount(accs []Account, name string) *Account {
	for i := range accs {
		if accs[i].Name == name {
			return &accs[i]
		}
	}
	return nil
}

// resolve picks the account answering for a pane right now. Today the pane
// only scopes logging — per-pane rules are the next phase — but the URL shape
// carries it from day one so no pane ever needs new env to gain them.
func (s *Service) resolve() (Account, error) {
	route := s.Route()
	if route == "" {
		return Account{Name: "anthropic", Kind: "anthropic", BaseURL: s.defaultUpstream}, nil
	}
	if a := findAccount(s.Accounts(), route); a != nil {
		return *a, nil
	}
	// Reject should make this unreachable; if the registry was edited out from
	// under us, failing loudly beats silently burning tokens somewhere else.
	return Account{}, fmt.Errorf("llm route %q names no account", route)
}
