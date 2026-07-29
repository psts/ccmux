package manager

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

func aliasManager(t *testing.T) *Manager {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "alias.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(context.Background(), &tmux.Server{Socket: "unused"}, st)
}

func TestIdentityAliases_RoundTrip(t *testing.T) {
	m := aliasManager(t)
	if got := m.IdentityAliases(); len(got) != 0 {
		t.Fatalf("fresh registry has aliases %v, want none", got)
	}

	if err := m.SetIdentityAliases(map[string]string{"Patric Sandelin": "p@example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := m.ResolveAlias("Patric Sandelin"); got != "p@example.com" {
		t.Errorf("ResolveAlias = %q, want the configured login", got)
	}
}

// Keys are lowercased on write so a lookup can be a plain map hit rather than a
// scan; that is only correct if the write side is the one doing the normalising.
func TestIdentityAliases_KeysAreStoredLowercased(t *testing.T) {
	m := aliasManager(t)
	if err := m.SetIdentityAliases(map[string]string{"  Patric Sandelin ": "  p@example.com  "}); err != nil {
		t.Fatal(err)
	}

	got := m.IdentityAliases()
	if _, ok := got["patric sandelin"]; !ok {
		t.Errorf("stored keys are %v, want the name lowercased and trimmed", got)
	}
	if got["patric sandelin"] != "p@example.com" {
		t.Errorf("login = %q, want it trimmed but otherwise untouched", got["patric sandelin"])
	}
}

// A half-filled row can only ever produce a wrong match. Dropping it would make
// the write succeed while doing something other than what was asked, and the one
// symptom would be push notifications that keep arriving — so it is rejected.
func TestIdentityAliases_RejectsEmptySides(t *testing.T) {
	cases := map[string]map[string]string{
		"empty name":      {"": "orphan@example.com"},
		"empty login":     {"nameless": ""},
		"whitespace only": {"  ": "  "},
		"one bad row":     {"real": "real@example.com", "": "orphan@example.com"},
	}
	for name, aliases := range cases {
		t.Run(name, func(t *testing.T) {
			m := aliasManager(t)
			err := m.SetIdentityAliases(aliases)
			if !errors.Is(err, ErrIncompleteAlias) {
				t.Fatalf("err = %v, want ErrIncompleteAlias", err)
			}
			if got := m.IdentityAliases(); len(got) != 0 {
				t.Errorf("stored %v; a rejected write must persist nothing", got)
			}
		})
	}
}

func TestResolveAlias_PassesThroughWhatItDoesNotKnow(t *testing.T) {
	m := aliasManager(t)
	if err := m.SetIdentityAliases(map[string]string{"known": "known@example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := m.ResolveAlias("stranger"); got != "stranger" {
		t.Errorf("ResolveAlias(stranger) = %q, want it unchanged", got)
	}
	if got := m.ResolveAlias(""); got != "" {
		t.Errorf("ResolveAlias(\"\") = %q, want it unchanged", got)
	}
}

// A corrupt settings value must not take identity resolution down with it — every
// caller would lose its login, and push suppression would silently invert.
func TestIdentityAliases_CorruptValueReadsAsEmpty(t *testing.T) {
	m := aliasManager(t)
	if err := m.store.SetSetting(settingIdentityAliases, "{not json"); err != nil {
		t.Fatal(err)
	}

	if got := m.IdentityAliases(); len(got) != 0 {
		t.Errorf("corrupt value produced %v, want an empty map", got)
	}
	if got := m.ResolveAlias("someone"); got != "someone" {
		t.Errorf("ResolveAlias = %q, want the name unchanged", got)
	}
}
