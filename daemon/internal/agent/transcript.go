package agent

import (
	"encoding/json"
	"fmt"
)

// Turn is one message of an agent's conversation in the shape both lenses
// render: who spoke, when, and its parts. Normalized here from opencode's
// message + parts so a change in opencode's wire format lands in one
// place, and the Mac and web chat views never each grow their own reading.
type Turn struct {
	ID    string     `json:"id"`
	Role  string     `json:"role"` // user | assistant
	Time  int64      `json:"time"` // unix millis, creation
	Parts []TurnPart `json:"parts"`
	// Error is the assistant turn's failure, if it ended in one ("aborted",
	// a provider refusal), in one line.
	Error string `json:"error,omitempty"`
}

// TurnPart is one piece of a turn: text the model wrote, its reasoning, or
// a tool call with its state. Other opencode part kinds (step markers,
// snapshots, patches) carry nothing a reader needs and are dropped.
type TurnPart struct {
	ID   string `json:"id"`
	Type string `json:"type"` // text | reasoning | tool
	Text string `json:"text,omitempty"`
	// Tool call fields: the tool's name, its state (pending | running |
	// completed | error), the one-line title opencode gives the call, its
	// input as opencode recorded it, and the output or error once it ended.
	Tool   string          `json:"tool,omitempty"`
	Status string          `json:"status,omitempty"`
	Title  string          `json:"title,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type rawMessage struct {
	Info  json.RawMessage   `json:"info"`
	Parts []json.RawMessage `json:"parts"`
}

type rawInfo struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	Time struct {
		Created int64 `json:"created"`
	} `json:"time"`
	Error *struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

type rawPart struct {
	ID        string `json:"id"`
	MessageID string `json:"messageID"`
	Type      string `json:"type"`
	Text      string `json:"text"`
	Tool      string `json:"tool"`
	State     *struct {
		Status string          `json:"status"`
		Input  json.RawMessage `json:"input"`
		Output string          `json:"output"`
		Title  string          `json:"title"`
		Error  string          `json:"error"`
	} `json:"state"`
}

// NormalizeMessages reads opencode's GET /session/{id}/message body (an
// array of {info, parts}) into turns.
func NormalizeMessages(raw []byte) ([]Turn, error) {
	var msgs []rawMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil, fmt.Errorf("opencode messages: %w", err)
	}
	return normalizeRaw(msgs)
}

// NormalizeExport reads `opencode export <session>` output ({info,
// messages: [{info, parts}]}) into turns — the offline source, for an
// agent that is asleep.
func NormalizeExport(raw []byte) ([]Turn, error) {
	var export struct {
		Messages []rawMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &export); err != nil {
		return nil, fmt.Errorf("opencode export: %w", err)
	}
	return normalizeRaw(export.Messages)
}

func normalizeRaw(msgs []rawMessage) ([]Turn, error) {
	turns := make([]Turn, 0, len(msgs))
	for _, m := range msgs {
		t, err := NormalizeInfo(m.Info)
		if err != nil {
			return nil, err
		}
		for _, p := range m.Parts {
			if _, part, ok := NormalizePart(p); ok {
				t.Parts = append(t.Parts, part)
			}
		}
		turns = append(turns, t)
	}
	return turns, nil
}

// NormalizeInfo reads one opencode message (the "info" object) into a turn
// without parts.
func NormalizeInfo(raw json.RawMessage) (Turn, error) {
	var in rawInfo
	if err := json.Unmarshal(raw, &in); err != nil {
		return Turn{}, fmt.Errorf("opencode message: %w", err)
	}
	t := Turn{ID: in.ID, Role: in.Role, Time: in.Time.Created, Parts: []TurnPart{}}
	if in.Error != nil {
		t.Error = in.Error.Name
		if in.Error.Data.Message != "" {
			t.Error += ": " + in.Error.Data.Message
		}
	}
	return t, nil
}

// NormalizePart reads one opencode part; ok is false for kinds a reader
// does not need. Returns the message the part belongs to.
func NormalizePart(raw json.RawMessage) (messageID string, part TurnPart, ok bool) {
	var in rawPart
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", TurnPart{}, false
	}
	part = TurnPart{ID: in.ID, Type: in.Type}
	switch in.Type {
	case "text", "reasoning":
		part.Text = in.Text
	case "tool":
		part.Tool = in.Tool
		if in.State != nil {
			part.Status, part.Title, part.Input = in.State.Status, in.State.Title, in.State.Input
			part.Output, part.Error = in.State.Output, in.State.Error
		}
	default:
		return in.MessageID, TurnPart{}, false
	}
	return in.MessageID, part, true
}
