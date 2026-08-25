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
	"errors"
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
// OAuth token) travels to the upstream untouched. Kind "codex" is always a
// pass-through: the codex harness sends its own ChatGPT bearer (rotating
// tokens in ~/.codex/auth.json that only codex can refresh), so there is no
// key the proxy could hold — the account exists to name the ChatGPT upstream
// and let harness starts route their pane to it.
type Account struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	BaseURL string `json:"baseURL"`
	// APIKey is write-only through the settings API. Setting an account whose
	// name already exists with an EMPTY key keeps the stored key — that is what
	// lets the settings UI round-trip the redacted list without wiping secrets.
	// Deleting the account is what clears its key.
	APIKey string `json:"apiKey,omitempty"`
	// ModelAliases rewrite request model names on the way through (first match
	// wins; a From ending in '*' matches by prefix). This is what lets a local
	// upstream answer for names it has never heard of — Claude Code's
	// background calls hardwire haiku model names that 404 on Ollama.
	ModelAliases []ModelAlias `json:"modelAliases,omitempty"`
}

// ModelAlias maps one requested model name (or '*'-suffixed prefix) to the
// model the account's upstream actually serves.
type ModelAlias struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// RedactedAccount is what GET /v1/settings reports: presence, never the key.
// Aliases are not secrets, so they round-trip in full for the editor.
type RedactedAccount struct {
	Name         string       `json:"name"`
	Kind         string       `json:"kind"`
	BaseURL      string       `json:"baseURL"`
	APIKeySet    bool         `json:"apiKeySet"`
	ModelAliases []ModelAlias `json:"modelAliases,omitempty"`
}

const (
	settingAccounts   = "llm_accounts"
	settingRoute      = "llm_route"
	settingPaneRoutes = "llm_pane_routes"
)

// ErrUnknownAccount marks a route naming no configured account — the API
// layer answers it with a 400, not a 500.
var ErrUnknownAccount = errors.New("names no llm account")

// DefaultUpstream answers when no route is configured: straight to Anthropic,
// byte-for-byte what the pane would have done with no proxy at all.
const DefaultUpstream = "https://api.anthropic.com"

// CodexUpstream is where a codex account's traffic goes: the ChatGPT backend
// the codex CLI itself defaults to (its chatgpt_base_url). The proxied paths
// land under it — /models, /responses, never the Anthropic dialect.
const CodexUpstream = "https://chatgpt.com/backend-api"

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
		out = append(out, RedactedAccount{
			Name: a.Name, Kind: a.Kind, BaseURL: a.BaseURL,
			APIKeySet: a.APIKey != "", ModelAliases: a.ModelAliases,
		})
	}
	return out, route, nil
}

// PaneRoutes returns the per-pane overrides (paneID → account name). Same
// unreadable-is-not-empty rule as Accounts.
func (s *Service) PaneRoutes() (map[string]string, error) {
	raw, err := s.store.GetSetting(settingPaneRoutes)
	if err != nil {
		return nil, fmt.Errorf("llm pane routes unreadable: %w", err)
	}
	if raw == "" {
		return map[string]string{}, nil
	}
	var routes map[string]string
	if err := json.Unmarshal([]byte(raw), &routes); err != nil {
		return nil, fmt.Errorf("llm pane routes corrupt: %w", err)
	}
	return routes, nil
}

// SetPaneRoute points one pane at an account by name; "" clears the override
// so the pane follows the global route again. A name matching no account is
// ErrUnknownAccount.
func (s *Service) SetPaneRoute(paneID, route string) error {
	route = strings.TrimSpace(route)
	if route != "" {
		accs, err := s.Accounts()
		if err != nil {
			return err
		}
		if findAccount(accs, route) == nil {
			return fmt.Errorf("pane route %q %w", route, ErrUnknownAccount)
		}
	}
	routes, err := s.PaneRoutes()
	if err != nil {
		return fmt.Errorf("refusing to write llm pane routes: %w", err)
	}
	if route == "" {
		delete(routes, paneID)
	} else {
		routes[paneID] = route
	}
	return s.writePaneRoutes(routes)
}

func (s *Service) writePaneRoutes(routes map[string]string) error {
	b, err := json.Marshal(routes)
	if err != nil {
		return err
	}
	return s.store.SetSetting(settingPaneRoutes, string(b))
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
		switch a.Kind {
		case "anthropic", "openai":
		case "codex":
			if a.APIKey != "" {
				return fmt.Sprintf("llm account %q: codex accounts pass the harness's own ChatGPT login through and hold no key — delete the account and re-add it as codex", a.Name)
			}
		default:
			return fmt.Sprintf("llm account %q: unknown kind %q (anthropic, openai, or codex)", a.Name, a.Kind)
		}
		u, err := url.Parse(a.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Sprintf("llm account %q: baseURL must be an http(s) URL", a.Name)
		}
		if a.APIKey == "" && !passthroughHostAllowed(a, u.Hostname()) {
			return fmt.Sprintf("llm account %q has no api key, so each pane's own login token would be forwarded to it — that is only allowed to api.anthropic.com, chatgpt.com (codex accounts), localhost, or a private-network IP", a.Name)
		}
		for _, al := range a.ModelAliases {
			if strings.TrimSpace(al.From) == "" || strings.TrimSpace(al.To) == "" {
				return fmt.Sprintf("llm account %q: model alias with an empty side", a.Name)
			}
		}
	}
	return ""
}

