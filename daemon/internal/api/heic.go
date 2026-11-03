package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// A HEIC photo (what iPhones and Macs take) is no format Claude reads, and
// Chrome cannot open one to redraw it as PNG the way Safari does (web/paste.js).
// So the host converts it: the web lens sends the HEIC bytes as they are, and
// the paste lands as a PNG made by whichever converter this host has.

// heifBrands are the ISO-BMFF major brands of HEIC/HEIF stills (mif1 is the
// generic image brand, also used by AVIF, which the same tools read).
var heifBrands = map[string]bool{
	"heic": true, "heix": true, "hevc": true, "hevx": true,
	"heim": true, "heis": true, "mif1": true, "msf1": true,
}

// isHEIF reports bytes that open with an ftyp box naming a HEIF brand.
func isHEIF(b []byte) bool {
	return len(b) >= 12 && string(b[4:8]) == "ftyp" && heifBrands[string(b[8:12])]
}

// heifConverter is one command that turns a HEIF file into a PNG file.
type heifConverter struct {
	name string
	args func(in, out string) []string
}

// heifConverters, tried in order: sips ships with macOS; on Linux,
// ImageMagick or heif-convert (libheif-examples), both of which need
// libheif's HEVC decoder (the libheif-plugin-libde265 package). A HEIC can
// hold several images (a burst, a depth map); ImageMagick then writes
// out-0.png, out-1.png and no out.png, so it is asked for the first, [0].
var heifConverters = []heifConverter{
	{"sips", func(in, out string) []string { return []string{"-s", "format", "png", in, "--out", out} }},
	{"magick", func(in, out string) []string { return []string{in + "[0]", out} }},
	{"convert", func(in, out string) []string { return []string{in + "[0]", out} }},
	{"heif-convert", func(in, out string) []string { return []string{in, out} }},
}

// The seams tests swap: finding a command, and running one.
var (
	lookPath = exec.LookPath
	runCmd   = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
)

const heifTimeout = 20 * time.Second

// errNoHEIF is what a host with no converter installed answers. The lens
// shows it to the person who pasted, so it names the fix.
var errNoHEIF = errors.New("this host has no HEIC converter (" + heifFix() + ")")

// heifFix is the advice for this host's system: macOS always has sips.
func heifFix() string {
	if runtime.GOOS == "darwin" {
		return "sips should be in /usr/bin"
	}
	return "install libheif-plugin-libde265"
}

// convertHEIF lands a HEIF upload in dir as a PNG and returns the PNG's path.
// The HEIF file it converts from is removed either way.
func convertHEIF(ctx context.Context, dir string, b []byte) (string, error) {
	in, err := writePaste(dir, b, ".heic")
	if err != nil {
		return "", err
	}
	defer os.Remove(in)
	out := strings.TrimSuffix(in, ".heic") + ".png"
	ctx, cancel := context.WithTimeout(ctx, heifTimeout)
	defer cancel()
	var failed []string
	for _, c := range heifConverters {
		if _, err := lookPath(c.name); err != nil {
			continue
		}
		if why := runHEIF(ctx, c, in, out); why != "" {
			failed = append(failed, why)
			continue
		}
		return out, nil
	}
	if len(failed) == 0 {
		return "", errNoHEIF
	}
	// Every converter there was failed. The last one's reason goes back to
	// the person who pasted; the rest are in the log.
	return "", fmt.Errorf("the HEIC image could not be converted (%s); if every HEIC fails, %s",
		failed[len(failed)-1], heifFix())
}

// runHEIF runs one converter and says why it failed, or "" when out is now a
// PNG. Each failure is logged with the converter's own output.
func runHEIF(ctx context.Context, c heifConverter, in, out string) string {
	output, err := runCmd(ctx, c.name, c.args(in, out)...)
	if err == nil && isPNGFile(out) {
		return ""
	}
	removeOutputs(out)
	why := strings.TrimSpace(string(output))
	if err != nil {
		why = strings.TrimSpace(err.Error() + ": " + why)
	}
	if why == "" {
		why = "it wrote no PNG"
	}
	if len(why) > 200 {
		why = why[:200] + "…"
	}
	log.Printf("paste: %s could not convert a HEIC image: %s", c.name, why)
	return c.name + ": " + why
}

// removeOutputs removes what a failed conversion wrote: out, and the numbered
// out-N.png files a converter writes for a HEIC with several images.
func removeOutputs(out string) {
	_ = os.Remove(out)
	numbered, _ := filepath.Glob(strings.TrimSuffix(out, ".png") + "-*.png")
	for _, f := range numbered {
		_ = os.Remove(f)
	}
}

// isPNGFile reports a file a converter wrote that really is a PNG.
func isPNGFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 8)
	n, _ := f.Read(head)
	return sniffImageExt(head[:n]) == ".png"
}
