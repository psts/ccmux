package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowDirIsFoundByIdOrNamed(t *testing.T) {
	root := t.TempDir()
	s := NewStore(filepath.Join(root, "agents"))
	windows := filepath.Join(root, "windows")
	id := "a86926c5-54af-46ca-85cc-10ade12b37da"
	// Nothing on disk: a new folder, slug plus id prefix.
	if got := s.WindowDir(id, "Chart Labs"); got != filepath.Join(windows, "chart-labs-a86926c5") {
		t.Fatalf("fresh = %s", got)
	}
	// A folder that merely shares the slug is somebody else's.
	os.MkdirAll(filepath.Join(windows, "chartlabs"), 0o755)
	if got := s.WindowDir(id, "ChartLabs"); got != filepath.Join(windows, "chartlabs-a86926c5") {
		t.Fatalf("slug-only folder must not be adopted: %s", got)
	}
	// A folder carrying the id wins over the slug, and survives a rename.
	os.MkdirAll(filepath.Join(windows, "old-name-a86926c5"), 0o755)
	for _, name := range []string{"ChartLabs", "Chart Labs", "Renamed"} {
		if got := s.WindowDir(id, name); got != filepath.Join(windows, "old-name-a86926c5") {
			t.Fatalf("by id (%s) = %s", name, got)
		}
	}
	if got := s.InstanceDir(id, "Renamed", "scout"); got != filepath.Join(windows, "old-name-a86926c5", "agents", "scout") {
		t.Fatalf("instance = %s", got)
	}
}

func TestEnsureSharedWritesTheReadmeOnce(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "agents"))
	id := "bce808d7-02d8-4c3a-b003-067d91136fc4"
	if s.ExistingSharedDir(id, "Dasha") != "" {
		t.Fatal("no shared folder before the first agent start")
	}
	dir, err := s.EnsureShared(id, "Dasha")
	if err != nil || !strings.HasSuffix(dir, filepath.Join("dasha-bce808d7", "shared")) {
		t.Fatalf("ensure = %s, %v", dir, err)
	}
	if s.ExistingSharedDir(id, "Dasha") != dir {
		t.Fatal("existing after ensure")
	}
	readme := filepath.Join(dir, "README.md")
	seeded, _ := os.ReadFile(readme)
	for _, want := range []string{"One folder per agent", "YYYY-MM-DD-<slug>.md", "Frontmatter", "`.env`"} {
		if !strings.Contains(string(seeded), want) {
			t.Fatalf("seeded README lacks %q", want)
		}
	}
	os.WriteFile(readme, []byte("mine now"), 0o644)
	if _, err := s.EnsureShared(id, "Dasha"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(readme); string(b) != "mine now" {
		t.Fatalf("README rewritten: %q", b)
	}
}
