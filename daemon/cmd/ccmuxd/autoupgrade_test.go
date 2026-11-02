package main

import (
	"errors"
	"net/http"
	"testing"
)

func TestNewerRelease(t *testing.T) {
	cases := []struct {
		tag, current string
		want         bool
	}{
		{"v0.1.88", "0.1.87", true},
		{"v0.1.10", "0.1.9", true}, // numbers, not strings
		{"v0.2.0", "0.1.99", true},
		{"v0.1.87", "0.1.87", false},
		{"v0.1.86", "0.1.87", false}, // never backwards
		{"latest", "0.1.87", false},  // not a release tag
	}
	for _, c := range cases {
		if got := newerRelease(c.tag, c.current); got != c.want {
			t.Errorf("newerRelease(%q, %q) = %v, want %v", c.tag, c.current, got, c.want)
		}
	}
}

func TestReleaseBuild(t *testing.T) {
	for b, want := range map[string]bool{"0.1.87": true, "dev": false, "0.1.87-3-gabc": false, "0.1.87-dirty": false} {
		if got := releaseBuild(b); got != want {
			t.Errorf("releaseBuild(%q) = %v, want %v", b, got, want)
		}
	}
}

func TestAutoUpgradeOnce(t *testing.T) {
	var started []string
	start := func(tag string) (int, string) {
		started = append(started, tag)
		return http.StatusAccepted, "upgrading"
	}
	latest := func(tag string, err error) func() (string, error) {
		return func() (string, error) { return tag, err }
	}

	// The same or an older release starts nothing.
	autoUpgradeOnce("0.1.87", latest("v0.1.87", nil), start, false)
	autoUpgradeOnce("0.1.87", latest("v0.1.86", nil), start, false)
	if len(started) != 0 {
		t.Fatalf("started %v for a release that is not newer", started)
	}
	// A newer one starts the upgrade, with that exact tag.
	if failing := autoUpgradeOnce("0.1.87", latest("v0.1.88", nil), start, false); failing {
		t.Fatal("a readable release must clear the failing state")
	}
	if len(started) != 1 || started[0] != "v0.1.88" {
		t.Fatalf("started = %v, want [v0.1.88]", started)
	}
	// An unreadable release list starts nothing and reports the failing state,
	// so the loop logs it once rather than every tick.
	if failing := autoUpgradeOnce("0.1.87", latest("", errors.New("offline")), start, false); !failing {
		t.Fatal("a failed read must report the failing state")
	}
	if len(started) != 1 {
		t.Fatalf("a failed read started %v", started)
	}
}
