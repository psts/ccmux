package harness

import (
	"errors"
	"testing"
)

// The full conversion: a custom default becomes the claude override, rules
// map by exact command match, then by program (an INSTALLED pi answers for
// `env FOO=1 pi --fast`), and the unmappable — including a program no
// harness runs — are dropped; both legacy keys are cleared and a second run
// is a no-op.
func TestMigrateLegacyStartupSettings(t *testing.T) {
	st := fakeStore{
		legacySettingStartupCommand: "claude --dangerously-load-development-channels server:claude-peers",
		legacySettingStartupRules: `[
			{"pathPrefix":"/w/CL","command":"claude --dangerously-load-development-channels server:claude-peers"},
			{"pathPrefix":"/w/pi","command":"env FOO=1 pi --fast"},
			{"pathPrefix":"/w/odd","command":"./run.sh"}]`,
	}
	s := testService(st)
	s.lookPath = func(name string) (string, error) {
		if name == "pi" {
			return "/usr/local/bin/pi", nil
		}
		return "", errors.New("not installed")
	}
	if err := s.MigrateLegacyStartupSettings(); err != nil {
		t.Fatal(err)
	}
	c, err := s.Resolve("claude")
	if err != nil || c.Command != "claude --dangerously-load-development-channels server:claude-peers" {
		t.Fatalf("claude after migration = %+v, %v", c, err)
	}
	if c.Source != "" || c.Icon != "✳" || !c.Autoconfirm {
		t.Fatalf("override shape = %+v, want user entry keeping icon and autoconfirm", c)
	}
	rules, err := s.Rules()
	if err != nil {
		t.Fatal(err)
	}
	// /w/CL matched the migrated claude override's command exactly; /w/pi
	// resolves by PROGRAM against the detected pi harness (the real guess
	// only knows claude); ./run.sh maps to nothing and is dropped.
	if len(rules) != 2 || rules[0].Harness != "claude" || rules[1].Harness != "pi" {
		t.Fatalf("rules after migration = %+v", rules)
	}
	if st[legacySettingStartupCommand] != "" || st[legacySettingStartupRules] != "" {
		t.Fatalf("legacy keys not cleared: %q / %q", st[legacySettingStartupCommand], st[legacySettingStartupRules])
	}
	before := st[settingHarnesses] + "|" + st[settingHarnessRules]
	if err := s.MigrateLegacyStartupSettings(); err != nil {
		t.Fatal(err)
	}
	if after := st[settingHarnesses] + "|" + st[settingHarnessRules]; after != before {
		t.Fatalf("second run changed state:\n%s\n%s", before, after)
	}
}

// An existing user claude override already won wholesale — the legacy default
// must not clobber it, only vanish.
func TestMigrateKeepsExistingClaudeOverride(t *testing.T) {
	st := fakeStore{legacySettingStartupCommand: "claude --old-flags"}
	s := testService(st)
	if err := s.Apply([]Harness{{Name: "claude", Command: "claude --continue"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateLegacyStartupSettings(); err != nil {
		t.Fatal(err)
	}
	c, _ := s.Resolve("claude")
	if c.Command != "claude --continue" {
		t.Fatalf("override clobbered: %+v", c)
	}
	if st[legacySettingStartupCommand] != "" {
		t.Fatal("legacy key not cleared")
	}
}

// failingKeyStore wraps fakeStore, failing every write to one key — the
// crash-between-writes shape the migration's clear-only-after-save contract
// exists for.
type failingKeyStore struct {
	fakeStore
	failKey string
}

func (f failingKeyStore) SetSetting(key, value string) error {
	if key == f.failKey {
		return errors.New("disk full")
	}
	return f.fakeStore.SetSetting(key, value)
}

// A failed save must leave the legacy key in place so the next boot retries —
// clearing first would lose the user's configured command forever.
func TestMigrateFailedSaveLeavesLegacyKey(t *testing.T) {
	st := fakeStore{legacySettingStartupCommand: "claude --continue"}
	s := testService(failingKeyStore{fakeStore: st, failKey: settingHarnesses})
	if err := s.MigrateLegacyStartupSettings(); err == nil {
		t.Fatal("migration reported success despite the failed save")
	}
	if st[legacySettingStartupCommand] != "claude --continue" {
		t.Fatalf("legacy key = %q, want it untouched for the next boot's retry", st[legacySettingStartupCommand])
	}
}

// Rules already configured in the new model are never overwritten by a
// late-arriving legacy conversion (a failed earlier boot left the legacy key;
// the user built rules through the new editors since).
func TestMigrateKeepsConfiguredRules(t *testing.T) {
	st := fakeStore{legacySettingStartupRules: `[{"pathPrefix":"/w/old","command":"claude"}]`}
	s := testService(st)
	if err := s.SetRules([]Rule{{PathPrefix: "/w/new", Harness: "pi"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateLegacyStartupSettings(); err != nil {
		t.Fatal(err)
	}
	rules, err := s.Rules()
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].PathPrefix != "/w/new" {
		t.Fatalf("rules = %+v, want the user's new-model rules untouched", rules)
	}
	if st[legacySettingStartupRules] != "" {
		t.Fatal("legacy rules key not cleared")
	}
}

// A legacy default equal to the fallback preserves nothing — no override is
// seeded, the builtin stays live.
func TestMigrateSkipsFallbackEqualDefault(t *testing.T) {
	st := fakeStore{legacySettingStartupCommand: FallbackClaudeCommand}
	s := testService(st)
	if err := s.MigrateLegacyStartupSettings(); err != nil {
		t.Fatal(err)
	}
	hs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 1 || hs[0].Source != "builtin" {
		t.Fatalf("list = %+v, want only the live builtin", hs)
	}
	if st[legacySettingStartupCommand] != "" {
		t.Fatal("legacy key not cleared")
	}
}
