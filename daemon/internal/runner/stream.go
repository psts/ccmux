package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// What the thread page needs of a runner, beyond what the supervisor does:
// the conversation as it stands, the numbered events after it, and the
// human's answers (a card's reply, Stop). The daemon relays these between a
// browser and the runner's socket (api/sessionstream.go); nothing here
// touches the registry.

// Snapshot is a runner's conversation at one moment: Seq is the number of
// the last event already folded into it, so reading the events after Seq
// continues it with nothing lost and nothing twice. Boot names the runner
// process; a different boot means the numbering started over. Body is the
// runner's whole answer (messages, pending cards, what it says of itself),
// passed to the page as it is.
type Snapshot struct {
	Boot string
	Seq  int64
	Body json.RawMessage
}

// Snapshot reads the runner's conversation and where its event log stands.
func (c *Client) Snapshot(ctx context.Context) (Snapshot, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/snapshot", nil, &raw); err != nil {
		return Snapshot{}, err
	}
	var head struct {
		Boot string `json:"boot"`
		Seq  int64  `json:"seq"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return Snapshot{}, fmt.Errorf("runner snapshot: %w", err)
	}
	if head.Boot == "" {
		return Snapshot{}, errors.New("runner snapshot names no boot")
	}
	return Snapshot{Boot: head.Boot, Seq: head.Seq, Body: raw}, nil
}

// Event is one numbered event of a runner: Data is the event as the runner
// sent it ({type, properties}).
type Event struct {
	Seq  int64
	Data json.RawMessage
}

// ErrGap is Events' answer when the runner no longer holds every event
// after the number asked for: the reader must take a snapshot instead.
var ErrGap = errors.New("the runner dropped events before the ones asked for")

// maxEventLine bounds one event line. A longer one ends the stream with an
// error; the relay then rebuilds from a snapshot rather than asking for the
// same event again.
const maxEventLine = 16 << 20

// Events reads the events of runner process boot numbered above after, in
// order, and hands each to fn until ctx ends, the runner closes the stream
// (nil), fn fails (its error) or a line cannot be read. It returns ErrGap
// before any event when the runner cannot continue from there: it dropped
// some of them, or it is another process whose numbering started over.
func (c *Client) Events(ctx context.Context, boot string, after int64, fn func(Event) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://runner/event?after="+strconv.FormatInt(after, 10), nil)
	if err != nil {
		return err
	}
	// The client's timeout would cut the stream; its transport still applies.
	res, err := (&http.Client{Transport: c.http.Transport}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("runner event stream: HTTP %d", res.StatusCode)
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 64*1024), maxEventLine)
	if err := readEvents(sc, cursor{boot: boot, after: after}, fn); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return sc.Err()
}

// cursor is where a reader stands: the runner process it was reading and
// the last event it has.
type cursor struct {
	boot  string
	after int64
}

// readEvents walks the stream's lines: "id: N" numbers the data line that
// follows it. The opening line (server.connected) has no id and says
// whether the stream continues from the cursor.
func readEvents(sc *bufio.Scanner, at cursor, fn func(Event) error) error {
	var seq int64
	for sc.Scan() {
		line := sc.Bytes()
		if id, ok := bytes.CutPrefix(line, []byte("id:")); ok {
			seq, _ = strconv.ParseInt(string(bytes.TrimSpace(id)), 10, 64)
			continue
		}
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		data = bytes.TrimSpace(data)
		if seq == 0 {
			if at.gapSaid(data) {
				return ErrGap
			}
			continue
		}
		ev := Event{Seq: seq, Data: append(json.RawMessage(nil), data...)}
		seq = 0
		if err := fn(ev); err != nil {
			return err
		}
	}
	return nil
}

// gapSaid reads the stream's opening line: the runner says it dropped
// events, or it is not the process the cursor belongs to (another boot, or
// a log that stands before the cursor).
func (at cursor) gapSaid(data []byte) bool {
	var open struct {
		Type  string `json:"type"`
		Props struct {
			Gap  bool   `json:"gap"`
			Seq  int64  `json:"seq"`
			Boot string `json:"boot"`
		} `json:"properties"`
	}
	if json.Unmarshal(data, &open) != nil || open.Type != "server.connected" {
		return false
	}
	return open.Props.Gap || open.Props.Seq < at.after || open.Props.Boot != at.boot
}

// Abort stops the session's running turn. A session between turns takes it
// and does nothing.
func (c *Client) Abort(ctx context.Context, sessionID string) error {
	return c.do(ctx, http.MethodPost, "/session/"+sessionID+"/abort", struct{}{}, nil)
}

// Answer is a human's answer to one card, from the session's own page:
// for a permission, Reply (once, always or reject); for a question, the
// chosen labels per question in Answers, or Reject to decline it.
type Answer struct {
	Kind    string     `json:"kind"`
	ID      string     `json:"id"`
	Reply   string     `json:"reply,omitempty"`
	Answers [][]string `json:"answers,omitempty"`
	Reject  bool       `json:"reject,omitempty"`
}

// ErrBadAnswer is an answer no card of that kind takes.
var ErrBadAnswer = errors.New("not an answer a card takes")

// route is the runner route an answer goes to, and its body.
func (a Answer) route() (path string, body any, err error) {
	switch {
	case a.ID == "":
		return "", nil, fmt.Errorf("%w: it names no card", ErrBadAnswer)
	case a.Kind == "permission" && (a.Reply == "once" || a.Reply == "always" || a.Reply == "reject"):
		return "/permission/" + a.ID + "/reply", map[string]string{"reply": a.Reply}, nil
	case a.Kind == "question" && a.Reject:
		return "/question/" + a.ID + "/reject", struct{}{}, nil
	case a.Kind == "question":
		answers := a.Answers
		if answers == nil {
			answers = [][]string{}
		}
		return "/question/" + a.ID + "/reply", map[string]any{"answers": answers}, nil
	}
	return "", nil, fmt.Errorf("%w: kind %q, reply %q", ErrBadAnswer, a.Kind, a.Reply)
}

// AnswerCard sends a human's answer to a pending card. answered is false
// when the runner holds no such card any more (answered elsewhere first, or
// gone with an earlier runner process), which is not an error.
func (c *Client) AnswerCard(ctx context.Context, a Answer) (answered bool, err error) {
	path, body, err := a.route()
	if err != nil {
		return false, err
	}
	status, err := c.status(ctx, http.MethodPost, path, body)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("runner POST %s: HTTP %d", path, status)
}

// status is one call whose HTTP status is the answer.
func (c *Client) status(ctx context.Context, method, path string, in any) (int, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://runner"+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("content-type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	res.Body.Close()
	return res.StatusCode, nil
}
