package llmproxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// UpstreamModels asks a named account's upstream what models it serves, via
// the OpenAI-style GET /v1/models that Ollama, OpenRouter, and Anthropic all
// answer with the same {data:[{id}]} shape. This is what lets the settings
// Accounts tab offer a real model picker for alias targets instead of a
// blind text field. The account's own credential applies, same as proxied
// traffic.
func (s *Service) UpstreamModels(name string) ([]string, error) {
	accs, err := s.Accounts()
	if err != nil {
		return nil, err
	}
	a := findAccount(accs, name)
	if a == nil {
		return nil, fmt.Errorf("account %q %w", name, ErrUnknownAccount)
	}
	req, err := http.NewRequest("GET", a.BaseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	applyAuth(req, *a)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s is not answering: %w", a.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d to /v1/models", a.Name, resp.StatusCode)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("%s: unexpected /v1/models shape: %w", a.Name, err)
	}
	out := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}
