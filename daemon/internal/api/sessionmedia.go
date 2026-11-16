package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // the decoders DecodeConfig reads a picture's size with
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
)

// A runner session's images. A picture pasted into the thread page is
// uploaded here, lands in the session's media folder under a name made from
// its bytes, and is sent with a prompt by that name: Claude gets the picture
// itself, not a path typed into a terminal. The same folder holds the images
// the session's runner kept (a tool's screenshot, a prompt read back from
// disk), and the page loads all of them from here.

// The limits a picture must keep to be sent to Claude at all, checked here
// because nothing after this point resizes it, and a picture stays in the
// conversation for good: one the API refuses would be refused again on
// every later turn.
//
//   - maxSessionImageBytes: the API takes 10 MB per image, measured as
//     base64 (docs: build-with-claude/vision, "Request limits", read
//     2026-11-16); 5 MB of bytes is 6.7 MB as base64.
//   - maxSessionImageSide: an image may be 8000 px a side, but once a
//     request holds more than 20 images each must stay under a stricter
//     limit, and the docs' advice to stay under it is 3000 px a side. A long
//     conversation gets past 20 images, so that is the limit from the first.
//
// The page shrinks a pasted picture to fit before it uploads; this is the
// check that it did.
const (
	maxSessionImageBytes = 5 << 20
	maxSessionImageSide  = 3000
)

// mediaNameRE is the only shape of name the media folder holds
// (claudeserve/media.mjs keeps the same rule on the runner's side).
var mediaNameRE = regexp.MustCompile(`^[0-9a-f]{32}\.(png|jpg|gif|webp)$`)

// uploadKinds are the pictures an upload may be, by what DecodeConfig calls
// them. (A runner also keeps WebP images a tool returned; the page turns a
// pasted WebP into a PNG, since this build cannot read a WebP's size.)
var uploadKinds = map[string]string{"png": ".png", "jpeg": ".jpg", "gif": ".gif"}

// checkSessionImage says what kind of picture b is (its file extension), or
// why it cannot be sent to Claude.
func checkSessionImage(b []byte) (ext string, err error) {
	cfg, kind, err := image.DecodeConfig(bytes.NewReader(b))
	ext, known := uploadKinds[kind]
	if err != nil || !known {
		return "", errors.New("not a PNG, JPEG or GIF image")
	}
	if cfg.Width > maxSessionImageSide || cfg.Height > maxSessionImageSide {
		return "", fmt.Errorf("the image is %d x %d px; the most Claude takes in a long conversation is %d px a side", cfg.Width, cfg.Height, maxSessionImageSide)
	}
	return ext, nil
}

// storeSessionImage writes b into dir under the name its bytes give it and
// returns that name. A picture already there is left as it is.
func storeSessionImage(dir string, b []byte, ext string) (string, error) {
	sum := sha256.Sum256(b)
	name := hex.EncodeToString(sum[:])[:32] + ext
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		return name, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return "", err
	}
	return name, os.Rename(tmp, path)
}

// uploadRunnerImage: POST /v1/sessions/{id}/images, the body the image's
// bytes. 201 {name, url}: name is what a prompt lists under images.
func (s *Server) uploadRunnerImage(w http.ResponseWriter, r *http.Request) {
	if !s.runnersReady(w) {
		return
	}
	id := r.PathValue("id")
	if sess, err := s.runnerRows.RunnerSession(id); sess == nil {
		writeRunnerError(w, orNoRunner(err))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSessionImageBytes)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the image is over %d MB, the most that is sent to Claude", maxSessionImageBytes>>20))
			return
		}
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	ext, err := checkSessionImage(b)
	if err != nil {
		writeError(w, http.StatusUnsupportedMediaType, err.Error())
		return
	}
	name, err := storeSessionImage(s.runners.MediaDir(id), b, ext)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the image was not stored: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": name, "url": "/v1/sessions/" + id + "/media/" + name})
}

// runnerMedia: GET /v1/sessions/{id}/media/{name} serves one stored image.
// A name is its bytes, so the answer never changes and a browser may keep it.
func (s *Server) runnerMedia(w http.ResponseWriter, r *http.Request) {
	if !s.runnersReady(w) {
		return
	}
	id, name := r.PathValue("id"), r.PathValue("name")
	if sess, err := s.runnerRows.RunnerSession(id); sess == nil {
		writeRunnerError(w, orNoRunner(err))
		return
	}
	if !mediaNameRE.MatchString(name) {
		writeError(w, http.StatusNotFound, "no such image")
		return
	}
	path := filepath.Join(s.runners.MediaDir(id), name)
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		writeError(w, http.StatusNotFound, "no such image")
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, path)
}

// promptImagesProblem says why a prompt's images cannot go with it, or "":
// each must be a name this session's media folder holds.
func (s *Server) promptImagesProblem(sessionID string, names []string) string {
	dir := s.runners.MediaDir(sessionID)
	for _, name := range names {
		if !mediaNameRE.MatchString(name) {
			return "not an image of this session: " + name
		}
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return "no such image in this session (upload it first): " + name
		}
	}
	return ""
}
