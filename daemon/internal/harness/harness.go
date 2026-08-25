// Package harness makes "what runs in a pane" a first-class object instead of
// a raw startup command. A harness is a named way of working in a workspace —
// claude code today; pi, hermes, opencode, or a plain shell tomorrow — with
// the metadata the lenses and the manager need: what to type into the shell,
// what icon to show on the picker button, and whether the startup-prompt
// autoconfirm watcher should be armed for it.
//
// The list lives in the settings table so every lens reads the same one. The
// "claude" harness is built in — its command is the daemon's configured
// startup command, so the existing default_startup_command setting and
// per-folder rules keep meaning what they always did — and a user entry named
// "claude" overrides the built-in wholesale.
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
	// Accounts names the llm accounts this harness may be paired with (empty =
	// any). Declarative for now: the pickers will read it once pairing exists.
	Accounts []string `json:"accounts,omitempty"`
	// Source labels where a listed entry came from — "builtin", "detected"
	// (binary found on this host), or "" for a user-configured entry. Stamped
	// by List for the editors; stripped on Apply.
	Source string `json:"source,omitempty"`
}

const settingHarnesses = "harnesses"

// Builtin is the harness every daemon has without configuration.
const Builtin = "claude"

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
	// claudeCommand resolves the built-in claude harness's command — wired to
	// the manager's DefaultStartupCommand so the existing setting keeps working.
	claudeCommand func() string
	// lookPath answers "is this program installed here" (exec.LookPath in
	// production; tests inject).
	lookPath func(string) (string, error)
}

func New(store Store, claudeCommand func() string) *Service {
	return &Service{store: store, claudeCommand: claudeCommand, lookPath: lookPathUserBins}
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
		out = append(out, Harness{Name: Builtin, Icon: "✳", Command: s.claudeCommand(), Autoconfirm: true, Source: "builtin"})
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
