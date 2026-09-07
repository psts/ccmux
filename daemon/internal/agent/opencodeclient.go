package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Opencode is the daemon's client for one running instance's HTTP server
// (the TUI serves it on --port). The chat view rides on it: history,
// prompts, aborts, permission replies and the event stream. Loopback only.
type Opencode struct {
	Base string
	HTTP *http.Client
}

// OpencodeAt is the client for the instance on port.
func OpencodeAt(port int) *Opencode {
	return &Opencode{Base: "http://127.0.0.1:" + strconv.Itoa(port), HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// OpencodeSession is one conversation of an instance; newest first from
// Sessions. Directory is the cwd it was opened in, which for an agent is
// its instance folder — the key that tells its sessions from a stray one.
type OpencodeSession struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Directory string `json:"directory"`
	Updated   int64  `json:"updated"`
}

// PermissionRequest is opencode asking a human before a tool runs: what
// (permission, patterns) for which tool call. Replies are once, always or
// reject.
type PermissionRequest struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"sessionID"`
	Permission string          `json:"permission"`
	Patterns   []string        `json:"patterns"`
	Always     []string        `json:"always,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	Tool       *struct {
		MessageID string `json:"messageID"`
		CallID    string `json:"callID"`
	} `json:"tool,omitempty"`
}

// QuestionRequest is opencode's question tool asking the human to choose:
// one or more questions, each with options (single or multiple choice, a
// free answer allowed when Custom). The reply is one list of chosen labels
// per question, in order.
type QuestionRequest struct {
	ID        string         `json:"id"`
	SessionID string         `json:"sessionID"`
	Questions []QuestionInfo `json:"questions"`
	Tool      *struct {
		MessageID string `json:"messageID"`
		CallID    string `json:"callID"`
	} `json:"tool,omitempty"`
}

type QuestionInfo struct {
	Question string           `json:"question"`
	Header   string           `json:"header"`
	Options  []QuestionOption `json:"options"`
	Multiple bool             `json:"multiple,omitempty"`
	Custom   bool             `json:"custom,omitempty"`
}

type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// OpencodeEvent is one frame of GET /event: its kind and the properties
// as opencode sent them, decoded by whoever knows the kind.
type OpencodeEvent struct {
	Type  string          `json:"type"`
	Props json.RawMessage `json:"properties"`
}

// Up says whether the server answers at all.
func (c *Opencode) Up(ctx context.Context) bool {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.Base+"/session", nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

// Sessions lists the instance's sessions, newest first.
func (c *Opencode) Sessions(ctx context.Context) ([]OpencodeSession, error) {
	var raw []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Directory string `json:"directory"`
		Time      struct {
			Updated int64 `json:"updated"`
		} `json:"time"`
	}
	if err := c.getJSON(ctx, "/session", &raw); err != nil {
		return nil, err
	}
	out := make([]OpencodeSession, 0, len(raw))
	for _, s := range raw {
		out = append(out, OpencodeSession{ID: s.ID, Title: s.Title, Directory: s.Directory, Updated: s.Time.Updated})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out, nil
}

// Messages is the session's conversation, normalized.
func (c *Opencode) Messages(ctx context.Context, sessionID string) ([]Turn, error) {
	body, err := c.get(ctx, "/session/"+sessionID+"/message")
	if err != nil {
		return nil, err
	}
	return NormalizeMessages(body)
}

// Prompt sends text as the next user message and returns as soon as
// opencode has queued it; the reply streams through Events.
func (c *Opencode) Prompt(ctx context.Context, sessionID, text string) error {
	body, _ := json.Marshal(map[string]any{"parts": []map[string]string{{"type": "text", "text": text}}})
	return post(ctx, c.HTTP, c.Base+"/session/"+sessionID+"/prompt_async", body)
}

// Abort stops the session's current turn.
func (c *Opencode) Abort(ctx context.Context, sessionID string) error {
	return post(ctx, c.HTTP, c.Base+"/session/"+sessionID+"/abort", []byte("{}"))
}

// Permissions lists every permission request still waiting for a reply.
func (c *Opencode) Permissions(ctx context.Context) ([]PermissionRequest, error) {
	out := []PermissionRequest{}
	err := c.getJSON(ctx, "/permission", &out)
	return out, err
}

// ReplyPermission answers one request: once, always or reject.
func (c *Opencode) ReplyPermission(ctx context.Context, requestID, reply string) error {
	if reply != "once" && reply != "always" && reply != "reject" {
		return fmt.Errorf("permission reply %q: want once, always or reject", reply)
	}
	body, _ := json.Marshal(map[string]string{"reply": reply})
	return post(ctx, c.HTTP, c.Base+"/permission/"+requestID+"/reply", body)
}

// Questions lists every question still waiting for an answer.
func (c *Opencode) Questions(ctx context.Context) ([]QuestionRequest, error) {
	out := []QuestionRequest{}
	err := c.getJSON(ctx, "/question", &out)
	return out, err
}

// ReplyQuestion answers one request: the chosen labels per question.
func (c *Opencode) ReplyQuestion(ctx context.Context, requestID string, answers [][]string) error {
	if answers == nil {
		answers = [][]string{}
	}
	body, _ := json.Marshal(map[string]any{"answers": answers})
	return post(ctx, c.HTTP, c.Base+"/question/"+requestID+"/reply", body)
}

// RejectQuestion declines to answer; the tool call fails and the agent
// carries on without it.
func (c *Opencode) RejectQuestion(ctx context.Context, requestID string) error {
	return post(ctx, c.HTTP, c.Base+"/question/"+requestID+"/reject", []byte("{}"))
}

// Events reads GET /event (server-sent events, one JSON object per data:
// line) and hands each to fn until ctx ends or the server closes the
// stream, which is the error returned.
func (c *Opencode) Events(ctx context.Context, fn func(OpencodeEvent)) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+"/event", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{}).Do(req) // no timeout: this stream lives as long as ctx
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("event stream → %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev OpencodeEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev); err == nil {
			fn(ev)
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return ctx.Err()
}

func (c *Opencode) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s → %d: %s", path, resp.StatusCode, bytes.TrimSpace(body))
	}
	return body, nil
}

func (c *Opencode) getJSON(ctx context.Context, path string, out any) error {
	body, err := c.get(ctx, path)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}
