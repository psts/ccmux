package agent

import (
	"ccmux.dev/ccmuxd/internal/agent/agenttest"
	"encoding/json"
	"testing"
)

func TestNormalizeMessagesKeepsWhatAReaderNeeds(t *testing.T) {
	turns, err := NormalizeMessages([]byte(agenttest.SampleMessages))
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0].Role != "user" || turns[0].Parts[0].Text != "where is X?" || turns[0].Time != 1000 {
		t.Fatalf("user turn: %+v", turns[0])
	}
	a := turns[1]
	if a.Error != "MessageAbortedError: aborted" || len(a.Parts) != 3 {
		t.Fatalf("assistant turn: %+v", a)
	}
	if a.Parts[0].Type != "reasoning" || a.Parts[0].Text != "thinking" {
		t.Errorf("reasoning: %+v", a.Parts[0])
	}
	tool := a.Parts[1]
	if tool.Type != "tool" || tool.Tool != "grep" || tool.Status != "completed" || tool.Title != "grep X" || tool.Output != "a.go:1" || string(tool.Input) != `{"pattern":"X"}` {
		t.Errorf("tool: %+v", tool)
	}
	if a.Parts[2].Text != "It is in a.go:1" {
		t.Errorf("text: %+v", a.Parts[2])
	}
	// The export form wraps the same array.
	export, err := NormalizeExport([]byte(`{"info":{"id":"ses_1"},"messages":` + agenttest.SampleMessages + `}`))
	if err != nil || len(export) != 2 {
		t.Fatalf("export: %v %d", err, len(export))
	}
	// A turn with no parts serializes with an empty list, not null: lenses
	// index it without a guard.
	b, _ := json.Marshal(Turn{ID: "x", Role: "user", Parts: []TurnPart{}})
	if string(b) != `{"id":"x","role":"user","time":0,"parts":[]}` {
		t.Errorf("empty turn: %s", b)
	}
}

func TestNormalizePartDropsMarkers(t *testing.T) {
	if _, _, ok := NormalizePart(json.RawMessage(`{"id":"p","messageID":"m","type":"step-start"}`)); ok {
		t.Error("step-start must be dropped")
	}
	mid, p, ok := NormalizePart(json.RawMessage(`{"id":"p","messageID":"m","type":"tool","tool":"bash","state":{"status":"error","input":{"command":"rm"},"error":"denied"}}`))
	if !ok || mid != "m" || p.Status != "error" || p.Error != "denied" {
		t.Errorf("tool error: %v %+v", ok, p)
	}
	if _, _, ok := NormalizePart(json.RawMessage(`not json`)); ok {
		t.Error("garbage must be dropped")
	}
}
