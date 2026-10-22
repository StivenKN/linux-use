package x11

import (
	"fmt"
	"image"
	"math/bits"

	"github.com/jezek/xgb/xproto"
)

// pixfmt is how the server lays out ZPixmap pixels of the root window.
type pixfmt struct {
	bpp     int // bits per pixel: 16, 24 or 32
	pad     int // scanline pad in bits
	msb     bool
	r, g, b uint32 // channel masks
}

// rootFormat finds the pixel layout of the root window's visual. Only
// TrueColor and DirectColor visuals are supported, which covers every
// depth 15/16/24/30/32 server in use.
func rootFormat(s *xproto.SetupInfo, scr *xproto.ScreenInfo) (pixfmt, error) {
	f := pixfmt{msb: s.ImageByteOrder == xproto.ImageOrderMSBFirst}
	for _, pf := range s.PixmapFormats {
		if pf.Depth == scr.RootDepth {
			f.bpp, f.pad = int(pf.BitsPerPixel), int(pf.ScanlinePad)
		}
	}
	for _, d := range scr.AllowedDepths {
		for _, v := range d.Visuals {
			if v.VisualId != scr.RootVisual {
				continue
			}
			if v.Class != xproto.VisualClassTrueColor && v.Class != xproto.VisualClassDirectColor {
				return f, fmt.Errorf("root visual class %d is not TrueColor", v.Class)
			}
			f.r, f.g, f.b = v.RedMask, v.GreenMask, v.BlueMask
		}
	}
	if f.bpp != 16 && f.bpp != 24 && f.bpp != 32 || f.r == 0 || f.pad == 0 {
		return f, fmt.Errorf("unsupported root pixel format: depth %d, %d bpp", scr.RootDepth, f.bpp)
	}
	return f, nil
}

// stride is the length in bytes of one image row w pixels wide.
func (f pixfmt) stride(w int) int {
	return (w*f.bpp + f.pad - 1) / f.pad * f.pad / 8
}

// decode writes the rows in data, a ZPixmap image as wide as dst, into dst
// starting at row y0. Pixels come out opaque.
func (f pixfmt) decode(dst *image.RGBA, y0 int, data []byte) {
	w := dst.Rect.Dx()
	stride := f.stride(w)
	rows := min(len(data)/stride, dst.Rect.Dy()-y0)
	// Fast path: little-endian BGRX, what nearly every server sends.
	if f.bpp == 32 && !f.msb && f.r == 0xff0000 && f.g == 0xff00 && f.b == 0xff {
		for y := range rows {
			src := data[y*stride : y*stride+w*4]
			out := dst.Pix[(y0+y)*dst.Stride:]
			for i := 0; i < len(src); i += 4 {
				out[i], out[i+1], out[i+2], out[i+3] = src[i+2], src[i+1], src[i], 0xff
			}
		}
		return
	}
	r, g, b := newChannel(f.r), newChannel(f.g), newChannel(f.b)
	n := f.bpp / 8
	for y := range rows {
		row := data[y*stride:]
		out := dst.Pix[(y0+y)*dst.Stride:]
		for x := range w {
			var p uint32
			for i, c := range row[x*n : x*n+n] {
				if f.msb {
					p = p<<8 | uint32(c)
				} else {
					p |= uint32(c) << (8 * i)
				}
			}
			out[x*4], out[x*4+1], out[x*4+2], out[x*4+3] = r.get(p), g.get(p), b.get(p), 0xff
		}
	}
}

// channel extracts one color component from a pixel, scaled to 8 bits.
type channel struct {
	mask         uint32
	shift, width int
}

func newChannel(mask uint32) channel {
	return channel{mask, bits.TrailingZeros32(mask), bits.OnesCount32(mask)}
}

func (c channel) get(p uint32) uint8 {
	v := (p & c.mask) >> c.shift
	if c.width >= 8 {
		return uint8(v >> (c.width - 8))
	}
	return uint8(v * 255 / (1<<c.width - 1))
}
