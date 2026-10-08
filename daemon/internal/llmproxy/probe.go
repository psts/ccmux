package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// A check asks Anthropic about one subscription account now, instead of
// waiting for traffic to rediscover it. It exists for the limit that lifts
// early: a manual reset frees the subscription, but the mark the last 429 left
// stands until the reset THAT answer named, so the proxy keeps skipping a
// usable account for hours.
//
// The check is one real, minimal Messages request. Its answer carries the same
// unified headers live traffic does, and it goes through observe, the writer
// every live response goes through, so state() stays the only rule and the row
// the lenses show afterwards is whatever that answer means.

// probeModel is the haiku name Claude Code's own background calls send (see
// compat_test.go), so a subscription is known to serve it. max_tokens 1 keeps
// the cost a sliver of quota on an open account; a limited one is refused
// before any work is done.
const probeModel = "claude-haiku-4-5-20251001"

// probeTimeout bounds the whole exchange, body included.
const probeTimeout = 20 * time.Second

var probeClient = &http.Client{
	Timeout: probeTimeout,
	// A redirect is not an answer about quota. Report it rather than follow it.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// ErrNotCheckable marks an account a check has no way to ask about.
var ErrNotCheckable = errors.New("cannot be checked")

// CheckAccount probes one claude account upstream and returns its status row
// as it stands after the answer.
//
// Only an answer about QUOTA is recorded: served (2xx) or limited. The
// asymmetry is the point. Anything else a probe gets back is at least as
// likely to be about the probe as about the account (a model this
// subscription does not serve, a header the upstream wants), and recording it
// is how a check would sideline a healthy account: a 401 marks unauthorized
// with no expiry, a transport error benches it for 30s. Those come back as an
// error naming what the upstream said, and the row stays as it was. A dead
// credential is still found, by the next live request.
func (s *Service) CheckAccount(ctx context.Context, name string) (AccountStatus, error) {
	a, err := s.checkable(name)
	if err != nil {
		return AccountStatus{}, err
	}
	resp, err := sendProbe(ctx, a)
	if err != nil {
		return AccountStatus{}, fmt.Errorf("%s is not answering, nothing recorded: %w", a.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && !limitResponse(resp) {
		err := probeRefused(a.Name, resp)
		log.Printf("llm: %v", err)
		return AccountStatus{}, err
	}
	log.Printf("llm: check of account %s answered %d", a.Name, resp.StatusCode)
	s.health.observe(a, resp)
	return s.statusFor(a), nil
}

// checkable resolves the account a check may ask about: a claude subscription
// holding its own setup-token. Other kinds either report no quota headers
// (Ollama, OpenRouter) or hold no credential to ask with: a keyless
// pass-through forwards the PANE's login, and a check has no pane.
func (s *Service) checkable(name string) (Account, error) {
	accs, err := s.Accounts()
	if err != nil {
		return Account{}, err
	}
	a := findAccount(accs, name)
	if a == nil {
		return Account{}, fmt.Errorf("account %q %w", name, ErrUnknownAccount)
	}
	if a.Kind != "claude" || a.APIKey == "" {
		return Account{}, fmt.Errorf("account %q %w: only a claude account with a setup-token reports its quota", name, ErrNotCheckable)
	}
	return *a, nil
}

// sendProbe sends the one-token request with the account's own token. The
// beta flag is the one Claude Code sends alongside a subscription bearer; that
// a request ccmux originates is accepted with it is NOT verified live, and a
// refusal records nothing (see CheckAccount).
func sendProbe(ctx context.Context, a Account) (*http.Response, error) {
	body, err := json.Marshal(map[string]any{
		"model":      probeModel,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "ok"}},
	})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(a.BaseURL, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	applyAuth(req, a)
	return probeClient.Do(req)
}

// probeRefused names what the upstream said, from an Anthropic-shaped error
// body when there is one, so the lens can show the reason rather than a bare
// status.
func probeRefused(name string, resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &parsed) == nil && parsed.Error.Message != "" {
		msg = parsed.Error.Message
	}
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	if msg == "" {
		return fmt.Errorf("%s answered the check with %d, nothing recorded", name, resp.StatusCode)
	}
	return fmt.Errorf("%s answered the check with %d, nothing recorded: %s", name, resp.StatusCode, msg)
}

// statusFor is one account's row, read under the lock Statuses holds.
func (s *Service) statusFor(a Account) AccountStatus {
	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	return statusRow(a, s.health.get(a.Name), s.health.now())
}
