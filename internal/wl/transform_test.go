package wl

import (
	"image"
	"image/color"
	"testing"
)

// A 2x1 buffer with a red left pixel: after undoing each transform, red must
// land where the output shows it.
func TestUntransform(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2, 1))
	src.Set(0, 0, color.RGBA{255, 0, 0, 255})
	src.Set(1, 0, color.RGBA{0, 0, 255, 255})
	cases := []struct {
		transform int
		w, h      int
		red       image.Point
	}{
		{0, 2, 1, image.Pt(0, 0)},
		{1, 1, 2, image.Pt(0, 0)}, // 90: rotate back clockwise, left edge goes to top
		{2, 2, 1, image.Pt(1, 0)},
		{3, 1, 2, image.Pt(0, 1)},
		{4, 2, 1, image.Pt(1, 0)}, // flipped
	}
	for _, c := range cases {
		dst := Untransform(src, c.transform)
		if dst.Bounds().Dx() != c.w || dst.Bounds().Dy() != c.h {
			t.Errorf("transform %d: size %v", c.transform, dst.Bounds().Size())
			continue
		}
		if r, _, _, _ := dst.At(c.red.X, c.red.Y).RGBA(); r != 0xffff {
			t.Errorf("transform %d: red not at %v", c.transform, c.red)
		}
	}
}
