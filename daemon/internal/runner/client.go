package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Protocol is the runner surface this daemon speaks (RUNNER_PROTOCOL in
// claudeserve/runnerstate.mjs). A runner saying another is restarted on
// the installed sidecar between turns.
const Protocol = 1

// Hello is what a runner says at connect.
type Hello struct {
	Protocol   int    `json:"protocol"`
	Boot       string `json:"boot"`
	Session    string `json:"session"`
	Busy       bool   `json:"busy"`
	TurnOpen   bool   `json:"turnOpen"`
	Seq        int64  `json:"seq"`
	Model      string `json:"model"`
	ClaudeCode string `json:"claudeCode"`
	SDK        string `json:"sdk"`
}

// Card is a pending permission or question as the runner lists it; only
// the id is read, the rest is kept whole.
type Card struct {
	ID   string
	Body json.RawMessage
}

// Client talks to one runner over its unix socket.
type Client struct {
	http *http.Client
}

// Dial makes a client for the runner on socket. Nothing connects until a
// call.
func Dial(socket string) *Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	return &Client{http: &http.Client{Transport: tr, Timeout: 15 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://runner"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("runner %s %s: HTTP %d: %s", method, path, res.StatusCode, bytes.TrimSpace(msg))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func (c *Client) Hello(ctx context.Context) (Hello, error) {
	var h Hello
	err := c.do(ctx, http.MethodGet, "/hello", nil, &h)
	return h, err
}

// Prompt sends one inbox prompt. sent is false when the runner already has
// that id, which is not an error.
func (c *Client) Prompt(ctx context.Context, id, text string) (sent bool, err error) {
	var out struct {
		Sent bool `json:"sent"`
	}
	err = c.do(ctx, http.MethodPost, "/prompt", map[string]string{"id": id, "text": text}, &out)
	return out.Sent, err
}

// Delivered is the subset of ids Claude has.
func (c *Client) Delivered(ctx context.Context, ids []string) ([]string, error) {
	var out struct {
		Delivered []string `json:"delivered"`
	}
	err := c.do(ctx, http.MethodPost, "/delivered", map[string]any{"ids": ids}, &out)
	return out.Delivered, err
}

// Cards lists the pending permissions and questions, as kind → cards.
func (c *Client) Cards(ctx context.Context) (map[string][]Card, error) {
	out := map[string][]Card{}
	for kind, path := range map[string]string{"permission": "/permission", "question": "/question"} {
		var raw []json.RawMessage
		if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			var head struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(r, &head) == nil && head.ID != "" {
				out[kind] = append(out[kind], Card{ID: head.ID, Body: r})
			}
		}
	}
	return out, nil
}
