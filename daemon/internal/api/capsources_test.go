package api

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitAPI(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func writeT(t *testing.T, path, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCapSources_Routes adds a local marketplace with one plugin, pins it,
// moves the marketplace on, and reads the update available off the list.
func TestCapSources_Routes(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	s := agentsServer(t)
	root := t.TempDir()
	work, bare := filepath.Join(root, "work"), filepath.Join(root, "m.git")
	writeT(t, filepath.Join(work, ".claude-plugin", "marketplace.json"), `{"name":"m","plugins":[{"name":"qa","source":"./qa"}]}`)
	writeT(t, filepath.Join(work, "qa", ".claude-plugin", "plugin.json"), `{"name":"qa","version":"1.0.0"}`)
	gitAPI(t, root, "init", "--quiet", "-b", "main", work)
	gitAPI(t, work, "add", "-A")
	gitAPI(t, work, "commit", "--quiet", "-m", "one")
	gitAPI(t, root, "clone", "--quiet", "--bare", work, bare)

	if rec := do(t, s, "GET", "/v1/capabilities/sources", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"personal"`) {
		t.Fatalf("seeds = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "POST", "/v1/capabilities/sources", `{"id":"x","url":"https://u:p@github.com/a/b"}`); rec.Code != 400 {
		t.Fatalf("a url with a secret is refused: %d", rec.Code)
	}
	if rec := do(t, s, "POST", "/v1/capabilities/sources", `{"id":"gone","url":"`+filepath.Join(root, "nothing.git")+`"}`); rec.Code != 502 || strings.Contains(rec.Body.String(), "removing") {
		t.Fatalf("a source that does not clone = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "GET", "/v1/capabilities/sources", ""); strings.Contains(rec.Body.String(), `"gone"`) {
		t.Fatal("a source whose first fetch failed is not kept")
	}
	// A repo with no marketplace clones, fails, and its pin folder cannot be
	// removed: the reply says the rollback failed too.
	plain := filepath.Join(root, "plain")
	writeT(t, filepath.Join(plain, "README"), "x")
	gitAPI(t, root, "init", "--quiet", "-b", "main", plain)
	gitAPI(t, plain, "add", "-A")
	gitAPI(t, plain, "commit", "--quiet", "-m", "x")
	pins := filepath.Join(filepath.Dir(s.agents.Root), "capabilities", ".pins", "plain")
	writeT(t, filepath.Join(pins, "c", "f"), "x")
	os.Chmod(pins, 0o500)
	t.Cleanup(func() { os.Chmod(pins, 0o755) })
	if rec := do(t, s, "POST", "/v1/capabilities/sources", `{"id":"plain","url":"`+plain+`"}`); rec.Code != 502 || !strings.Contains(rec.Body.String(), "removing the source again failed") {
		t.Fatalf("a failed rollback is reported = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "POST", "/v1/capabilities/sources", `{"id":"m","url":"`+bare+`"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"added":["qa"]`) {
		t.Fatalf("add = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "POST", "/v1/capabilities/qa/pin", `{"pinned":true}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"pinned":true`) {
		t.Fatalf("pin = %d %s", rec.Code, rec.Body)
	}
	writeT(t, filepath.Join(work, "qa", ".claude-plugin", "plugin.json"), `{"name":"qa","version":"1.1.0"}`)
	gitAPI(t, work, "commit", "--quiet", "-am", "two")
	gitAPI(t, work, "push", "--quiet", bare, "main")
	if rec := do(t, s, "POST", "/v1/capabilities/sources/m/update", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"behind":[{"id":"qa","from":"1.0.0","to":"1.1.0"}]`) {
		t.Fatalf("update = %d %s", rec.Code, rec.Body)
	}
	var got struct {
		Capabilities []struct {
			ID, Version, Source, UpdateAvailable string
		} `json:"capabilities"`
		Sources []struct{ ID string } `json:"sources"`
	}
	rec := do(t, s, "GET", "/v1/capabilities", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Capabilities) != 1 || len(got.Sources) != 4 {
		t.Fatalf("list = %s %v", rec.Body, err)
	}
	if c := got.Capabilities[0]; c.Version != "1.0.0" || c.Source != "m" || c.UpdateAvailable != "1.1.0" {
		t.Fatalf("a pinned entry shows the update its source has: %+v", c)
	}
	if rec := do(t, s, "POST", "/v1/capabilities/sources/nope/update", ""); rec.Code != 404 {
		t.Fatalf("unknown source = %d", rec.Code)
	}
	if rec := do(t, s, "POST", "/v1/capabilities/nope/pin", `{"pinned":true}`); rec.Code != 404 {
		t.Fatalf("unknown entry = %d", rec.Code)
	}
	if rec := do(t, s, "DELETE", "/v1/capabilities/sources/m", ""); rec.Code != 200 {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, "GET", "/v1/capabilities", ""); !strings.Contains(rec.Body.String(), `"capabilities":[]`) {
		t.Fatalf("its entries go with it: %s", rec.Body)
	}
}
