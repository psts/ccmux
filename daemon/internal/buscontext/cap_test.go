package buscontext

import "testing"

// The cap is whatever Claude Code was told in its environment, and its own
// default for anything it would not take as a length.
func TestCap(t *testing.T) {
	for value, want := range map[string]int{
		"":       DefaultCap,
		"16384":  16384,
		" 8192 ": 8192,
		"0":      DefaultCap,
		"-5":     DefaultCap,
		"lots":   DefaultCap,
		"4096.5": DefaultCap,
	} {
		if got := Cap(value); got != want {
			t.Errorf("Cap(%q) = %d, want %d", value, got, want)
		}
	}
	if Cap("") != 4096 || Cap(RaisedCapValue) != RaisedCap || RaisedCap <= DefaultCap {
		t.Fatalf("default %d, raised %d (%q): the daemon's value must read back as the raised cap", DefaultCap, RaisedCap, RaisedCapValue)
	}
}
