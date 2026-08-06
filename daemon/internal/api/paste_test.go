package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

var pngMagic = []byte("\x89PNG\r\n\x1a\nfakebody")

func TestSniffImageExt(t *testing.T) {
	cases := map[string]string{
		string(pngMagic):             ".png",
		"\xff\xd8\xffdata":           ".jpg",
		"GIF89adata":                 ".gif",
		"RIFF\x00\x00\x00\x00WEBPvp": ".webp",
		"plain text":                 "",
		"":                           "",
	}
	for in, want := range cases {
		if got := sniffImageExt([]byte(in)); got != want {
			t.Errorf("sniff(%q) = %q, want %q", in[:min(8, len(in))], got, want)
		}
	}
}

func TestPasteImageRoute(t *testing.T) {
	base, wsID, _ := filesStack(t)
	pasteURL := base + "/v1/workspaces/" + wsID + "/paste"

	// A PNG upload lands on disk and echoes its path.
	resp, err := http.Post(pasteURL, "application/octet-stream", bytes.NewReader(pngMagic))
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Path string }
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 || got.Path == "" {
		t.Fatalf("paste: status %d path %q", resp.StatusCode, got.Path)
	}
	t.Cleanup(func() { _ = os.Remove(got.Path) })
	b, err := os.ReadFile(got.Path)
	if err != nil || !bytes.Equal(b, pngMagic) {
		t.Fatalf("paste content: %v %q", err, b)
	}

	// Non-image bytes are refused.
	resp, err = http.Post(pasteURL, "application/octet-stream", bytes.NewReader([]byte("not an image")))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 415 {
		t.Fatalf("non-image: status %d, want 415", resp.StatusCode)
	}

	// Unknown workspace 404s.
	resp, err = http.Post(base+"/v1/workspaces/nope/paste", "application/octet-stream", bytes.NewReader(pngMagic))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown ws: status %d, want 404", resp.StatusCode)
	}
}

func TestSweepStalePastes(t *testing.T) {
	dir := t.TempDir()
	old := dir + "/paste-old.png"
	fresh := dir + "/paste-fresh.png"
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-pasteMaxAge - time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	sweepStalePastes(dir)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("stale paste survived the sweep")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh paste was swept")
	}
}
