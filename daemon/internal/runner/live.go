package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// What a person does to a running session from its page beyond prompting
// and answering cards: switch its permission mode (Shift+Tab in Claude
// Code), and stop one of its background tasks (a dev server, a monitor).
// The runner does both on its live query (claudeserve/agent.mjs).

// ErrRunnerSaidNo is an action the runner refused, in its words (a mode it
// does not offer, no running session).
var ErrRunnerSaidNo = errors.New("refused")

// SetMode switches the session's permission mode until it ends or starts
// again.
func (c *Client) SetMode(ctx context.Context, mode string) error {
	return c.action(ctx, "/mode", map[string]string{"mode": mode})
}

// StopTask stops one of the session's background tasks.
func (c *Client) StopTask(ctx context.Context, taskID string) error {
	return c.action(ctx, "/task/"+url.PathEscape(taskID)+"/stop", struct{}{})
}

// action posts one page action; a 409 is the runner's "no" in its words.
func (c *Client) action(ctx context.Context, path string, in any) error {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://runner"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	var said struct{ Error string }
	if res.StatusCode == http.StatusConflict && json.Unmarshal(body, &said) == nil && said.Error != "" {
		return fmt.Errorf("%w: %s", ErrRunnerSaidNo, said.Error)
	}
	return fmt.Errorf("runner POST %s: HTTP %d: %s", path, res.StatusCode, bytes.TrimSpace(body))
}
