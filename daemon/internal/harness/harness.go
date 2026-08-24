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
}

const settingHarnesses = "harnesses"

// Builtin is the harness every daemon has without configuration.
const Builtin = "claude"

type Service struct {
	store Store
	// claudeCommand resolves the built-in claude harness's command — wired to
	// the manager's DefaultStartupCommand so the existing setting keeps working.
	claudeCommand func() string
}

func New(store Store, claudeCommand func() string) *Service {
	return &Service{store: store, claudeCommand: claudeCommand}
}

// List returns every harness, built-in claude first unless a user entry named
// "claude" overrides it. Unreadable is not empty (see llmproxy.Accounts): a
// read failure is an error, never a silently shorter list.
func (s *Service) List() ([]Harness, error) {
	user, err := s.userHarnesses()
	if err != nil {
		return nil, err
	}
	for _, h := range user {
		if h.Name == Builtin {
			return user, nil
		}
	}
	builtin := Harness{Name: Builtin, Icon: "✳", Command: s.claudeCommand(), Autoconfirm: true}
	return append([]Harness{builtin}, user...), nil
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
