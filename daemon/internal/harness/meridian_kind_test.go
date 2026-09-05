package harness

import (
	"slices"
	"strings"
	"testing"
)

func TestOpencodeDefaultsExcludeRawSubscriptionAndCodex(t *testing.T) {
	kinds := defaultAccountKinds["opencode"]
	if kinds[0] != "meridian" {
		t.Errorf("meridian must be first so the subscription wins pairing: %v", kinds)
	}
	for _, want := range []string{"anthropic", "openai", "meridian"} {
		if !slices.Contains(kinds, want) {
			t.Errorf("opencode should pair with %s: %v", want, kinds)
		}
	}
	for _, refuse := range []string{"claude", "codex"} {
		if slices.Contains(kinds, refuse) {
			t.Errorf("opencode must not pair with %s: %v", refuse, kinds)
		}
	}
}

func TestRejectKnowsMeridianKind(t *testing.T) {
	var s Service
	if msg := s.Reject([]Harness{{Name: "oc", Command: "opencode", AccountKinds: []string{"meridian"}}}); msg != "" {
		t.Fatalf("meridian kind refused: %s", msg)
	}
	msg := s.Reject([]Harness{{Name: "oc", Command: "opencode", AccountKinds: []string{"nope"}}})
	if !strings.Contains(msg, "or meridian") {
		t.Fatalf("unknown-kind message should list meridian, got %q", msg)
	}
}
