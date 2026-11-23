package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func get(t *testing.T, path, ifNoneMatch string) *http.Response {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	return rec.Result()
}

func TestBuildNamesTheFiles(t *testing.T) {
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(Build) {
		t.Fatalf("Build = %q, want 12 hex digits", Build)
	}
	if hashFiles(Files) != Build {
		t.Fatal("the same files hashed twice gave two builds")
	}
}

func TestBuildJSSaysTheBuild(t *testing.T) {
	resp := get(t, "/build.js", "")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"`+Build+`"`) {
		t.Fatalf("GET /build.js = %d %q, want 200 naming %s", resp.StatusCode, body, Build)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("Content-Type = %q, want javascript", ct)
	}
}

// A browser that already holds this build is told so in a 304, on the shell
// files, the page itself and build.js alike; one holding another build gets
// the bytes.
func TestUnchangedBuildIsA304(t *testing.T) {
	etag := `"` + Build + `"`
	for _, p := range []string{"/", "/app.js", "/build.js"} {
		resp := get(t, p, etag)
		if resp.StatusCode != http.StatusNotModified {
			t.Errorf("GET %s with the current ETag = %d, want 304", p, resp.StatusCode)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want no-cache", p, cc)
		}
	}
	if resp := get(t, "/app.js", `"older"`); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /app.js with an old ETag = %d, want 200", resp.StatusCode)
	}
}

// Build is also every file's ETag, so a change it misses is a deploy no
// browser ever fetches: a one-byte edit of the same length, and a rename,
// must each give a new build. A set that only moved nothing keeps its build.
func TestBuildFollowsEveryByte(t *testing.T) {
	base := fstest.MapFS{"app.js": {Data: []byte("let a = 1;")}, "style.css": {Data: []byte("b{}")}}
	edited := fstest.MapFS{"app.js": {Data: []byte("let a = 2;")}, "style.css": {Data: []byte("b{}")}}
	renamed := fstest.MapFS{"app2.js": {Data: []byte("let a = 1;")}, "style.css": {Data: []byte("b{}")}}
	same := fstest.MapFS{"style.css": {Data: []byte("b{}")}, "app.js": {Data: []byte("let a = 1;")}}
	if hashFiles(base) == hashFiles(edited) {
		t.Error("a same-length edit kept the build")
	}
	if hashFiles(base) == hashFiles(renamed) {
		t.Error("a rename kept the build")
	}
	if hashFiles(base) != hashFiles(same) {
		t.Error("the same files gave another build")
	}
}

// Every file index.html loads and every icon the manifest names must be
// served, each stamped with the build: a browser revalidates it by that ETag
// on every load. Catches a name left out of go:embed.
func TestEveryLensFileIsServed(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile("manifest.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	page := matches(`(?:src|href)="(/[^"]+)"`, html)
	icons := matches(`"src"\s*:\s*"(/[^"]+)"`, manifest)
	if len(page) < 15 || len(icons) < 2 {
		t.Fatalf("found %d page paths and %d icons; the patterns no longer match the files", len(page), len(icons))
	}
	for _, p := range append(page, icons...) {
		resp := get(t, p, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", p, resp.StatusCode)
		}
		if tag := resp.Header.Get("ETag"); tag != `"`+Build+`"` {
			t.Errorf("GET %s ETag = %s, want the build", p, tag)
		}
	}
	if code := get(t, "/no-such-file.js", "").StatusCode; code != http.StatusNotFound {
		t.Errorf("a missing file answered %d; the check above would pass on anything", code)
	}
}

// The thread page is its own document beside the lens: it and every file it
// loads must be embedded too, or the page opens blank on a deployed daemon.
func TestEveryThreadPageFileIsServed(t *testing.T) {
	html, err := os.ReadFile("threads.html")
	if err != nil {
		t.Fatal(err)
	}
	page := matches(`(?:src|href)="(/[^"]+)"`, html)
	if len(page) < 12 {
		t.Fatalf("found %d paths in threads.html; the pattern no longer matches the file", len(page))
	}
	for _, p := range append(page, "/threads.html") {
		if p == "/" {
			continue // the link back to the lens
		}
		resp := get(t, p, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", p, resp.StatusCode)
		}
		if tag := resp.Header.Get("ETag"); tag != `"`+Build+`"` {
			t.Errorf("GET %s ETag = %s, want the build", p, tag)
		}
	}
	// Every script of the page is one it shares with the lens or one of its
	// own; a script tag with a relative path would slip past the pattern
	// above and 404 only in a browser.
	if rel := matches(`(?:src|href)="([^/"#][^"]*)"`, html); len(rel) != 0 {
		t.Errorf("threads.html loads by relative path, which nothing checks: %v", rel)
	}
}

// The thread page installs as its own app: its manifest opens on it, has an
// id the lens's manifest does not (a browser takes two manifests with one id,
// or none and one start_url, for the same app), and names icons that are
// served.
func TestThreadPageIsItsOwnApp(t *testing.T) {
	var lens, threads struct {
		ID       string `json:"id"`
		StartURL string `json:"start_url"`
		Icons    []struct {
			Src string `json:"src"`
		} `json:"icons"`
	}
	for name, into := range map[string]any{"manifest.webmanifest": &lens, "threads.webmanifest": &threads} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	lensID := lens.ID
	if lensID == "" {
		lensID = lens.StartURL
	}
	if threads.StartURL != "/threads.html" || threads.ID == "" || threads.ID == lensID {
		t.Errorf("threads manifest start_url %q id %q, lens id %q: not an app of its own", threads.StartURL, threads.ID, lensID)
	}
	resp := get(t, "/threads.webmanifest", "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/manifest+json" {
		t.Errorf("GET /threads.webmanifest = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if len(threads.Icons) < 2 {
		t.Fatalf("threads manifest names %d icons", len(threads.Icons))
	}
	for _, ic := range threads.Icons {
		if code := get(t, ic.Src, "").StatusCode; code != http.StatusOK {
			t.Errorf("GET %s = %d", ic.Src, code)
		}
	}
}

// The Settings window is one piece of code for both pages: the block of
// scripts that makes it (dom.js to the agent editor) must be the same files
// in the same order on each, beside its stylesheet. A tab file added to one
// page alone would show as a missing tab, or as a script that dies on load,
// in a browser only.
func TestBothPagesLoadTheSameSettingsWindow(t *testing.T) {
	block := func(name string) []string {
		html, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		scripts := matches(`<script src="(/[^"]+)"`, html)
		from, to := slices.Index(scripts, "/dom.js"), slices.Index(scripts, "/agentmodal.js")
		if from < 0 || to < from {
			t.Fatalf("%s: no settings block from /dom.js to /agentmodal.js in %v", name, scripts)
		}
		if !slices.Contains(matches(`<link rel="stylesheet" href="(/[^"]+)"`, html), "/settings.css") {
			t.Errorf("%s does not link /settings.css, so the window would open unstyled", name)
		}
		return scripts[from : to+1]
	}
	lens, threads := block("index.html"), block("threads.html")
	if len(lens) < 10 {
		t.Fatalf("the lens's settings block is %v; the frame and five tabs are more files than that", lens)
	}
	if !slices.Equal(lens, threads) {
		t.Errorf("the two pages load different Settings windows:\n lens    %v\n threads %v", lens, threads)
	}
}

func matches(pattern string, b []byte) []string {
	var out []string
	for _, m := range regexp.MustCompile(pattern).FindAllSubmatch(b, -1) {
		out = append(out, string(m[1]))
	}
	return out
}
