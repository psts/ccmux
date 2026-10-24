package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Complete is a short one-shot request the daemon makes for itself (a
// repo's "owns" line, say), on the same small model and the same claude
// subscription tokens CheckAccount asks with. It tries each claude account
// holding a setup-token in turn and returns the first answer's text. It
// records nothing about the accounts: a refusal here is as likely to be
// about this request as about the account, the same reasoning as a check.
func (s *Service) Complete(ctx context.Context, prompt string, maxTokens int) (string, error) {
	accs, err := s.Accounts()
	if err != nil {
		return "", err
	}
	var errs []error
	for _, a := range accs {
		if a.Kind != "claude" || a.APIKey == "" {
			continue
		}
		text, err := completeWith(ctx, a, prompt, maxTokens)
		if err == nil {
			return text, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", a.Name, err))
	}
	if len(errs) == 0 {
		return "", errors.New("no claude account with a setup-token to ask with")
	}
	return "", errors.Join(errs...)
}

var completeClient = &http.Client{Timeout: 60 * time.Second}

func completeWith(ctx context.Context, a Account, prompt string, maxTokens int) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":      probeModel,
		"max_tokens": maxTokens,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(a.BaseURL, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	applyAuth(req, a)
	resp, err := completeClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("answered %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return replyText(resp.Body)
}

// replyText is the text blocks of a Messages answer, joined.
func replyText(r io.Reader) (string, error) {
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("unreadable answer: %w", err)
	}
	var b strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return strings.TrimSpace(b.String()), nil
}
