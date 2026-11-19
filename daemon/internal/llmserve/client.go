package llmserve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"ccmux.dev/ccmuxd/internal/llmproxy"
)

// Client is the daemon's end of the service: the control reads
// (llmproxy.Remote), the health probe, and the forward that keeps the
// daemon's old /llm/pane address working.
type Client struct {
	base string
	// quick answers the settings page's reads: local, small, and not worth
	// holding a page for. slow is a check, which asks Anthropic and waits.
	quick, slow *http.Client
}

// NewClient talks to the service at base (http://127.0.0.1:<port>).
func NewClient(base string) *Client {
	return &Client{
		base:  base,
		quick: &http.Client{Timeout: 3 * time.Second},
		slow:  &http.Client{Timeout: 30 * time.Second},
	}
}

// URL is the service's base address, the one a pane's ANTHROPIC_BASE_URL is
// built on.
func (c *Client) URL() string { return c.base }

var _ llmproxy.Remote = (*Client)(nil)

func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	return h, c.call(ctx, c.quick, "GET", "/health", nil, &h)
}

func (c *Client) Statuses(ctx context.Context) ([]llmproxy.AccountStatus, error) {
	var sts []llmproxy.AccountStatus
	return sts, c.call(ctx, c.quick, "GET", "/ctl/statuses", nil, &sts)
}

func (c *Client) PaneOrders(ctx context.Context, paneIDs []string) (map[string][]string, map[string]string, error) {
	var out ordersAnswer
	if err := c.call(ctx, c.quick, "POST", "/ctl/orders", ordersRequest{Panes: paneIDs}, &out); err != nil {
		return nil, nil, err
	}
	// The local answer is never nil in either map, and the lenses are handed
	// these as JSON: null where {} was would be a change they did not ask for.
	if out.Orders == nil {
		out.Orders = map[string][]string{}
	}
	if out.Failures == nil {
		out.Failures = map[string]string{}
	}
	return out.Orders, out.Failures, nil
}

func (c *Client) PaneStatus(ctx context.Context, paneID string) (string, []string, error) {
	var out paneAnswer
	err := c.call(ctx, c.quick, "GET", "/ctl/panes/"+url.PathEscape(paneID), nil, &out)
	return out.Explicit, out.Order, err
}

func (c *Client) CheckAccount(ctx context.Context, name string) (llmproxy.AccountStatus, error) {
	var st llmproxy.AccountStatus
	return st, c.call(ctx, c.slow, "POST", "/ctl/accounts/"+url.PathEscape(name)+"/check", nil, &st)
}

// call is one control request. A refusal comes back as the service's own
// words, wrapped in the error it named so errors.Is still finds it.
func (c *Client) call(ctx context.Context, hc *http.Client, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("the llm proxy service at %s is not answering: %w", c.base, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("the llm proxy service's answer was cut short: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return refusal(resp.StatusCode, raw)
	}
	return json.Unmarshal(raw, out)
}

// refusal turns a failed control answer back into an error.
func refusal(status int, raw []byte) error {
	var we wireError
	if json.Unmarshal(raw, &we) != nil || we.Error == "" {
		return fmt.Errorf("the llm proxy service answered %d: %s", status, bytes.TrimSpace(raw))
	}
	switch we.Kind {
	case kindUnknownAccount:
		return remoteError{we.Error, llmproxy.ErrUnknownAccount}
	case kindNotCheckable:
		return remoteError{we.Error, llmproxy.ErrNotCheckable}
	}
	return remoteError{we.Error, nil}
}

// remoteError says what the service said, word for word (the daemon's API
// shows it as is), and is the error the service named.
type remoteError struct {
	msg string
	is  error
}

func (e remoteError) Error() string { return e.msg }
func (e remoteError) Unwrap() error { return e.is }

// Forward is the daemon's old pane address, kept answering: a pane opened
// before the proxy moved still has http://<daemon>/llm/pane/<id> in its
// environment and keeps it until it restarts, so the daemon passes those
// requests on. A reply streamed through here is still cut when the daemon
// restarts; a pane gets the benefit of the move at its next start.
//
// The request goes on exactly as it came: path and query untouched, so the
// service's own path guard (llmproxy's firstSegment) sees what the pane sent
// and not a cleaned-up copy that would hide a dot segment from it.
func (c *Client) Forward() http.Handler {
	target, err := url.Parse(c.base)
	if err != nil {
		target = &url.URL{} // fails the dial with a loggable error, like llmproxy's rewrite
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.Host = target.Host
		},
		// A token stream, same as the proxy itself.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // the harness hung up first
			}
			log.Printf("llm: forward to the proxy service at %s failed: %v", c.base, err)
			http.Error(w, "ccmux llm proxy: the proxy service is not answering (systemctl --user status ccmux-llm-proxy)", http.StatusBadGateway)
		},
	}
}
