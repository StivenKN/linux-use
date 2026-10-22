package x11

import (
	"image"
	"image/color"
	"testing"
)

// TestDecode checks ZPixmap decoding for the layouts servers send: the
// common little-endian BGRX, big-endian servers, packed 24 bpp and 16 bpp
// RGB565, with scanline padding and a non-zero start row.
func TestDecode(t *testing.T) {
	red, green, blue := color.RGBA{255, 0, 0, 255}, color.RGBA{0, 255, 0, 255}, color.RGBA{0, 0, 255, 255}
	tests := []struct {
		name string
		f    pixfmt
		data []byte // one row: red, green, blue
	}{
		{"bgrx", pixfmt{bpp: 32, pad: 32, r: 0xff0000, g: 0xff00, b: 0xff},
			[]byte{0, 0, 255, 0, 0, 255, 0, 0, 255, 0, 0, 0}},
		{"msb xrgb", pixfmt{bpp: 32, pad: 32, msb: true, r: 0xff0000, g: 0xff00, b: 0xff},
			[]byte{0, 255, 0, 0, 0, 0, 255, 0, 0, 0, 0, 255}},
		{"packed 24", pixfmt{bpp: 24, pad: 32, r: 0xff0000, g: 0xff00, b: 0xff},
			[]byte{0, 0, 255, 0, 255, 0, 255, 0, 0, 0, 0, 0}},
		{"rgb565", pixfmt{bpp: 16, pad: 32, r: 0xf800, g: 0x07e0, b: 0x001f},
			[]byte{0x00, 0xf8, 0xe0, 0x07, 0x1f, 0x00, 0, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, want := tt.f.stride(3), len(tt.data); got != want {
				t.Fatalf("stride(3) = %d, want %d", got, want)
			}
			img := image.NewRGBA(image.Rect(0, 0, 3, 2))
			tt.f.decode(img, 1, tt.data)
			for x, want := range []color.RGBA{red, green, blue} {
				if got := img.RGBAAt(x, 1); got != want {
					t.Errorf("pixel %d = %v, want %v", x, got, want)
				}
				if got := img.RGBAAt(x, 0); got != (color.RGBA{}) {
					t.Errorf("row 0 pixel %d = %v, want untouched", x, got)
				}
			}
		})
	}
}
