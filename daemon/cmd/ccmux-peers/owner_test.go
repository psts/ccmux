package main

import "testing"

// ownsBus is the pane-inbox ownership rule: this process may act as the pane's
// single bus peer only if it holds a controlling terminal (the pane's pty) and
// is in that terminal's foreground process group — i.e. it's the interactive
// session or one of its direct children. Sub-agents and warm spares are detached
// or in another group, so they must be excluded.
func TestOwnsBus(t *testing.T) {
	tests := []struct {
		name            string
		hasCtty         bool
		fg, myPg, parPg int
		want            bool
	}{
		{"interactive session: MCP server shares the session's group", true, 100, 100, 100, true},
		{"interactive session: MCP server in own group, parent is the session", true, 100, 555, 100, true},
		{"sub-agent not detached: different group, parent isn't the foreground", true, 100, 300, 300, false},
		{"sub-agent detached: no controlling terminal at all", false, 0, 0, 0, false},
		{"has a terminal but a foreground job unrelated to us holds it", true, 100, 300, 400, false},
		{"terminal reports no foreground group", true, 0, 0, 0, false},
	}
	for _, tc := range tests {
		if got := ownsBus(tc.hasCtty, tc.fg, tc.myPg, tc.parPg); got != tc.want {
			t.Errorf("%s: ownsBus(%v, fg=%d, my=%d, parent=%d) = %v, want %v",
				tc.name, tc.hasCtty, tc.fg, tc.myPg, tc.parPg, got, tc.want)
		}
	}
}
