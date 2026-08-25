// Package harness makes "what runs in a pane" a first-class object instead of
// a raw startup command. A harness is a named way of working in a workspace —
// claude code today; pi, hermes, opencode, or a plain shell tomorrow — with
// the metadata the lenses and the manager need: what to type into the shell,
// what icon to show on the picker button, and whether the startup-prompt
// autoconfirm watcher should be armed for it.
//
// The list lives in the settings table so every lens reads the same one, and
// it is the single source of truth for what a harness runs: the "claude"
// harness is built in with FallbackClaudeCommand, and a user entry named
// "claude" overrides the built-in wholesale. Per-folder rules (rules.go) name
// a harness to preselect; migrate.go converts the retired raw-command
// settings that predate this model.
package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Store is the slice of the registry the service needs (satisfied by
// *store.SQLite).
type Store interface {
	GetSetting(key string) (string, error)
	SetSetting(key, value string) error
}

// Harness is one named way of working in a pane.
type Harness struct {
	Name string `json:"name"`
	Icon string `json:"icon,omitempty"`
	// Command is typed into the pane's shell to start the harness.
	Command string `json:"command"`
	// Autoconfirm arms the watcher that presses Enter through a harness's
	// startup prompts (built for claude's trust prompt; other harnesses opt in
	// when their prompts are known to be safe to confirm blind).
	Autoconfirm bool `json:"autoconfirm"`
	// AccountKinds names the llm account KINDS this harness can talk to —
	// compatibility is a protocol property, so it keys on kind, never on
	// account names (those rot as accounts come and go). Empty inherits the
	// registry default for this NAME (see defaultAccountKinds), so a user
	// override of e.g. the codex command keeps codex's pairing; a name with
	// no default means any kind except codex (see api.kindAllowed). The
	// spawn pairing, the route guard, and the pane route picker all read
	// this one declaration.
	AccountKinds []string `json:"accountKinds,omitempty"`
	// Source labels where a listed entry came from — "builtin", "detected"
	// (binary found on this host), or "" for a user-configured entry. Stamped
	// by List for the editors; stripped on Apply.
	Source string `json:"source,omitempty"`
}

const settingHarnesses = "harnesses"

// defaultAccountKinds is what AccountKinds resolves to when an entry leaves
// it empty, keyed by harness name: claude speaks the Anthropic dialect,
// which a keyed org account, an injected subscription token, or a
// bearer-auth Anthropic-compatible gateway (openai kind — OpenRouter, a
// local server) all serve; codex speaks OpenAI's Responses dialect, which
// only a codex account's upstream answers. Absent names (pi, opencode,
// custom entries) mean any kind except codex (see api.kindAllowed).
var defaultAccountKinds = map[string][]string{
	Builtin: {"anthropic", "openai", "claude"},
	"codex": {"codex"},
}

// Builtin is the harness every daemon has without configuration.
const Builtin = "claude"

// FallbackClaudeCommand is what the built-in claude harness runs when no user
// entry named "claude" overrides it: a peers-enabled claude, so ccmux-created
// sessions get live channel push out of the box (plain `claude` would load the
// peer tools but silently drop pushed messages).
//
// `env -u TMUX` is a cleanup on the clipboard path, NOT what makes copies reach
// the lens. Measured against Claude Code 2.1.224: it writes OSC 52 on every
// copy regardless of $TMUX, and `tmux load-buffer` / pbcopy / xclip are extra
// writes rather than alternatives to it. tmux forwards a pane's OSC 52 to its
// client inside %output either way, so copies already reach the lens with $TMUX
// set — verified end to end against a real hosted pane and an attached lens.
//
// What unsetting it buys: with $TMUX set, claude emits the sequence twice (once
// plain, once wrapped in a tmux DCS passthrough) and awaits `tmux load-buffer`
// for up to 4s before emitting at all. Unsetting drops both.
//
// What it costs: claude's own subprocesses no longer see $TMUX, so a `tmux`
// command one of them runs does not find the ccmux server. TMUX_PANE survives,
// so anything keyed on the pane id is unaffected. In a ccmux pane the daemon
// owns tmux, so that is a deliberate trade.
const FallbackClaudeCommand = "env -u TMUX claude --dangerously-load-development-channels server:claude-peers"

// known are the harnesses the daemon recognizes on sight: if the program is
// installed on this host, the harness exists — settings and pickers included,
// zero configuration. A user entry with the same name overrides the detected
// default wholesale.
var known = []Harness{
	{Name: "pi", Icon: "π", Command: "pi"},
	{Name: "opencode", Icon: "⌬", Command: "opencode"},
	{Name: "aider", Icon: "✎", Command: "aider"},
	{Name: "codex", Icon: "◎", Command: codexCommand},
}

// codexCommand starts codex with its model calls pointed at the pane's llm
// proxy URL, like every other harness's. Codex has no base-URL env var; the
// -c overrides define a provider that keeps ChatGPT subscription auth
// (requires_openai_auth — the proxy passes the bearer through untouched, see
// llmproxy's "codex" account kind) but streams over HTTP instead of codex's
// default websocket, which connects straight to chatgpt.com and would bypass
// the proxy. Login state stays codex's own (~/.codex/auth.json, rotating
// refresh tokens — there is nothing durable the daemon could hold instead).
const codexCommand = `codex -c model_provider=ccmux` +
	` -c model_providers.ccmux.name=ccmux` +
	` -c "model_providers.ccmux.base_url=$ANTHROPIC_BASE_URL"` +
	` -c model_providers.ccmux.wire_api=responses` +
	` -c model_providers.ccmux.requires_openai_auth=true` +
	` -c model_providers.ccmux.supports_websockets=false`

type Service struct {
	store Store
	// lookPath answers "is this program installed here" (exec.LookPath in
	// production; tests inject).
	lookPath func(string) (string, error)
}

func New(store Store) *Service {
	return &Service{store: store, lookPath: lookPathUserBins}
}

// lookPathUserBins is exec.LookPath widened with ~/.local/bin. Panes run
// LOGIN shells whose profile puts user bins on PATH, but the daemon runs
// under systemd with the bare system PATH — a harness the pane can start
// would otherwise be invisible to detection.
func lookPathUserBins(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(home, ".local", "bin", name)
	if info, err := os.Stat(p); err == nil && !info.IsDir() {
		return p, nil
	}
	return "", fmt.Errorf("%s: not found", name)
}

// List returns every harness: built-in claude first (unless a user entry
// named "claude" overrides it), then user entries, then detected known
// programs not already named. Unreadable is not empty (see llmproxy.Accounts):
// a read failure is an error, never a silently shorter list.
func (s *Service) List() ([]Harness, error) {
	user, err := s.userHarnesses()
	if err != nil {
		return nil, err
	}
	named := map[string]bool{}
	out := make([]Harness, 0, len(user)+len(known)+1)
	for _, h := range user {
		named[h.Name] = true
	}
	if !named[Builtin] {
		out = append(out, Harness{Name: Builtin, Icon: "✳", Command: FallbackClaudeCommand, Autoconfirm: true, Source: "builtin"})
	}
	out = append(out, user...)
	for _, k := range known {
		if named[k.Name] {
			continue
		}
		if _, err := s.lookPath(k.Name); err != nil {
			continue
		}
		k.Source = "detected"
		out = append(out, k)
	}
	for i := range out {
		// Stamp the resolved pairing, like Source: an entry that says nothing
		// gets its name's default, so overriding a builtin/detected harness's
		// command never silently drops what it can pair with.
		if len(out[i].AccountKinds) == 0 {
			out[i].AccountKinds = defaultAccountKinds[out[i].Name]
		}
	}
	return out, nil
}

// Resolve returns the named harness, or an error naming what is missing —
// the spawn path must not quietly run the wrong thing.
func (s *Service) Resolve(name string) (Harness, error) {
	list, err := s.List()
	if err != nil {
		return Harness{}, err
	}
	for _, h := range list {
		if h.Name == name {
			return h, nil
		}
	}
	return Harness{}, fmt.Errorf("unknown harness %q", name)
}

// Reject validates a proposed user list before anything persists; "" accepts.
func (s *Service) Reject(hs []Harness) string {
	seen := map[string]bool{}
	for _, h := range hs {
		name := strings.TrimSpace(h.Name)
		if name == "" {
			return "harness with an empty name"
		}
		if seen[name] {
			return fmt.Sprintf("duplicate harness %q", name)
		}
		seen[name] = true
		if strings.TrimSpace(h.Command) == "" {
			return fmt.Sprintf("harness %q has no command", name)
		}
		for _, k := range h.AccountKinds {
			// An unknown kind is a rule that matches nothing — the harness
			// would refuse every account and nothing would say why.
			switch strings.TrimSpace(k) {
			case "anthropic", "openai", "claude", "codex":
			default:
				return fmt.Sprintf("harness %q: unknown account kind %q (anthropic, openai, claude, or codex)", name, k)
			}
		}
	}
	return ""
}

// Apply persists the user list Reject accepted (normalized). The whole list
// replaces, like the identity aliases — an entry you couldn't remove by
// resubmitting without it would be a trap.
func (s *Service) Apply(hs []Harness) error {
	next := make([]Harness, 0, len(hs))
	for _, h := range hs {
		h.Name = strings.TrimSpace(h.Name)
		h.Command = strings.TrimSpace(h.Command)
		h.Source = "" // stamped by List, never stored
		kinds := make([]string, 0, len(h.AccountKinds))
		for _, k := range h.AccountKinds {
			if k = strings.TrimSpace(k); k != "" {
				kinds = append(kinds, k)
			}
		}
		h.AccountKinds = kinds
		next = append(next, h)
	}
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return s.store.SetSetting(settingHarnesses, string(b))
}

func (s *Service) userHarnesses() ([]Harness, error) {
	raw, err := s.store.GetSetting(settingHarnesses)
	if err != nil {
		return nil, fmt.Errorf("harnesses unreadable: %w", err)
	}
	if raw == "" {
		return []Harness{}, nil
	}
	var hs []Harness
	if err := json.Unmarshal([]byte(raw), &hs); err != nil {
		return nil, fmt.Errorf("harnesses corrupt: %w", err)
	}
	return hs, nil
}
