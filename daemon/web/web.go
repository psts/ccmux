// Package web embeds the ccmux web lens (a no-build xterm.js client + PWA) so the
// daemon serves the whole UI — shell, service worker, manifest, icons — from its
// single binary.
package web

import (
	"embed"
	"mime"
)

//go:embed index.html app.js push.js sw.js style.css manifest.webmanifest vendor icons
var Files embed.FS

func init() {
	// http.FileServer types responses via mime.TypeByExtension; register the
	// manifest type so browsers accept /manifest.webmanifest.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}
