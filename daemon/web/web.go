// Package web embeds the ccmux web lens (a no-build xterm.js client) so the
// daemon serves the whole UI from its single binary.
package web

import "embed"

//go:embed index.html app.js style.css vendor
var Files embed.FS
