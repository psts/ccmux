package harness

import (
	"encoding/json"
	"fmt"
	"strings"
)

const settingHarnessRules = "harness_rules"

// Rule maps a folder subtree to the harness a lens should PRESELECT for new
// workspaces there: a rule for ~/Work/Coding/ChartLabs covers every repo under
// it, and the longest matching prefix wins. Rules only suggest — nothing
// auto-starts from one (see the empty-with-preselect model in the lenses).
type Rule struct {
	PathPrefix string `json:"pathPrefix"`
	Harness    string `json:"harness"`
}

// Rules returns the configured per-folder rules. Unreadable is not empty
// (same stance as List): a read failure is an error, never a silently blank
// editor that would wipe the rules on its next save.
func (s *Service) Rules() ([]Rule, error) {
	raw, err := s.store.GetSetting(settingHarnessRules)
	if err != nil {
		return nil, fmt.Errorf("harness rules unreadable: %w", err)
	}
	if raw == "" {
		return []Rule{}, nil
	}
	var rules []Rule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return nil, fmt.Errorf("harness rules corrupt: %w", err)
	}
	return rules, nil
}

// SetRules persists the rules, dropping rows missing either side (half-filled
// editor rows, not meaningful rules). A rule may name a harness that no longer
// exists — resolution degrades to the builtin (see PreselectFor) rather than
// an editor refusing to save.
func (s *Service) SetRules(rules []Rule) error {
	kept := make([]Rule, 0, len(rules))
	for _, r := range rules {
		r.PathPrefix = strings.TrimRight(strings.TrimSpace(r.PathPrefix), "/")
		r.Harness = strings.TrimSpace(r.Harness)
		if r.PathPrefix != "" && r.Harness != "" {
			kept = append(kept, r)
		}
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	return s.store.SetSetting(settingHarnessRules, string(b))
}

// PreselectFor names the harness a lens should preselect for a new workspace
// at repoPath: the longest matching folder rule, verified to still name an
// existing harness. Any miss — no rule, a dangling name, an unreadable store —
// returns Builtin: this is a suggestion, never a spawn path, so it degrades
// instead of erroring.
func (s *Service) PreselectFor(repoPath string) string {
	rules, err := s.Rules()
	if err != nil {
		return Builtin
	}
	repoPath = strings.TrimRight(repoPath, "/")
	best, bestLen := "", -1
	for _, r := range rules {
		if len(r.PathPrefix) <= bestLen {
			continue
		}
		if repoPath == r.PathPrefix || strings.HasPrefix(repoPath, r.PathPrefix+"/") {
			best, bestLen = r.Harness, len(r.PathPrefix)
		}
	}
	if bestLen < 0 {
		return Builtin
	}
	if _, err := s.Resolve(best); err != nil {
		return Builtin
	}
	return best
}
