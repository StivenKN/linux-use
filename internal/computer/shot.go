package computer

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"math"

	"golang.org/x/image/draw"

	"github.com/StivenKN/linux-use/internal/desktop"
)

// Screenshot budget, matching Claude Code's built-in computer use: the long
// edge stays within 1568 px and the image within 1568 tokens of 28×28 px
// tiles. Images over budget get resized by the harness, which would break
// coordinate mapping, so we size them ourselves and keep the exact factor.
const (
	maxTargetPx     = 1568
	maxTargetTokens = 1568
	pxPerToken      = 28
)

// Shot remembers how the last screenshot maps onto the screen: the model
// speaks in image pixels, input backends in logical screen coordinates.
type Shot struct {
	Display desktop.Display
	Width   int // image size the model saw
	Height  int
}

func tokens(w, h int) int { return ((w-1)/pxPerToken + 1) * ((h-1)/pxPerToken + 1) }

// fit returns the largest size within budget that keeps the aspect ratio,
// by binary search on the long edge (the same search Claude Code runs).
func fit(w, h int) (int, int) {
	if w <= maxTargetPx && h <= maxTargetPx && tokens(w, h) <= maxTargetTokens {
		return w, h
	}
	if h > w {
		a, b := fit(h, w)
		return b, a
	}
	ratio := float64(w) / float64(h)
	short := func(long int) int { return max(int(math.Round(float64(long)/ratio)), 1) }
	lo, hi := 1, w
	for lo+1 < hi {
		mid := (lo + hi) / 2
		if mid <= maxTargetPx && tokens(mid, short(mid)) <= maxTargetTokens {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo, short(lo)
}

// zoomSize sizes a zoomed region: native pixels, magnified up to 2x so small
// text spans more of the model's tiles, within the same budget.
func zoomSize(w, h int) (int, int) {
	return fit(w*2, h*2)
}

// shotFor returns the geometry a screenshot of d would have. Like Claude
// Code, it fits the physical pixels, so HiDPI screens keep more detail.
func shotFor(d desktop.Display) Shot {
	scale := d.Scale
	if scale <= 0 {
		scale = 1
	}
	w, h := fit(int(math.Round(float64(d.Width)*scale)), int(math.Round(float64(d.Height)*scale)))
	return Shot{Display: d, Width: w, Height: h}
}

// ToScreen converts image coordinates to global logical screen coordinates,
// clamped to the display.
func (s Shot) ToScreen(x, y float64) (int, int) {
	sx := float64(s.Display.Width) / float64(s.Width)
	sy := float64(s.Display.Height) / float64(s.Height)
	lx := min(max(int(math.Round(x*sx)), 0), s.Display.Width-1)
	ly := min(max(int(math.Round(y*sy)), 0), s.Display.Height-1)
	return s.Display.X + lx, s.Display.Y + ly
}

// Check rejects coordinates outside the screenshot.
func (s Shot) Check(p Point) error {
	if p.X < 0 || p.Y < 0 || p.X > float64(s.Width) || p.Y > float64(s.Height) {
		return fmt.Errorf("coordinate [%g, %g] is outside the coordinate frame (%dx%d) — coordinates are pixels in the most recent screenshot. Take a new screenshot and pick a point inside it.", p.X, p.Y, s.Width, s.Height)
	}
	return nil
}

// FromScreen converts global logical coordinates to image coordinates.
func (s Shot) FromScreen(x, y int) (int, int) {
	sx := float64(s.Width) / float64(s.Display.Width)
	sy := float64(s.Height) / float64(s.Display.Height)
	return int(math.Round(float64(x-s.Display.X) * sx)), int(math.Round(float64(y-s.Display.Y) * sy))
}

// resize scales src to exactly w×h. Downscaling uses Catmull-Rom so small
// UI text stays legible.
func resize(src image.Image, w, h int) image.Image {
	if b := src.Bounds(); b.Dx() == w && b.Dy() == h {
		return src
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	return dst
}

// encode renders img as JPEG at quality 75, like the built-in tools.
func encode(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 75})
	return buf.Bytes(), err
}
