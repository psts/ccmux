package api

import (
	"bytes"
	"encoding/json"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

// picture is a w x h image encoded as kind.
func picture(t *testing.T, kind string, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	var err error
	switch kind {
	case "png":
		err = png.Encode(&buf, img)
	case "jpeg":
		err = jpeg.Encode(&buf, img, nil)
	case "gif":
		err = gif.Encode(&buf, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// mediaServer is the session routes over a fake supervisor whose sessions
// keep their images in a folder of the test's.
func mediaServer(t *testing.T) (*Server, *fakeRunners) {
	t.Helper()
	f := &fakeRunners{mediaDir: filepath.Join(t.TempDir(), "s1.media")}
	s := &Server{}
	s.SetRunners(f, fakeRunnerRows{})
	return s, f
}

func upload(s *Server, session string, body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/sessions/"+session+"/images", bytes.NewReader(body)))
	return w
}

func TestAnUploadedImageIsStoredUnderItsBytesAndServedBack(t *testing.T) {
	s, f := mediaServer(t)
	shot := picture(t, "png", 40, 30)
	w := upload(s, "s1", shot)
	var out struct{ Name, URL string }
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != http.StatusCreated || !mediaNameRE.MatchString(out.Name) || out.URL != "/v1/sessions/s1/media/"+out.Name {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	if again := upload(s, "s1", shot); !strings.Contains(again.Body.String(), out.Name) {
		t.Fatalf("the same picture is the same file: %s", again.Body)
	}
	if entries, _ := os.ReadDir(f.mediaDir); len(entries) != 1 {
		t.Fatalf("stored once: %v", entries)
	}
	got := callSessions(t, s, "GET", out.URL, "")
	if got.Code != 200 || !bytes.Equal(got.Body.Bytes(), shot) || got.Header().Get("Content-Type") != "image/png" || !strings.Contains(got.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("served back: %d %s %q", got.Code, got.Header().Get("Content-Type"), got.Header().Get("Cache-Control"))
	}
	for kind, ext := range map[string]string{"jpeg": ".jpg", "gif": ".gif"} {
		if w := upload(s, "s1", picture(t, kind, 8, 8)); w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), ext+`"`) {
			t.Errorf("%s: %d %s", kind, w.Code, w.Body)
		}
	}
}

func TestAnImageClaudeWouldRefuseIsRefusedAtUpload(t *testing.T) {
	s, f := mediaServer(t)
	for name, tc := range map[string]struct {
		body []byte
		code int
		say  string
	}{
		"too wide":       {picture(t, "png", maxSessionImageSide+1, 10), http.StatusUnsupportedMediaType, "3001 x 10 px"},
		"too tall":       {picture(t, "png", 10, maxSessionImageSide+1), http.StatusUnsupportedMediaType, "a side"},
		"not an image":   {[]byte("#!/bin/sh\nrm -rf /"), http.StatusUnsupportedMediaType, "not a PNG, JPEG or GIF"},
		"a webp header":  {[]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), http.StatusUnsupportedMediaType, "not a PNG, JPEG or GIF"},
		"over the bytes": {append(picture(t, "png", 4, 4), make([]byte, maxSessionImageBytes)...), http.StatusRequestEntityTooLarge, "over 5 MB"},
	} {
		if w := upload(s, "s1", tc.body); w.Code != tc.code || !strings.Contains(w.Body.String(), tc.say) {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if w := upload(s, "s1", picture(t, "png", maxSessionImageSide, maxSessionImageSide)); w.Code != http.StatusCreated {
		t.Errorf("exactly the limit is taken: %d %s", w.Code, w.Body)
	}
	if entries, _ := os.ReadDir(f.mediaDir); len(entries) != 1 {
		t.Fatalf("nothing refused was stored: %v", entries)
	}
	if w := upload(s, "nope", picture(t, "png", 4, 4)); w.Code != http.StatusNotFound {
		t.Fatalf("no such session: %d", w.Code)
	}
}

func TestMediaServesOnlyNamesOfItsOwnShape(t *testing.T) {
	s, f := mediaServer(t)
	os.MkdirAll(f.mediaDir, 0o700)
	os.WriteFile(filepath.Join(filepath.Dir(f.mediaDir), "secret.txt"), []byte("nope"), 0o600)
	os.WriteFile(filepath.Join(f.mediaDir, "notes.txt"), []byte("nope"), 0o600)
	for _, name := range []string{"notes.txt", "..%2Fsecret.txt", "0123456789abcdef0123456789abcdef.png", "0123456789ABCDEF0123456789abcdef.png"} {
		if w := callSessions(t, s, "GET", "/v1/sessions/s1/media/"+name, ""); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if w := callSessions(t, s, "GET", "/v1/sessions/nope/media/0123456789abcdef0123456789abcdef.png", ""); w.Code != http.StatusNotFound {
		t.Fatalf("no such session: %d", w.Code)
	}
}

func TestAPromptTakesTheImagesItNames(t *testing.T) {
	s, f := mediaServer(t)
	var up struct{ Name string }
	json.Unmarshal(upload(s, "s1", picture(t, "png", 20, 20)).Body.Bytes(), &up)

	w := callSessions(t, s, "POST", "/v1/sessions/s1/prompt", `{"text":"what is this?","sender":"pat","images":["`+up.Name+`"]}`)
	if w.Code != http.StatusAccepted || len(f.prompts) != 1 {
		t.Fatalf("%d %s %+v", w.Code, w.Body, f.prompts)
	}
	if p := f.prompts[0]; p.Text != "what is this?" || p.Sender != "pat" || len(p.Images) != 1 || p.Images[0] != up.Name {
		t.Fatalf("the prompt carries its picture's name: %+v", p)
	}
	// A picture with no words is a prompt; words with no picture go the way
	// they always did.
	if w := callSessions(t, s, "POST", "/v1/sessions/s1/prompt", `{"images":["`+up.Name+`"]}`); w.Code != http.StatusAccepted || len(f.prompts) != 2 {
		t.Fatalf("image only: %d %s", w.Code, w.Body)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/s1/prompt", `{"text":"plain"}`); w.Code != http.StatusAccepted || len(f.prompts) != 2 || len(f.sent) != 1 {
		t.Fatalf("text only: %d, %d image prompts, sent %v", w.Code, len(f.prompts), f.sent)
	}
	for name, body := range map[string]string{
		"neither":             `{}`,
		"a name never stored": `{"text":"x","images":["0123456789abcdef0123456789abcdef.png"]}`,
		"a path":              `{"text":"x","images":["../../etc/passwd"]}`,
	} {
		if w := callSessions(t, s, "POST", "/v1/sessions/s1/prompt", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if len(f.prompts) != 2 {
		t.Fatalf("a refused prompt queues nothing: %+v", f.prompts)
	}
	if w := callSessions(t, s, "POST", "/v1/sessions/ended/prompt", `{"text":"x"}`); w.Code != http.StatusConflict {
		t.Fatalf("an ended session: %d", w.Code)
	}
	_ = store.ErrNoSession
}
