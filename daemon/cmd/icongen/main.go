// Command icongen renders the ccmux PWA icon set into daemon/web/icons. It draws
// a ">_" prompt mark (brand green on the app's dark background) so the icons are
// reproducible from source rather than committed as opaque binaries with no
// provenance. Run from the daemon module root: `go run ./cmd/icongen`.
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
)

var (
	bg    = color.NRGBA{0x16, 0x18, 0x1b, 0xff} // --bg2
	green = color.NRGBA{0x72, 0xb9, 0x79, 0xff} // --accent
	white = color.NRGBA{0xff, 0xff, 0xff, 0xff}
)

func main() {
	outDir := "web/icons"
	if len(os.Args) > 1 {
		outDir = os.Args[1]
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Fatal(err)
	}
	// square icons (any-purpose): tight inset so the mark fills the tile.
	write(outDir, "icon-192.png", render(192, 0.14, bg, green, true))
	write(outDir, "icon-512.png", render(512, 0.14, bg, green, true))
	// maskable: content kept inside the center ~64% safe zone (0.18 inset per side).
	write(outDir, "icon-512-maskable.png", render(512, 0.22, bg, green, true))
	// iOS home screen: opaque background, no rounding (iOS applies its own mask).
	write(outDir, "apple-touch-icon.png", render(180, 0.14, bg, green, false))
	// Android notification badge: monochrome glyph, transparent background.
	write(outDir, "badge-72.png", render(72, 0.12, color.NRGBA{}, white, false))
}

// render draws the ccmux mark at side px. inset is the fractional padding around
// the content box. bgCol fills the tile (transparent if alpha 0); fgCol draws the
// glyph. rounded rounds the tile corners (ignored when bg is transparent).
func render(side int, inset float64, bgCol, fgCol color.NRGBA, rounded bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, side, side))
	s := float64(side)
	if bgCol.A > 0 {
		r := 0.0
		if rounded {
			r = s * 0.22
		}
		fillRoundRect(img, 0, 0, s, s, r, bgCol)
	}

	pad := s * inset
	cx0, cy0, cx1 := pad, pad, s-pad
	box := cx1 - cx0
	w := box * 0.11 // stroke width

	// ">" chevron: down-stroke to the tip, then up-stroke.
	lx := cx0 + box*0.16
	tip := cx0 + box*0.46
	top := cy0 + box*0.26
	mid := cy0 + box*0.50
	bot := cy0 + box*0.74
	thickLine(img, lx, top, tip, mid, w, fgCol)
	thickLine(img, lx, bot, tip, mid, w, fgCol)

	// "_" underscore cursor.
	uy := cy0 + box*0.74
	thickLine(img, cx0+box*0.56, uy, cx1-box*0.06, uy, w, fgCol)

	return img
}

func write(dir, name string, img image.Image) {
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s", filepath.Join(dir, name))
}

// --- tiny anti-aliased rasterizer ---

func fillRoundRect(img *image.NRGBA, x0, y0, x1, y1, r float64, c color.NRGBA) {
	for y := int(y0); y < int(y1); y++ {
		for x := int(x0); x < int(x1); x++ {
			if a := roundRectCoverage(float64(x)+0.5, float64(y)+0.5, x0, y0, x1, y1, r); a > 0 {
				blend(img, x, y, c, a)
			}
		}
	}
}

// roundRectCoverage returns 1 inside the rounded rect, 0 outside, with a 1px AA
// ramp at the corner arcs.
func roundRectCoverage(px, py, x0, y0, x1, y1, r float64) float64 {
	if r <= 0 {
		if px >= x0 && px < x1 && py >= y0 && py < y1 {
			return 1
		}
		return 0
	}
	// distance into the corner region
	dx := math.Max(math.Max(x0+r-px, px-(x1-r)), 0)
	dy := math.Max(math.Max(y0+r-py, py-(y1-r)), 0)
	d := math.Hypot(dx, dy)
	return clamp(r+0.5-d, 0, 1)
}

func thickLine(img *image.NRGBA, x0, y0, x1, y1, width float64, c color.NRGBA) {
	half := width / 2
	minX := int(math.Floor(math.Min(x0, x1) - half - 1))
	maxX := int(math.Ceil(math.Max(x0, x1) + half + 1))
	minY := int(math.Floor(math.Min(y0, y1) - half - 1))
	maxY := int(math.Ceil(math.Max(y0, y1) + half + 1))
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			d := distToSegment(float64(x)+0.5, float64(y)+0.5, x0, y0, x1, y1)
			if a := clamp(half+0.5-d, 0, 1); a > 0 {
				blend(img, x, y, c, a)
			}
		}
	}
}

func distToSegment(px, py, x0, y0, x1, y1 float64) float64 {
	vx, vy := x1-x0, y1-y0
	wx, wy := px-x0, py-y0
	c1 := vx*wx + vy*wy
	if c1 <= 0 {
		return math.Hypot(wx, wy)
	}
	c2 := vx*vx + vy*vy
	if c2 <= c1 {
		return math.Hypot(px-x1, py-y1)
	}
	t := c1 / c2
	return math.Hypot(px-(x0+t*vx), py-(y0+t*vy))
}

func blend(img *image.NRGBA, x, y int, c color.NRGBA, a float64) {
	if x < 0 || y < 0 || x >= img.Bounds().Dx() || y >= img.Bounds().Dy() {
		return
	}
	src := c
	src.A = uint8(float64(c.A) * a)
	dst := img.NRGBAAt(x, y)
	sa := float64(src.A) / 255
	da := float64(dst.A) / 255
	outA := sa + da*(1-sa)
	if outA == 0 {
		img.SetNRGBA(x, y, color.NRGBA{})
		return
	}
	mix := func(s, d uint8) uint8 {
		return uint8((float64(s)*sa + float64(d)*da*(1-sa)) / outA)
	}
	img.SetNRGBA(x, y, color.NRGBA{mix(src.R, dst.R), mix(src.G, dst.G), mix(src.B, dst.B), uint8(outA * 255)})
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }
