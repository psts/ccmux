package manager

import "encoding/json"

// opencodeConfigContent is the JSON opencode reads from OPENCODE_CONFIG_CONTENT
// — a config layer that overrides the project's opencode.json. It does three
// things for a hosted pane:
//
//   - points the anthropic provider at the pane's ccmux proxy (the SDK appends
//     /messages to baseURL, so the pane URL gets a /v1 suffix) with a
//     placeholder key, because opencode enables a provider only when it has
//     one; the proxy swaps the placeholder for the routed account's credential;
//   - turns snapshots off: opencode's per-turn file-change snapshots rewrite
//     the conversation prefix every time a NEW file appears, which halved the
//     prompt-cache hit rate in the 2026-09-05 spike (docs/agents-plan.md);
//   - lists the Meridian opencode plugin when Meridian is installed, so a
//     meridian-routed pane's title/summary agents are told apart from its
//     primary agent.
func opencodeConfigContent(paneProxyURL, pluginPath string) string {
	cfg := map[string]any{
		"provider": map[string]any{
			"anthropic": map[string]any{
				"options": map[string]any{
					"baseURL": paneProxyURL + "/v1",
					"apiKey":  "ccmux",
				},
			},
		},
		"snapshot": false,
	}
	if pluginPath != "" {
		cfg["plugin"] = []string{pluginPath}
	}
	b, _ := json.Marshal(cfg) // only strings, bools and maps: cannot fail
	return string(b)
}

// opencodePlugin resolves the Meridian plugin path through the wired hook.
func (m *Manager) opencodePlugin() string {
	if m.OpencodePlugin == nil {
		return ""
	}
	return m.OpencodePlugin()
}
