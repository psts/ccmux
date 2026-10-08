package model

import "testing"

func TestAttentionClaims(t *testing.T) {
	for att, want := range map[Attention]bool{
		AttentionDone: true, AttentionNeedsInput: true,
		AttentionIdle: false, AttentionRunning: false, "": false,
	} {
		if got := att.Claims(); got != want {
			t.Errorf("%q.Claims() = %v, want %v", att, got, want)
		}
	}
}

// One wait keeps its start: a Stop, then Claude's idle reminder a minute later
// (done → needs_input, both "finished"), is the same claim. A different reason
// is a new claim, and claiming nothing clears the start.
func TestNextClaimSince(t *testing.T) {
	waiting := Pane{Attention: AttentionDone, AttentionReason: ReasonFinished, AttentionSince: 100}
	cases := []struct {
		name   string
		prev   Pane
		att    Attention
		reason AttentionReason
		want   int64
	}{
		{"new claim from idle", Pane{Attention: AttentionIdle}, AttentionDone, ReasonFinished, 500},
		{"same wait carries on", waiting, AttentionNeedsInput, ReasonFinished, 100},
		{"different reason restarts", waiting, AttentionNeedsInput, ReasonPermission, 500},
		{"claiming nothing clears", waiting, AttentionIdle, ReasonNone, 0},
		{"running clears", waiting, AttentionRunning, ReasonNone, 0},
		{"a claim with no recorded start restarts", Pane{Attention: AttentionDone, AttentionReason: ReasonFinished}, AttentionDone, ReasonFinished, 500},
	}
	for _, c := range cases {
		if got := NextClaimSince(c.prev, c.att, c.reason, 500); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
