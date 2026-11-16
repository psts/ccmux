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
//
//	1  hello, prompts by inbox id, delivered, cards, numbered events
//	2  the snapshot route and the boot id on the event stream's opening
//	   line (the thread page's relay), images on a prompt, and the fuller
//	   translation the page draws (diffs, to-dos, images, what the session
//	   says of itself, each turn's cost)
const Protocol = 2

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

// Prompt sends one inbox prompt, with the names of the images that go with
// it (files in the session's media folder, which the runner reads). sent is
// false when the runner already has that id, which is not an error.
func (c *Client) Prompt(ctx context.Context, id, text string, images []string) (sent bool, err error) {
	var out struct {
		Sent bool `json:"sent"`
	}
	body := map[string]any{"id": id, "text": text}
	if len(images) > 0 {
		body["images"] = images
	}
	err = c.do(ctx, http.MethodPost, "/prompt", body, &out)
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

// CardReply answers one pending card: Kind "permission" with Allow, or
// Kind "question" with Answer, the one text put under every question on
// the card.
type CardReply struct {
	Kind   string
	ID     string
	Allow  bool
	Answer string
}

// ReplyCard answers a pending card. answered is false when the runner
// holds no such card any more (answered elsewhere first, or it died with
// an earlier runner process): the first answer wins, and that is not an
// error. A permission is allowed once, never as a rule.
func (c *Client) ReplyCard(ctx context.Context, r CardReply) (answered bool, err error) {
	pending, err := c.Cards(ctx)
	if err != nil {
		return false, err
	}
	for _, card := range pending[r.Kind] {
		if card.ID != r.ID {
			continue
		}
		path, body := "/permission/"+r.ID+"/reply", any(map[string]string{"reply": permissionReply(r.Allow)})
		if r.Kind == "question" {
			path, body = "/question/"+r.ID+"/reply", map[string]any{"answers": answersFor(card.Body, r.Answer)}
		}
		return true, c.do(ctx, http.MethodPost, path, body, nil)
	}
	return false, nil
}

func permissionReply(allow bool) string {
	if allow {
		return "once"
	}
	return "reject"
}

// answersFor puts the one answer under each question the card asks.
func answersFor(card json.RawMessage, answer string) [][]string {
	var head struct {
		Questions []json.RawMessage `json:"questions"`
	}
	_ = json.Unmarshal(card, &head)
	out := make([][]string, max(len(head.Questions), 1))
	for i := range out {
		out[i] = []string{answer}
	}
	return out
}
