package computer

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"math"

	"golang.org/x/image/draw"

	"github.com/StivenKN/linux-use/internal/desktop"
)

// Limits for images sent to the model. The API downsamples anything larger,
// which would silently break coordinate mapping, so we scale first and keep
// the exact factor.
var (
	MaxLongEdge = 1568
	MaxPixels   = 1_150_000
)

// Shot remembers how the last screenshot maps onto the screen: the model
// speaks in image pixels, input backends in logical screen coordinates.
type Shot struct {
	Display desktop.Display
	Width   int // image size the model saw
	Height  int
}

// fit returns the largest size within the model limits preserving aspect.
func fit(w, h int) (int, int) {
	s := 1.0
	if long := max(w, h); long > MaxLongEdge {
		s = float64(MaxLongEdge) / float64(long)
	}
	if px := float64(w) * float64(h) * s * s; px > float64(MaxPixels) {
		s *= math.Sqrt(float64(MaxPixels) / px)
	}
	return scaled(w, s), scaled(h, s)
}

// scaled floors n*s, tolerating float error so 3000*(1568/3000) is 1568.
func scaled(n int, s float64) int { return max(1, int(float64(n)*s+1e-6)) }

// zoomSize magnifies a w×h region up to 4x while staying within model limits.
func zoomSize(w, h int) (int, int) {
	s := min(4.0,
		float64(MaxLongEdge)/float64(max(w, h)),
		math.Sqrt(float64(MaxPixels)/(float64(w)*float64(h))))
	return scaled(w, s), scaled(h, s)
}

// shotFor returns the geometry a screenshot of d would have.
func shotFor(d desktop.Display) Shot {
	w, h := fit(d.Width, d.Height)
	return Shot{Display: d, Width: w, Height: h}
}

// ToScreen converts image coordinates to global logical screen coordinates.
func (s Shot) ToScreen(x, y float64) (int, int) {
	sx := float64(s.Display.Width) / float64(s.Width)
	sy := float64(s.Display.Height) / float64(s.Height)
	return s.Display.X + int(math.Round(x*sx)), s.Display.Y + int(math.Round(y*sy))
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

// encode renders img as PNG or JPEG.
func encode(img image.Image, format string, quality int) ([]byte, string, error) {
	var buf bytes.Buffer
	if format == "jpeg" {
		err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
		return buf.Bytes(), "image/jpeg", err
	}
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	err := enc.Encode(&buf, img)
	return buf.Bytes(), "image/png", err
}
