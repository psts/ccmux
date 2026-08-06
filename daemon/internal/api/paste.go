package api

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// The paste route bridges a lens clipboard image to the daemon's host: Claude
// Code reads its clipboard on the machine it RUNS on, so an image copied on
// the Mac is invisible to a hosted session. The lens uploads the bytes here,
// gets back a host-side temp path, and types that path into the pane — the
// same shape drag-and-drop produces locally.

// maxPasteBytes caps an uploaded paste image (screenshots, not transfers).
const maxPasteBytes = 10 << 20

// pasteMaxAge is how long pasted images live before the sweep removes them —
// long enough for a Claude session to read (and re-read) them, short enough
// that /tmp doesn't accumulate.
const pasteMaxAge = 24 * time.Hour

// sniffImageExt classifies image bytes by magic number; "" = not an image.
func sniffImageExt(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return ".png"
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return ".jpg"
	case bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a")):
		return ".gif"
	case len(b) >= 12 && bytes.Equal(b[0:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return ".webp"
	default:
		return ""
	}
}

// pasteDir is the host-side landing directory. Per-user by construction, same
// rule as cmd/ccmuxd's runtimeDir: fixed names in the shared /tmp made the
// daemon single-user per machine (sticky bit + another user's leftover dir =
// EACCES forever, seen live), so use XDG_RUNTIME_DIR where systemd provides
// it and a uid-suffixed TempDir name otherwise.
func pasteDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "ccmux-paste")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("ccmux-paste-%d", os.Getuid()))
}

// sweepStalePastes removes paste files older than pasteMaxAge. Best-effort —
// run on every upload, bounded by the directory's (small) size.
func sweepStalePastes(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-pasteMaxAge)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// pasteImage lands one clipboard image on this host and returns its path as
// {"path": "..."}. The body is the raw image bytes; type comes from the bytes
// themselves, never from headers.
func (s *Server) pasteImage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.workspaceRoot(w, r); !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPasteBytes)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "image too large")
			return
		}
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	ext := sniffImageExt(b)
	if ext == "" {
		writeError(w, http.StatusUnsupportedMediaType, "not an image")
		return
	}
	dir := pasteDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sweepStalePastes(dir)
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	path := filepath.Join(dir, "paste-"+hex.EncodeToString(rnd[:])+ext)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"path": path})
}
