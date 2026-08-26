package harness

import (
	"encoding/json"
	"log"
	"slices"
	"strings"
)

// The retired settings this package's model replaced: a daemon-wide raw
// startup command (the built-in claude harness's command now serves that
// role) and per-folder rules holding raw commands (rules now name a harness).
const (
	legacySettingStartupCommand = "default_startup_command"
	legacySettingStartupRules   = "startup_rules"
)

// MigrateLegacyStartupSettings converts the retired raw-command settings into
// this package's model, then clears them — each key only after its converted
// form saved, so a failed save leaves the key in place and the next boot
// retries. Cleared keys make reruns no-ops.
func (s *Service) MigrateLegacyStartupSettings() error {
	if err := s.migrateDefaultCommand(); err != nil {
		return err
	}
	return s.migrateRules()
}

// migrateDefaultCommand turns a configured default startup command into a
// user "claude" harness override, preserving what claude spawns run. A value
// equal to the fallback preserves nothing — it just clears the key.
func (s *Service) migrateDefaultCommand() error {
	raw, err := s.store.GetSetting(legacySettingStartupCommand)
	if err != nil {
		return err
	}
	if raw == "" {
		return nil
	}
	cmd := strings.TrimSpace(raw)
	if cmd != "" && cmd != FallbackClaudeCommand {
		if err := s.seedClaudeOverride(cmd); err != nil {
			return err
		}
	}
	return s.store.SetSetting(legacySettingStartupCommand, "")
}

// seedClaudeOverride preserves the retired default command as a user claude
// entry — unless one already exists, in which case it already won wholesale
// and there is nothing left to preserve.
func (s *Service) seedClaudeOverride(cmd string) error {
	user, err := s.userHarnesses()
	if err != nil {
		return err
	}
	if slices.ContainsFunc(user, func(h Harness) bool { return h.Name == Builtin }) {
		return nil
	}
	return s.Apply(append(user, Harness{Name: Builtin, Icon: "✳", Command: cmd, Autoconfirm: true}))
}

// migrateRules converts raw-command folder rules to harness-name rules: an
// exact match against a harness's command wins (run after
// migrateDefaultCommand, so a rule repeating the old default maps to the new
// claude override), else a harness running the same PROGRAM (StartupProgram
// sees through env/VAR= wrappers and flags, so `env FOO=1 pi --fast` maps to
// the detected pi), else the rule is dropped with a log line — a raw command
// no harness runs has no place in the new model.
func (s *Service) migrateRules() error {
	raw, err := s.store.GetSetting(legacySettingStartupRules)
	if err != nil {
		return err
	}
	if raw == "" {
		return nil
	}
	// Rules already configured in the new model win outright: this branch is
	// only reachable when an earlier boot's migration failed halfway, and by
	// now the user may have built rules through the new editors — converting
	// again would overwrite them with the stale legacy set.
	if existing, err := s.Rules(); err != nil {
		return err
	} else if len(existing) > 0 {
		log.Printf("harness: discarding legacy startup rules — harness rules already configured")
		return s.store.SetSetting(legacySettingStartupRules, "")
	}
	var legacy []struct{ PathPrefix, Command string }
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
		log.Printf("harness: legacy startup rules corrupt, discarding: %v", err)
		return s.store.SetSetting(legacySettingStartupRules, "")
	}
	list, err := s.List()
	if err != nil {
		return err
	}
	converted := make([]Rule, 0, len(legacy))
	for _, lr := range legacy {
		name := ruleHarnessName(list, lr.Command)
		if name == "" {
			log.Printf("harness: dropping startup rule for %q — no harness runs %q", lr.PathPrefix, lr.Command)
			continue
		}
		converted = append(converted, Rule{PathPrefix: lr.PathPrefix, Harness: name})
	}
	if err := s.SetRules(converted); err != nil {
		return err
	}
	return s.store.SetSetting(legacySettingStartupRules, "")
}

// ruleHarnessName maps one legacy raw command to a harness name, strongest
// signal first: exact command match, same program, then claude for any
// claude-shaped command (covering a claude override whose command runs a
// different program). "" means no harness runs it.
func ruleHarnessName(list []Harness, cmd string) string {
	for _, h := range list {
		if h.Command == cmd {
			return h.Name
		}
	}
	if prog := StartupProgram(cmd); prog != "" {
		for _, h := range list {
			if StartupProgram(h.Command) == prog {
				return h.Name
			}
		}
	}
	if StartupProgram(cmd) == Builtin {
		return Builtin
	}
	return ""
}
