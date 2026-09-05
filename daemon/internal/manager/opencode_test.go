package manager

import (
	"encoding/json"
	"testing"
)

func TestOpencodeConfigContent(t *testing.T) {
	var cfg struct {
		Provider struct {
			Anthropic struct {
				Options struct {
					BaseURL string `json:"baseURL"`
					APIKey  string `json:"apiKey"`
				} `json:"options"`
			} `json:"anthropic"`
		} `json:"provider"`
		Snapshot *bool    `json:"snapshot"`
		Plugin   []string `json:"plugin"`
	}
	raw := opencodeConfigContent("http://127.0.0.1:7900/llm/pane/p1", "")
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	if cfg.Provider.Anthropic.Options.BaseURL != "http://127.0.0.1:7900/llm/pane/p1/v1" {
		t.Errorf("baseURL %q", cfg.Provider.Anthropic.Options.BaseURL)
	}
	if cfg.Provider.Anthropic.Options.APIKey == "" {
		t.Error("provider needs a placeholder key to be enabled")
	}
	if cfg.Snapshot == nil || *cfg.Snapshot {
		t.Error("snapshot must be explicitly false")
	}
	if cfg.Plugin != nil {
		t.Errorf("no plugin without Meridian, got %v", cfg.Plugin)
	}
	raw = opencodeConfigContent("http://h/llm/pane/p1", "/x/plugin/meridian.ts")
	json.Unmarshal([]byte(raw), &cfg)
	if len(cfg.Plugin) != 1 || cfg.Plugin[0] != "/x/plugin/meridian.ts" {
		t.Errorf("plugin list %v", cfg.Plugin)
	}
}
