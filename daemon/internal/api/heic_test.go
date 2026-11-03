package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// heicMagic opens like an iPhone photo: an ftyp box with the heic brand.
var heicMagic = []byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00mif1heic")

func TestIsHEIF(t *testing.T) {
	cases := map[string]bool{
		string(heicMagic):                 true,
		"\x00\x00\x00\x18ftypmif1rest":    true,
		"\x00\x00\x00\x18ftypisomrest":    false, // an mp4 video
		string(pngMagic):                  false,
		"\x00\x00\x00\x18ftyp":            false, // too short to carry a brand
		"\x00\x00\x00\x18moovheicrestxxx": false, // no ftyp box
	}
	for in, want := range cases {
		if got := isHEIF([]byte(in)); got != want {
			t.Errorf("isHEIF(%q) = %v, want %v", in, got, want)
		}
	}
}

// fakeConverters makes only the named commands exist; each "converts" by
// writing out what write returns (nil writes nothing) and failing with fail.
func fakeConverters(t *testing.T, have map[string]bool, write func(name string) []byte, fail error) *[]string {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	ran := []string{}
	oldLook, oldRun := lookPath, runCmd
	t.Cleanup(func() { lookPath, runCmd = oldLook, oldRun })
	lookPath = func(name string) (string, error) {
		if have[name] {
			return "/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	runCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		ran = append(ran, name)
		if b := write(name); b != nil {
			_ = os.WriteFile(args[len(args)-1], b, 0o600)
		}
		return nil, fail
	}
	return &ran
}

func TestHEICLandsAsPNG(t *testing.T) {
	ran := fakeConverters(t, map[string]bool{"convert": true}, func(string) []byte { return pngMagic }, nil)
	path, code, err := landPaste(context.Background(), heicMagic)
	if err != nil || code != 0 {
		t.Fatalf("landPaste = %q, %d, %v", path, code, err)
	}
	if filepath.Ext(path) != ".png" || !isPNGFile(path) {
		t.Fatalf("landed %q, want a PNG", path)
	}
	if strings.Join(*ran, ",") != "convert" {
		t.Errorf("ran %v, want only the converter this host has", *ran)
	}
	left, _ := filepath.Glob(filepath.Join(pasteDir(), "*.heic"))
	if len(left) != 0 {
		t.Errorf("the HEIC it converted from is still there: %v", left)
	}
}

func TestHEICTriesTheNextConverter(t *testing.T) {
	// convert is present but writes nothing (no HEVC decoder); heif-convert works.
	write := func(name string) []byte {
		if name == "heif-convert" {
			return pngMagic
		}
		return nil
	}
	ran := fakeConverters(t, map[string]bool{"heif-convert": true, "convert": true}, write, nil)
	path, _, err := landPaste(context.Background(), heicMagic)
	if err != nil || !isPNGFile(path) {
		t.Fatalf("landPaste = %q, %v", path, err)
	}
	if strings.Join(*ran, ",") != "convert,heif-convert" {
		t.Errorf("ran %v", *ran)
	}
}

func TestHEICWithNoConverterNamesTheFix(t *testing.T) {
	fakeConverters(t, map[string]bool{}, func(string) []byte { return nil }, nil)
	_, code, err := landPaste(context.Background(), heicMagic)
	if code != http.StatusUnsupportedMediaType || err == nil || !strings.Contains(err.Error(), "no HEIC converter") ||
		!strings.Contains(err.Error(), heifInstall()) {
		t.Errorf("landPaste = %d, %v", code, err)
	}
	if files, _ := os.ReadDir(pasteDir()); len(files) != 0 {
		t.Errorf("left files behind: %d", len(files))
	}
}

// A converter that is there but fails says why, in its own words, rather
// than claiming nothing is installed.
func TestHEICConverterFailureSaysWhy(t *testing.T) {
	ran := fakeConverters(t, map[string]bool{"heif-convert": true, "convert": true}, func(string) []byte { return nil }, nil)
	runCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		*ran = append(*ran, name)
		return []byte("Unsupported feature: Unsupported codec\n"), errors.New("exit status 1")
	}
	_, code, err := landPaste(context.Background(), heicMagic)
	if code != http.StatusUnsupportedMediaType || err == nil {
		t.Fatalf("landPaste = %d, %v", code, err)
	}
	msg := err.Error()
	for _, want := range []string{"heif-convert: exit status 1: Unsupported feature: Unsupported codec", heifFix()} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "no HEIC converter") {
		t.Errorf("error %q claims nothing is installed", msg)
	}
	if strings.Join(*ran, ",") != "convert,heif-convert" {
		t.Errorf("ran %v, want every converter tried", *ran)
	}
	if files, _ := os.ReadDir(pasteDir()); len(files) != 0 {
		t.Errorf("left files behind: %d", len(files))
	}
}

func TestPNGUploadIsNotConverted(t *testing.T) {
	ran := fakeConverters(t, map[string]bool{"convert": true}, func(string) []byte { return pngMagic }, nil)
	path, _, err := landPaste(context.Background(), pngMagic)
	if err != nil || filepath.Ext(path) != ".png" || len(*ran) != 0 {
		t.Fatalf("landPaste = %q, %v; ran %v", path, err, *ran)
	}
}

// A HEIC with several images: ImageMagick is asked for the first one only,
// and the numbered files a converter writes instead of out.png are cleared.
func TestHEICWithSeveralImages(t *testing.T) {
	var gotArgs []string
	fakeConverters(t, map[string]bool{"convert": true}, func(string) []byte { return nil }, nil)
	runCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotArgs = args
		out := args[len(args)-1]
		base := strings.TrimSuffix(out, ".png")
		_ = os.WriteFile(base+"-0.png", pngMagic, 0o600) // what convert writes without [0]
		_ = os.WriteFile(base+"-1.png", pngMagic, 0o600)
		return nil, nil
	}
	_, _, err := landPaste(context.Background(), heicMagic)
	if err == nil || !strings.Contains(err.Error(), "wrote no PNG") {
		t.Fatalf("landPaste = %v", err)
	}
	if !strings.HasSuffix(gotArgs[0], ".heic[0]") {
		t.Errorf("convert was given %q, want the first image, [0]", gotArgs[0])
	}
	if files, _ := os.ReadDir(pasteDir()); len(files) != 0 {
		t.Errorf("left %d numbered files behind", len(files))
	}
}

// A paste dir the daemon cannot write is the host's trouble, a 500, not a
// HEIC the host cannot read (415).
func TestHEICWriteFailureIsTheHosts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through a read-only dir")
	}
	fakeConverters(t, map[string]bool{"convert": true}, func(string) []byte { return pngMagic }, nil)
	if err := os.MkdirAll(pasteDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(pasteDir(), 0o700) })
	_, code, err := landPaste(context.Background(), heicMagic)
	if code != http.StatusInternalServerError || err == nil {
		t.Errorf("landPaste = %d, %v; want 500", code, err)
	}
	var bad heifError
	if errors.As(err, &bad) {
		t.Errorf("a write failure was reported as a conversion failure: %v", err)
	}
}
