package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
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
