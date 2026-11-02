package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
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

// Every file the service worker caches and every file index.html loads must
// be served: one 404 fails the worker's whole cache refresh, and the page
// would never move onto a new build. Catches a name left out of go:embed.
func TestEveryShellFileIsServed(t *testing.T) {
	sw, _ := os.ReadFile("sw.js")
	html, _ := os.ReadFile("index.html")
	shell := regexp.MustCompile(`(?s)const SHELL = \[(.*?)\]`).FindSubmatch(sw)
	if shell == nil {
		t.Fatal("no SHELL list in sw.js")
	}
	var paths []string
	for _, m := range regexp.MustCompile(`"(/[^"]*)"`).FindAllSubmatch(shell[1], -1) {
		paths = append(paths, string(m[1]))
	}
	for _, m := range regexp.MustCompile(`(?:src|href)="(/[^"]+)"`).FindAllSubmatch(html, -1) {
		paths = append(paths, string(m[1]))
	}
	if len(paths) < 20 {
		t.Fatalf("found only %d paths; the patterns no longer match the files", len(paths))
	}
	for _, p := range paths {
		if code := get(t, p, "").StatusCode; code >= 400 {
			t.Errorf("GET %s = %d", p, code)
		}
	}
	if code := get(t, "/no-such-file.js", "").StatusCode; code != http.StatusNotFound {
		t.Errorf("a missing file answered %d; the check above would pass on anything", code)
	}
}
