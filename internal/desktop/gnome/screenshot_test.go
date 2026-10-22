package gnome

import (
	"image"
	"image/color"
	"testing"

	"github.com/StivenKN/linux-use/internal/desktop"
)

// GNOME renders a mixed-scale layout at the highest scale: a scale-1 monitor
// must come back at its own size, a scale-2 one at full resolution.
func TestCropMixedScales(t *testing.T) {
	ds := []desktop.Display{
		{Name: "A", X: 0, Y: 0, Width: 4, Height: 2, Scale: 1},
		{Name: "B", X: 4, Y: 0, Width: 4, Height: 2, Scale: 2},
	}
	red, blue := color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}
	shot := image.NewRGBA(image.Rect(0, 0, 16, 4)) // 8x2 logical at scale 2
	for y := range 4 {
		for x := range 16 {
			c := blue
			if x < 8 {
				c = red
			}
			shot.SetRGBA(x, y, c)
		}
	}
	for _, tc := range []struct {
		d    desktop.Display
		w, h int
		want color.RGBA
	}{{ds[0], 4, 2, red}, {ds[1], 8, 4, blue}} {
		img := crop(shot, ds, tc.d)
		if got := img.Bounds().Size(); got != image.Pt(tc.w, tc.h) {
			t.Fatalf("%s: size %v, want %dx%d", tc.d.Name, got, tc.w, tc.h)
		}
		for y := range tc.h {
			for x := range tc.w {
				if c := img.RGBAAt(x, y); c != tc.want {
					t.Fatalf("%s: pixel (%d,%d) = %v, want %v", tc.d.Name, x, y, c, tc.want)
				}
			}
		}
	}
}
