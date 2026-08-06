package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveWorkspacePath(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "docs")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sub, "plan.md")
	if err := os.WriteFile(file, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(secret, escape); err != nil {
		t.Fatal(err)
	}

	// EvalSymlinks-normalized root (macOS /var/folders is itself a symlink),
	// so expectations compare against what resolve actually returns.
	rootAbs, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, in string
		want     string // "" = expect error
		wantErr  error
	}{
		{"relative", "docs/plan.md", filepath.Join(rootAbs, "docs/plan.md"), nil},
		{"absolute inside", file, filepath.Join(rootAbs, "docs/plan.md"), nil},
		{"root itself", "", rootAbs, nil},
		{"traversal", "../" + filepath.Base(outside) + "/secret.txt", "", errPathEscapes},
		{"absolute outside", secret, "", errPathEscapes},
		{"symlink escape", "escape", "", errPathEscapes},
		{"missing", "docs/nope.md", "", os.ErrNotExist},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveWorkspacePath(root, c.in)
			if c.want == "" {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				if c.wantErr != nil && !errors.Is(err, c.wantErr) {
					t.Fatalf("want %v, got %v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("want %q, got %q", c.want, got)
			}
		})
	}
}

func TestSuffixMatchFallback(t *testing.T) {
	root := t.TempDir()
	mustRun := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustRun("git", "init", "-q")
	write("apps/api/app/routers/agents/content.py", "match")
	write("apps/api/app/other.py", "tracked")
	write("untracked/note.md", "untracked")
	mustRun("git", "add", "apps")

	rootAbs, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	// A tool-relative path (cwd unknown) finds the unique tracked match.
	got, err := resolveWorkspaceFile(root, "routers/agents/content.py")
	if err != nil || got != filepath.Join(rootAbs, "apps/api/app/routers/agents/content.py") {
		t.Fatalf("relative fallback: got %q, %v", got, err)
	}
	// A cwd-joined ABSOLUTE miss falls back too (the lens joins the pane cwd
	// before it can know the path was relative to something else).
	got, err = resolveWorkspaceFile(root, filepath.Join(root, "routers/agents/content.py"))
	if err != nil || got != filepath.Join(rootAbs, "apps/api/app/routers/agents/content.py") {
		t.Fatalf("absolute fallback: got %q, %v", got, err)
	}
	// Untracked-but-not-ignored files are still found.
	if _, err = resolveWorkspaceFile(root, "note.md"); err != nil {
		t.Fatalf("untracked fallback: %v", err)
	}
	// Ambiguity stays a miss.
	write("apps/web/app/routers/agents/content.py", "dupe")
	mustRun("git", "add", "apps/web")
	if _, err = resolveWorkspaceFile(root, "routers/agents/content.py"); err == nil {
		t.Fatal("ambiguous suffix must not resolve")
	}
	// A genuinely absent file stays a miss.
	if _, err = resolveWorkspaceFile(root, "no/such/file.py"); err == nil {
		t.Fatal("absent file must not resolve")
	}
}

// filesStack boots a real daemon and creates a workspace rooted at a temp repo
// with one seeded file, returning (base URL, workspace id, repo root).
func filesStack(t *testing.T) (string, string, string) {
	t.Helper()
	_, base := floodStack(t, "ccmux-files-test")
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "readme.md"), []byte("# hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"files","repoPath":` + jsonQuote(repo) + `,"createdBy":"tester","startupCommand":""}`
	resp, err := http.Post(base+"/v1/workspaces", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	var ws wsResult
	if err := json.NewDecoder(resp.Body).Decode(&ws); err != nil {
		t.Fatalf("decode ws: %v", err)
	}
	return base, ws.ID, repo
}

// jsonQuote JSON-quotes a string (paths can contain characters JSON must escape).
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestFileRoutes(t *testing.T) {
	base, wsID, repo := filesStack(t)
	filesURL := base + "/v1/workspaces/" + wsID + "/files"

	// Read the seeded file.
	resp, err := http.Get(filesURL + "?path=" + url.QueryEscape("readme.md"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Path, Content string }
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || got.Content != "# hello\n" {
		t.Fatalf("read: status %d content %q", resp.StatusCode, got.Content)
	}

	// Save new content over it.
	putBody := `{"path":"readme.md","content":"# changed\n"}`
	req, _ := http.NewRequest(http.MethodPut, filesURL, bytes.NewReader([]byte(putBody)))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("put: status %d", resp.StatusCode)
	}
	b, _ := os.ReadFile(filepath.Join(repo, "readme.md"))
	if string(b) != "# changed\n" {
		t.Fatalf("put content on disk: %q", b)
	}

	// Saving a file that doesn't exist must not create it.
	req, _ = http.NewRequest(http.MethodPut, filesURL, bytes.NewReader([]byte(`{"path":"new.md","content":"x"}`)))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("put-create: status %d, want 404", resp.StatusCode)
	}

	// Escapes to real files outside the root are rejected.
	resp, err = http.Get(filesURL + "?path=" + url.QueryEscape("/etc/passwd"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("escape: status %d, want 400", resp.StatusCode)
	}

	// Non-UTF-8 files are refused — the viewer is a text editor.
	if err := os.WriteFile(filepath.Join(repo, "blob.bin"), []byte{0xff, 0xfe, 0x00, 0x81}, 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err = http.Get(filesURL + "?path=" + url.QueryEscape("blob.bin"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 415 {
		t.Fatalf("binary read: status %d, want 415", resp.StatusCode)
	}

	// Oversized files are refused before being read.
	if err := os.WriteFile(filepath.Join(repo, "big.txt"), bytes.Repeat([]byte("a"), maxFileBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err = http.Get(filesURL + "?path=" + url.QueryEscape("big.txt"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("oversized read: status %d, want 413", resp.StatusCode)
	}

	// The dir route enforces the same root confinement.
	resp, err = http.Get(base + "/v1/workspaces/" + wsID + "/dir?path=" + url.QueryEscape("/etc"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("dir escape: status %d, want 400", resp.StatusCode)
	}

	// Directory listing of the root.
	resp, err = http.Get(base + "/v1/workspaces/" + wsID + "/dir")
	if err != nil {
		t.Fatal(err)
	}
	var dir struct {
		Entries []fileEntry `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dir); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	names := map[string]bool{}
	for _, e := range dir.Entries {
		names[e.Name] = e.Dir
	}
	if isDir, ok := names["src"]; !ok || !isDir {
		t.Fatalf("dir listing missing src dir: %+v", dir.Entries)
	}
	if isDir, ok := names["readme.md"]; !ok || isDir {
		t.Fatalf("dir listing missing readme.md file: %+v", dir.Entries)
	}
}