// cgnat is the tailnet address range (100.64.0.0/10) — machines on the user's
// own tailnet count as private for pass-through purposes.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// passthroughHostAllowed limits where a keyless account may forward the
// pane's own credentials: Anthropic itself, or hosts the user plausibly owns.
// A codex account may additionally point at chatgpt.com — the only place its
// bearer means anything (a misrouted CLAUDE pane is kept off it separately:
// the handler refuses Anthropic-dialect requests on codex accounts, so a
// Claude login never rides this allowance). Hostnames other than localhost
// are refused outright rather than resolved — DNS at validation time proves
// nothing about DNS at request time.
func passthroughHostAllowed(a Account, hostname string) bool {
	if a.Kind == "codex" && hostname == "chatgpt.com" {
		return true
	}
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
		next := merged(old, *accs)
		b, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if err := s.store.SetSetting(settingAccounts, string(b)); err != nil {
			return err
		}
		// Removing an account releases the panes pointed at it back to the
		// global route, in the same write. A dangling pane override would
		// otherwise 502 that pane until someone found why (Reject guards the
		// global route this way; panes are too many to hold hostage).
		if err := s.prunePaneRoutes(next); err != nil {
			return err
		}
	}
	if route != nil {
		return s.store.SetSetting(settingRoute, strings.TrimSpace(*route))
	}
	return nil
}

// PaneStatus reports a pane's routing for the lenses: the explicit override
// ("" when the pane follows the global route) and the name of the account
// actually answering right now.
func (s *Service) PaneStatus(paneID string) (explicit, effective string, err error) {
	routes, err := s.PaneRoutes()
	if err != nil {
		return "", "", err
	}
	acct, err := s.resolve(paneID)
	if err != nil {
		return "", "", err
	}
	return routes[paneID], acct.Name, nil
}

// prunePaneRoutes drops pane overrides naming accounts that no longer exist.
func (s *Service) prunePaneRoutes(accs []Account) error {
	routes, err := s.PaneRoutes()
	if err != nil {
		return err
	}
	changed := false
	for pane, name := range routes {
		if findAccount(accs, name) == nil {
			delete(routes, pane)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.writePaneRoutes(routes)
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
		// A codex account has exactly one sensible upstream; an empty baseURL
		// means it, so creating one is just a name and a kind.
		if a.Kind == "codex" && a.BaseURL == "" {
			a.BaseURL = CodexUpstream
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

// AccountNameForKind returns the first configured account of the given kind —
// the pairing lookup for a harness that can only talk to one kind of upstream
// (codex → a ChatGPT-backed account). One account per kind is the intended
// setup; with several, first wins and a pane route override picks others.
func (s *Service) AccountNameForKind(kind string) (string, error) {
	accs, err := s.Accounts()
	if err != nil {
		return "", err
	}
	for _, a := range accs {
		if a.Kind == kind {
			return a.Name, nil
		}
	}
	return "", fmt.Errorf("no %s llm account is configured — add one under settings, Models", kind)
}

// KindOf reports the kind of a named account ("" when the name matches none).
func (s *Service) KindOf(name string) (string, error) {
	accs, err := s.Accounts()
	if err != nil {
		return "", err
	}
	if a := findAccount(accs, name); a != nil {
		return a.Kind, nil
	}
	return "", nil
}

func findAccount(accs []Account, name string) *Account {
	for i := range accs {
		if accs[i].Name == name {
			return &accs[i]
		}
	}
	return nil
}

// resolve picks the account answering for a pane right now: the pane's own
// override, else the global route, else the Anthropic pass-through. Any
// failure to read the truth is a loud error, never a silent default: a proxy
// that guesses where to send tokens is worse than one that refuses.
func (s *Service) resolve(paneID string) (Account, error) {
	route, err := s.routeFor(paneID)
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
	// SetPaneRoute validation and Apply's prune should make this unreachable;
	// if the registry was edited out from under us, failing loudly beats
	// silently burning tokens somewhere else.
	return Account{}, fmt.Errorf("llm route %q names no account", route)
}

// routeFor is the route name resolve acts on: pane override beats global.
func (s *Service) routeFor(paneID string) (string, error) {
	routes, err := s.PaneRoutes()
	if err != nil {
		return "", err
	}
	if name, ok := routes[paneID]; ok {
		return name, nil
	}
	return s.Route()
}
