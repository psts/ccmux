package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// plugFile writes an installed_plugins.json and returns its path.
func plugFile(t *testing.T, doc string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "installed_plugins.json")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPruneDropsOnlyLocalRecordsOfGoneFoldersUnderTheWorktrees(t *testing.T) {
	wt := t.TempDir()
	live := filepath.Join(wt, "app-1", "task-2")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(wt, "app-1", "task-1")
	beside := wt + "2/app-1/task-9" // shares the prefix, not the folder
	doc := `{"version":2,"keep":"me","plugins":{
	  "qa@personal":[
	    {"scope":"user","installPath":"/c/qa","extra":1},
	    {"scope":"local","projectPath":"` + gone + `","installPath":"/c/qa"},
	    {"scope":"local","projectPath":"` + live + `","installPath":"/c/qa"},
	    {"scope":"local","projectPath":"` + beside + `","installPath":"/c/qa"},
	    {"scope":"local","projectPath":"/ext/projects/gone-repo","installPath":"/c/qa"}],
	  "only@dead":[{"scope":"local","projectPath":"` + gone + `"}]}}`
	p := plugFile(t, doc)
	removed, err := PruneInstalledPlugins(p, wt)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed %d, want 2 (qa's and only@dead's record of the gone task folder)", removed)
	}
	var got struct {
		Version int                                     `json:"version"`
		Keep    string                                  `json:"keep"`
		Plugins map[string][]map[string]json.RawMessage `json:"plugins"`
	}
	raw, _ := os.ReadFile(p)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || got.Keep != "me" {
		t.Fatalf("top-level fields lost: %s", raw)
	}
	if n := len(got.Plugins["qa@personal"]); n != 4 {
		t.Fatalf("qa keeps %d records, want 4: user scope, live folder, the look-alike folder, a folder outside the worktrees", n)
	}
	if string(got.Plugins["qa@personal"][0]["extra"]) != "1" {
		t.Fatalf("a record's own fields are kept: %s", raw)
	}
	if _, ok := got.Plugins["only@dead"]; ok {
		t.Fatalf("a plugin left with no record is dropped: %s", raw)
	}
}

func TestPruneLeavesTheFileAloneWhenNothingIsGone(t *testing.T) {
	wt := t.TempDir()
	p := plugFile(t, `{"plugins":{"qa@personal":[{"scope":"user"}]}}`)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := PruneInstalledPlugins(p, wt)
	if err != nil || removed != 0 {
		t.Fatalf("removed %d, err %v", removed, err)
	}
	if st, _ := os.Stat(p); !st.ModTime().Equal(old) {
		t.Fatal("the file was written though nothing was removed")
	}
}

func TestPruneWithNoFileOrNoWorktreesDoesNothing(t *testing.T) {
	if n, err := PruneInstalledPlugins(filepath.Join(t.TempDir(), "none.json"), t.TempDir()); n != 0 || err != nil {
		t.Fatalf("no file: %d %v", n, err)
	}
	p := plugFile(t, `{"plugins":{"qa@personal":[{"scope":"local","projectPath":"/gone/x"}]}}`)
	if n, err := PruneInstalledPlugins(p, ""); n != 0 || err != nil {
		t.Fatalf("no worktrees folder named: %d %v", n, err)
	}
}

func TestPruneRefusesAFileItCannotRead(t *testing.T) {
	p := plugFile(t, `{not json`)
	if _, err := PruneInstalledPlugins(p, t.TempDir()); err == nil {
		t.Fatal("a broken file must be an error, never rewritten")
	}
	if raw, _ := os.ReadFile(p); string(raw) != `{not json` {
		t.Fatalf("a broken file was changed: %s", raw)
	}
}
