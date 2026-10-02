package api

import (
	"encoding/json"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/agent"
)

// turnOf is a one-part assistant turn whose tool output is n bytes long.
func turnOf(id string, n int) agent.Turn {
	return agent.Turn{ID: id, Role: "assistant", Time: 1000, Parts: []agent.TurnPart{
		{ID: id + "-p", Type: "tool", Output: strings.Repeat("x", n)},
	}}
}

func encodedSize(t *testing.T, turns []agent.Turn) int {
	t.Helper()
	b, err := json.Marshal(turns)
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

func ids(turns []agent.Turn) []string {
	out := make([]string, len(turns))
	for i, t := range turns {
		out[i] = t.ID
	}
	return out
}

// A history under the budget goes out exactly as it came in: no marker,
// nothing dropped.
func TestCapHistory_UnderBudgetUnchanged(t *testing.T) {
	in := []agent.Turn{turnOf("a", 10), turnOf("b", 10)}
	got := capHistory(in, 1<<20)
	if strings.Join(ids(got), ",") != "a,b" {
		t.Fatalf("got %v, want [a b]", ids(got))
	}
}

// Over the budget, the oldest turns go first, the newest stay in order, a
// session-role marker leads, and what is kept fits the budget.
func TestCapHistory_DropsOldestBehindMarker(t *testing.T) {
	in := []agent.Turn{turnOf("a", 400), turnOf("b", 400), turnOf("c", 400), turnOf("d", 400)}
	budget := encodedSize(t, in[2:]) // room for c and d, not b
	got := capHistory(in, budget)
	if strings.Join(ids(got), ",") != historyTrimmedID+",c,d" {
		t.Fatalf("got %v, want [%s c d]", ids(got), historyTrimmedID)
	}
	m := got[0]
	if m.Role != "session" || !strings.HasPrefix(m.Title, "Older messages not shown") {
		t.Errorf("marker = %+v, want a session divider saying older messages are not shown", m)
	}
	if m.Time != got[1].Time {
		t.Errorf("marker time = %d, want the first kept turn's %d (a 0 leaves a dangling dot in both lenses)", m.Time, got[1].Time)
	}
	if m.Parts == nil {
		t.Error("marker parts are nil; they encode as null and every other turn sends []")
	}
	if n := encodedSize(t, got[1:]); n > budget {
		t.Errorf("kept turns are %d bytes, over the %d budget", n, budget)
	}
}

// A newest turn bigger than the whole budget is still shown: a chat that
// opens empty would be worse than one frame over the budget.
func TestCapHistory_KeepsNewestEvenWhenAloneOverBudget(t *testing.T) {
	in := []agent.Turn{turnOf("a", 10), turnOf("b", 5000)}
	got := capHistory(in, 100)
	if strings.Join(ids(got), ",") != historyTrimmedID+",b" {
		t.Fatalf("got %v, want [%s b]", ids(got), historyTrimmedID)
	}
}

func TestCapHistory_Empty(t *testing.T) {
	if got := capHistory([]agent.Turn{}, 100); len(got) != 0 {
		t.Fatalf("got %v, want no turns", ids(got))
	}
}

// history() is where the cap lives, so every hello (live, asleep, the
// one-shot GET) gets it: four sessions of 3 MiB each are over the 8 MiB
// budget, and the oldest go.
func TestHistory_AppliesTheBudget(t *testing.T) {
	sessions := []agent.OpencodeSession{{ID: "s4"}, {ID: "s3"}, {ID: "s2"}, {ID: "s1"}} // newest first
	got, err := history(sessions, func(id string) ([]agent.Turn, error) {
		return []agent.Turn{turnOf("m-"+id, 3<<20)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].ID != historyTrimmedID {
		t.Fatalf("first turn = %q, want the trimmed marker", got[0].ID)
	}
	if last := got[len(got)-1].ID; last != "m-s4" {
		t.Errorf("last turn = %q, want the newest session's m-s4", last)
	}
	if n := encodedSize(t, got[1:]); n > historyBudget {
		t.Errorf("kept turns are %d bytes, over the %d budget", n, historyBudget)
	}
}
