package wl

import "image"

// Untransform turns a captured buffer into what the output shows: buffers
// arrive in the output's native orientation, and wl_output.transform says
// how the compositor flips and rotates them (counter-clockwise) for display.
func Untransform(src *image.RGBA, transform int) *image.RGBA {
	if transform == 0 {
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	quarter := transform % 4
	dw, dh := w, h
	if quarter%2 == 1 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			dx := x
			if transform >= 4 {
				dx = dw - 1 - x // mirror after rotating back
			}
			var sx, sy int
			switch quarter {
			case 0:
				sx, sy = dx, y
			case 1: // rotate clockwise
				sx, sy = y, h-1-dx
			case 2:
				sx, sy = w-1-dx, h-1-y
			case 3: // rotate counter-clockwise
				sx, sy = w-1-y, dx
			}
			si, di := src.PixOffset(sx, sy), dst.PixOffset(x, y)
			copy(dst.Pix[di:di+4], src.Pix[si:si+4])
		}
	}
	return dst
}
