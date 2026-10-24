package repoowns

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

type memStore struct {
	mu   sync.Mutex
	rows map[string]store.RepoOwns
}

func (m *memStore) RepoOwns(p string) (store.RepoOwns, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows[p], nil
}

func (m *memStore) SetRepoOwnsAuto(p, auto, hash string, now int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.rows[p]
	o.Auto, o.AutoHash = auto, hash
	m.rows[p] = o
	return nil
}

func repo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The first read returns what is stored (nothing) without waiting, and
// summarizes in the background; the next read has the line. Unchanged
// files are never summarized again; a changed file is.
func TestLine_SummarizesOnceAndAgainWhenTheFilesChange(t *testing.T) {
	dir := repo(t, map[string]string{"CLAUDE.md": "# backend\nThe API.", "README.md": "Postgres and auth."})
	st := &memStore{rows: map[string]store.RepoOwns{}}
	var asked []string
	s := New(st, func(_ context.Context, prompt string, _ int) (string, error) {
		asked = append(asked, prompt)
		return "Owns: API, database, auth.\nextra", nil
	})
	if got := s.Line("backend", dir); got != "" {
		t.Fatalf("first read must not wait on the model: %q", got)
	}
	s.Wait()
	if got := s.Line("backend", dir); got != "API, database, auth" {
		t.Fatalf("second read = %q", got)
	}
	s.Wait()
	if len(asked) != 1 || !strings.Contains(asked[0], `"backend"`) || !strings.Contains(asked[0], "Postgres and auth.") {
		t.Fatalf("asked %d times: %q", len(asked), asked)
	}
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("Postgres, auth and billing."), 0o644)
	s.Line("backend", dir)
	s.Wait()
	if len(asked) != 2 {
		t.Fatalf("a changed README must be summarized again: asked %d times", len(asked))
	}
}

// A human's line wins over the summary.
func TestLine_ManualWins(t *testing.T) {
	dir := repo(t, map[string]string{"README.md": "x"})
	_, hash := Source(dir)
	st := &memStore{rows: map[string]store.RepoOwns{dir: {Auto: "auto", AutoHash: hash, Manual: "typed by hand"}}}
	s := New(st, func(context.Context, string, int) (string, error) {
		t.Fatal("nothing changed; no call")
		return "", nil
	})
	if got := s.Line("r", dir); got != "typed by hand" {
		t.Fatalf("line = %q", got)
	}
}

// A repo with neither file is never sent to the model; a failing model is
// not asked again on every read, only after the retry wait.
func TestLine_NoFilesNoCallAndAFailureWaits(t *testing.T) {
	st := &memStore{rows: map[string]store.RepoOwns{}}
	calls := 0
	s := New(st, func(context.Context, string, int) (string, error) { calls++; return "", errors.New("no account") })
	s.Line("empty", repo(t, nil))
	s.Wait()
	if calls != 0 {
		t.Fatalf("a repo with no CLAUDE.md or README must not be summarized: %d calls", calls)
	}
	dir := repo(t, map[string]string{"README.md": "x"})
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	s.Line("r", dir)
	s.Wait()
	s.Line("r", dir)
	s.Wait()
	if calls != 1 {
		t.Fatalf("a failure must wait before asking again: %d calls", calls)
	}
	now = now.Add(retryAfter)
	s.Line("r", dir)
	s.Wait()
	if calls != 2 {
		t.Fatalf("after the wait it asks again: %d calls", calls)
	}
}

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"API, database, auth":            "API, database, auth",
		"\n\"Owns: UI, routing.\"\nmore": "UI, routing",
		"owns: billing jobs.":            "billing jobs",
		"   ":                            "",
		strings.Repeat("é", 200):         strings.Repeat("é", maxLine) + "…",
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}
