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
// panes never have to hold provider secrets. Because pass-through forwards
// the pane's own login token, a keyless account may only point at Anthropic
// itself, localhost, or a private-network IP — anywhere else must bring its
// own key, or a single settings write would exfiltrate every pane's token.
package llmproxy

import (
	"encoding/json"
	"fmt"
	"net/netip"
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

// Accounts returns the configured accounts. A read or decode failure is an
// error, never an empty list: acting on "empty" would silently misroute
// traffic, render a blank settings page a save would then persist, and let
// Apply overwrite stored keys against a phantom state. Unreadable is not
// empty — the same rule the lenses learned in 345e042.
func (s *Service) Accounts() ([]Account, error) {
	raw, err := s.store.GetSetting(settingAccounts)
	if err != nil {
		return nil, fmt.Errorf("llm accounts unreadable: %w", err)
	}
	if raw == "" {
		return []Account{}, nil
	}
	var accs []Account
	if err := json.Unmarshal([]byte(raw), &accs); err != nil {
		return nil, fmt.Errorf("llm accounts corrupt: %w", err)
	}
	return accs, nil
}

// Route returns the global route: the name of the account LLM traffic goes to,
// "" for the built-in Anthropic pass-through.
func (s *Service) Route() (string, error) {
	v, err := s.store.GetSetting(settingRoute)
	if err != nil {
		return "", fmt.Errorf("llm route unreadable: %w", err)
	}
	return v, nil
}

// Snapshot is the settings surface's read: redacted accounts plus the route,
// or the error that must become a 5xx rather than an empty page.
func (s *Service) Snapshot() ([]RedactedAccount, string, error) {
	accs, err := s.Accounts()
	if err != nil {
		return nil, "", err
	}
	route, err := s.Route()
	if err != nil {
		return nil, "", err
	}
	out := make([]RedactedAccount, 0, len(accs))
	for _, a := range accs {
		out = append(out, RedactedAccount{Name: a.Name, Kind: a.Kind, BaseURL: a.BaseURL, APIKeySet: a.APIKey != ""})
	}
	return out, route, nil
}

// Reject validates a proposed settings change before anything persists. Either
// field may be nil ("leave alone"); the proposal is checked as the state Apply
// would produce — normalized, stored keys inherited — so a request cannot
// route to an account it also removes, and a keyed account resubmitted from
// the redacted view isn't mistaken for a keyless pass-through.
// Returns the message to refuse with, or "" to accept.
func (s *Service) Reject(accs *[]Account, route *string) string {
	stored, err := s.Accounts()
	if err != nil {
		return err.Error()
	}
	effRoute, err := s.Route()
	if err != nil {
		return err.Error()
	}
	effAccs := stored
	if accs != nil {
		effAccs = merged(stored, *accs)
		if msg := validateAccounts(effAccs); msg != "" {
			return msg
		}
	}
	if route != nil {
		effRoute = strings.TrimSpace(*route)
	}
	if effRoute != "" && findAccount(effAccs, effRoute) == nil {
		return fmt.Sprintf("llmRoute %q names no llm account", effRoute)
	}
	return ""
}

func validateAccounts(accs []Account) string {
	seen := map[string]bool{}
	for _, a := range accs {
		if a.Name == "" {
			return "llm account with an empty name"
		}
		if seen[a.Name] {
			return fmt.Sprintf("duplicate llm account %q", a.Name)
		}
		seen[a.Name] = true
		if a.Kind != "anthropic" && a.Kind != "openai" {
			return fmt.Sprintf("llm account %q: unknown kind %q (anthropic or openai)", a.Name, a.Kind)
		}
		u, err := url.Parse(a.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Sprintf("llm account %q: baseURL must be an http(s) URL", a.Name)
		}
		if a.APIKey == "" && !passthroughHostAllowed(u.Hostname()) {
			return fmt.Sprintf("llm account %q has no api key, so each pane's own Claude login would be forwarded to it — that is only allowed to api.anthropic.com, localhost, or a private-network IP", a.Name)
		}
	}
	return ""
}

// cgnat is the tailnet address range (100.64.0.0/10) — machines on the user's
// own tailnet count as private for pass-through purposes.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// passthroughHostAllowed limits where a keyless account may forward the
// pane's own credentials: Anthropic itself, or hosts the user plausibly owns.
// Hostnames other than localhost are refused outright rather than resolved —
// DNS at validation time proves nothing about DNS at request time.
func passthroughHostAllowed(hostname string) bool {
	if hostname == "api.anthropic.com" || hostname == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(hostname)
	if err != nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || cgnat.Contains(ip)
}

// Apply persists the change Reject accepted. A nil field is left alone. A
// failed read refuses the write: merging against a phantom empty list would
// wipe every stored key.
func (s *Service) Apply(accs *[]Account, route *string) error {
	if accs != nil {
		old, err := s.Accounts()
		if err != nil {
			return fmt.Errorf("refusing to write llm accounts: %w", err)
		}
		b, err := json.Marshal(merged(old, *accs))
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

// merged is the state an incoming accounts list persists as: names and URLs
// trimmed, kind defaulted, and empty keys inheriting the stored key for the
// same name (see Account.APIKey). Reject validates this, Apply writes it.
func merged(old, incoming []Account) []Account {
	next := make([]Account, 0, len(incoming))
	for _, a := range incoming {
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
	return next
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
// carries it from day one so no pane ever needs new env to gain them. Any
// failure to read the truth is a loud error, never a silent default: a proxy
// that guesses where to send tokens is worse than one that refuses.
func (s *Service) resolve() (Account, error) {
	route, err := s.Route()
	if err != nil {
		return Account{}, err
	}
	if route == "" {
		return Account{Name: "anthropic", Kind: "anthropic", BaseURL: s.defaultUpstream}, nil
	}
	accs, err := s.Accounts()
	if err != nil {
		return Account{}, err
	}
	if a := findAccount(accs, route); a != nil {
		return *a, nil
	}
	// Reject should make this unreachable; if the registry was edited out from
	// under us, failing loudly beats silently burning tokens somewhere else.
	return Account{}, fmt.Errorf("llm route %q names no account", route)
}
