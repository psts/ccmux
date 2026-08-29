package childproc

import "testing"

// TestParseStat_CommWithSpacesAndParen pins the field-splitting rule. A naive
// strings.Fields on the whole line reads the comm as fields 1..n and lands on
// the wrong column — and the process this package exists to count is literally
// named "tmux: client", which contains a space (verified: a live control client
// reads as `(tmux: client)`). comm cannot itself contain a ')' — Linux takes it
// from prctl and caps it at 15 bytes — so the last-')' rule is defensive
// against arbitrary processes rather than against tmux specifically. Getting
// this wrong does not error, it silently reports zero zombies forever.
func TestParseStat_CommWithSpacesAndParen(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		wantState string
		wantPPID  int
	}{
		{"plain comm", "42 (bash) S 7 42 42 0 -1 4194304 1 0", "S", 7},
		{"comm with a space", "99 (tmux: client) Z 260762 99 99 0 -1 0 0 0", "Z", 260762},
		{"comm containing a paren", "13 (weird ):) ) R 5 13 13 0 -1 0 0 0", "R", 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, ppid, ok := parseStat(tc.line)
			if !ok {
				t.Fatalf("parseStat(%q) not ok", tc.line)
			}
			if state != tc.wantState || ppid != tc.wantPPID {
				t.Errorf("parseStat = (%q, %d), want (%q, %d)",
					state, ppid, tc.wantState, tc.wantPPID)
			}
		})
	}

	// Garbage must report not-ok rather than a confident zero.
	for _, bad := range []string{"", "no parens here", "1 (x)", "1 (x) S notanumber"} {
		if _, _, ok := parseStat(bad); ok {
			t.Errorf("parseStat(%q) reported ok on unusable input", bad)
		}
	}
}
