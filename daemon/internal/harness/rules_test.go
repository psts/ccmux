package harness

import (
	"strings"
	"testing"
)

// TestFallbackClaudeCommand_HidesTMUX pins the `env -u TMUX` prefix. It does
// not verify the clipboard behavior (that needs a live claude in a real pane;
// measured by hand, see the constant's comment) — it exists so a future edit
// to the command line has to consciously delete this test to drop the prefix.
func TestFallbackClaudeCommand_HidesTMUX(t *testing.T) {
	if !strings.HasPrefix(FallbackClaudeCommand, "env -u TMUX ") {
		t.Errorf("claude command = %q, want it to hide $TMUX from claude", FallbackClaudeCommand)
	}
	if !strings.Contains(FallbackClaudeCommand, "server:claude-peers") {
		t.Errorf("claude command lost the peers channel: %q", FallbackClaudeCommand)
	}
}

// Half-filled editor rows are dropped, prefixes are normalized, and the rest
// round-trips.
func TestSetRulesNormalization(t *testing.T) {
	s := testService(fakeStore{})
	err := s.SetRules([]Rule{
		{PathPrefix: " /w/ChartLabs/ ", Harness: " pi "},
		{PathPrefix: "/w/half", Harness: "  "},
		{PathPrefix: "", Harness: "opencode"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rules, err := s.Rules()
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].PathPrefix != "/w/ChartLabs" || rules[0].Harness != "pi" {
		t.Fatalf("rules = %+v", rules)
	}
}

// PreselectFor is a suggestion, never a spawn path: longest matching prefix
// wins on folder boundaries, and every miss — no rule, a rule naming a
// harness that no longer exists, a broken store — degrades to the builtin.
func TestPreselectFor(t *testing.T) {
	s := testService(fakeStore{})
	if err := s.Apply([]Harness{{Name: "pi", Command: "pi"}, {Name: "opencode", Command: "opencode"}}); err != nil {
		t.Fatal(err)
	}
	err := s.SetRules([]Rule{
		{PathPrefix: "/w/ChartLabs", Harness: "pi"},
		{PathPrefix: "/w/ChartLabs/backend", Harness: "opencode"},
		{PathPrefix: "/w/gone", Harness: "deleted-harness"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ repo, want string }{
		{"/w/ChartLabs/web", "pi"},
		{"/w/ChartLabs/backend/api", "opencode"},
		{"/w/ChartLabs", "pi"},
		{"/w/ChartLabsFoo", Builtin}, // prefix ends on a folder boundary, not a substring
		{"/elsewhere", Builtin},
		{"/w/gone/repo", Builtin}, // rule outlived its harness
	}
	for _, c := range cases {
		if got := s.PreselectFor(c.repo); got != c.want {
			t.Errorf("PreselectFor(%q) = %q, want %q", c.repo, got, c.want)
		}
	}
	if got := testService(brokenStore{}).PreselectFor("/w/ChartLabs"); got != Builtin {
		t.Errorf("broken store preselect = %q, want the builtin", got)
	}
}
