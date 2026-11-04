// Package web embeds the ccmux web lens (a no-build xterm.js client + PWA) so the
// daemon serves the whole UI — shell, service worker, manifest, icons — from its
// single binary.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"strings"
	"time"
)

//go:embed index.html app.js route.js pagewin.js touchscroll.js drawerswipe.js agentchat.js scratchpad.js board.js tasklist.js tasks.js owns.js agentcaps.js agentmodal.js peers.js push.js paste.js keys.js flash.js windowwake.js update.js sw.js style.css manifest.webmanifest vendor icons
var Files embed.FS

// Build names this exact set of lens files: a short hash of every embedded
// file. A page learns the build it booted with from /build.js and the build
// the daemon serves now from /v1/health, and when the two differ it moves onto
// the new one (update.js). A hash, not the daemon's version, so a deploy that
// leaves the lens alone does not reload anybody.
var Build = hashFiles(Files)

func init() {
	// http.FileServer types responses via mime.TypeByExtension; register the
	// manifest type so browsers accept /manifest.webmanifest.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

func hashFiles(fsys fs.FS) string {
	h := sha256.New()
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
		h.Write(b)
		return nil
	})
	if err != nil {
		// Embedded files cannot go missing at run time; a failure here is a
		// broken build, and a lens that never learns of updates is the cost.
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// Handler serves the lens: the embedded files plus /build.js. Every answer
// carries Build as its ETag with no-cache, so a browser asks "still this
// build?" on every load and gets a 304 instead of the bytes when nothing
// changed. The service worker caches nothing (sw.js), so this is the only
// cache, and a deploy lands on the very next load.
func Handler() http.Handler {
	files := http.FileServerFS(Files)
	etag := `"` + Build + `"`
	buildJS := fmt.Sprintf("window.ccmuxBuild = %q;\n", Build)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", etag)
		if r.URL.Path == "/build.js" {
			http.ServeContent(w, r, "build.js", time.Time{}, strings.NewReader(buildJS))
			return
		}
		files.ServeHTTP(w, r)
	})
}
