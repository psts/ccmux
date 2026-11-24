package memtidy

import (
	"encoding/json"
	"os"
)

// DreamLive says whether Claude Code's own auto-dream would run, by the
// rule in its code (2.1.331): the feature tengu_onyx_plover must be
// enabled or available on Anthropic's side (cached in ~/.claude.json), and
// then the user setting autoDreamEnabled decides, defaulting to enabled.
// Anything unreadable counts as off: this tidy-up then runs.
func DreamLive(claudeJSON, settingsJSON string) bool {
	var cfg struct {
		Features map[string]json.RawMessage `json:"cachedGrowthBookFeatures"`
	}
	if !readJSON(claudeJSON, &cfg) {
		return false
	}
	var flag struct{ Enabled, Available bool }
	if raw, ok := cfg.Features["tengu_onyx_plover"]; !ok || json.Unmarshal(raw, &flag) != nil {
		return false
	}
	if !flag.Enabled && !flag.Available {
		return false
	}
	var settings struct {
		AutoDream *bool `json:"autoDreamEnabled"`
	}
	readJSON(settingsJSON, &settings)
	if settings.AutoDream != nil {
		return *settings.AutoDream
	}
	return flag.Enabled
}

func readJSON(path string, v any) bool {
	b, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(b, v) == nil
}
