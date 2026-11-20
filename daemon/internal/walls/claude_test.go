package walls

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// claudeHome is a home folder with a ~/.claude like a real one: its
// config, its data, and no agents folder yet.
func claudeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	for _, name := range []string{"hooks", "plugins", "skills", "commands", "projects", "shell-snapshots", "session-env", "sessions", "file-history"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{filepath.Join(dir, "settings.json"), filepath.Join(dir, "settings.local.json"), filepath.Join(dir, "CLAUDE.md"), filepath.Join(dir, ".credentials.json"), filepath.Join(home, ".claude.json")} {
		if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestClaudesFolderIsNotOpenedOnlyItsConversationsAre(t *testing.T) {
	home := claudeHome(t)
	dir := filepath.Join(home, ".claude")
	p := Plan{Write: []string{"/wt"}, PrivateRoot: "/own"}
	p.ClaudeHome(home)
	text := p.DropIn()
	rw := line(text, "ReadWritePaths=")
	if rw != "ReadWritePaths=/wt -"+filepath.Join(dir, "projects") {
		t.Fatalf("of Claude's folder only the conversations are opened, and never the folder itself: %s", rw)
	}
	for _, opened := range []string{dir, filepath.Join(home, ".claude.json"), filepath.Join(dir, "settings.json"), filepath.Join(dir, "hooks"), filepath.Join(dir, ".credentials.json")} {
		if slices.Contains(p.Write, opened) || slices.Contains(p.Caches, opened) {
			t.Errorf("%s is opened, and what is written there runs outside the walls", opened)
		}
	}
}

func TestEveryPartOfClaudesConfigIsSealedAndChecked(t *testing.T) {
	home := claudeHome(t)
	dir := filepath.Join(home, ".claude")
	p := Plan{PrivateRoot: "/own"}
	p.ClaudeHome(home)
	refused := p.Expect("mnt:[1]").Refused
	ro := line(p.DropIn(), "ReadOnlyPaths=")
	for _, sealed := range []string{
		filepath.Join(dir, "settings.json"), filepath.Join(dir, "settings.local.json"), filepath.Join(dir, "CLAUDE.md"),
		filepath.Join(dir, "hooks"), filepath.Join(dir, "plugins"), filepath.Join(dir, "skills"), filepath.Join(dir, "commands"),
		filepath.Join(home, ".claude.json"),
	} {
		if !strings.Contains(ro+" ", " "+sealed+" ") && !strings.Contains(ro+" ", "="+sealed+" ") {
			t.Errorf("%s is not sealed in the unit file: %s", sealed, ro)
		}
		if !slices.Contains(refused, sealed) {
			t.Errorf("the unit's check does not try a write at %s: %v", sealed, refused)
		}
	}
	if !slices.Contains(refused, dir) {
		t.Errorf("the check must try the folder itself: a new agents or commands folder is made there: %v", refused)
	}
	// What is not there is not named: systemd fails a unit on a sealed
	// path that is missing. The folder being read-only is what keeps it
	// from being made.
	if strings.Contains(ro, filepath.Join(dir, "agents")) {
		t.Errorf("a config folder that is not there cannot be named: %s", ro)
	}
}

func TestWhatASessionSourcesIsTheUnitsOwn(t *testing.T) {
	home := claudeHome(t)
	dir := filepath.Join(home, ".claude")
	p := Plan{PrivateRoot: "/own"}
	p.ClaudeHome(home)
	binds := line(p.DropIn(), "BindPaths=")
	for _, name := range []string{"shell-snapshots", "session-env", "sessions", "file-history"} {
		if !strings.Contains(binds, ":"+filepath.Join(dir, name)) {
			t.Errorf("%s must be the unit's own folder, not every session's: %s", name, binds)
		}
	}
	if strings.Contains(binds, "todos") {
		t.Errorf("a folder that is not there has nothing to stand in for: %s", binds)
	}
	if strings.Contains(binds, filepath.Join(dir, "projects")) {
		t.Errorf("the conversations are the real ones, or a session could not be resumed: %s", binds)
	}
}

func TestTheFoldersATurnNeedsAreMadeBeforeTheFirstWalledStart(t *testing.T) {
	home := t.TempDir()
	if err := MakeClaudeFolders(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Fatal("a host with no Claude Code gets no folder made for it")
	}
	os.Mkdir(filepath.Join(home, ".claude"), 0o755)
	if err := MakeClaudeFolders(home); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"projects", "shell-snapshots", "session-env", "sessions"} {
		if _, err := os.Stat(filepath.Join(home, ".claude", name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "todos")); err == nil {
		t.Error("only what a turn cannot do without is made")
	}
}

// line is the unit file line that starts with key, "" for none.
func line(text, key string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, key) {
			return l
		}
	}
	return ""
}
