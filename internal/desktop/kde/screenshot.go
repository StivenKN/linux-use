package kde

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	shotService = "org.kde.KWin.ScreenShot2"
	shotPath    = "/org/kde/KWin/ScreenShot2"
)

// QImage::Format values KWin may hand back. The ARGB32 family stores each
// pixel as a native-endian 0xAARRGGBB word (B, G, R, A in memory on the
// little-endian machines we run on); the RGBA8888 family stores R, G, B, A.
const (
	qimageRGB32                 = 4
	qimageARGB32                = 5
	qimageARGB32Premultiplied   = 6
	qimageRGBX8888              = 16
	qimageRGBA8888              = 17
	qimageRGBA8888Premultiplied = 18
)

// screenshot captures one output through KWin's ScreenShot2 D-Bus interface,
// at its native (physical) resolution and without the cursor. KWin replies
// with the image metadata, then writes the raw pixels into the pipe.
func screenshot(ctx context.Context, bus *dbus.Conn, output string) (*image.RGBA, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	opts := map[string]dbus.Variant{"native-resolution": dbus.MakeVariant(true)}
	var meta map[string]dbus.Variant
	err = bus.Object(shotService, shotPath).
		CallWithContext(ctx, shotService+".CaptureScreen", 0, output, opts, dbus.UnixFD(w.Fd())).
		Store(&meta)
	w.Close() // KWin holds its own copy; ours would keep the read below from seeing EOF
	if err != nil {
		var de dbus.Error
		if errors.As(err, &de) && de.Name == shotService+".Error.NoAuthorized" {
			return nil, fmt.Errorf("KWin refused the screenshot: %v (see the entry linux-use installs in ~/.local/share/applications)", err)
		}
		return nil, fmt.Errorf("KWin screenshot: %w", err)
	}

	num := func(key string) int {
		v, _ := meta[key].Value().(uint32)
		return int(v)
	}
	width, height, stride, format := num("width"), num("height"), num("stride"), num("format")
	if width == 0 || height == 0 || stride < width*4 {
		return nil, fmt.Errorf("KWin screenshot: unexpected metadata %v", meta)
	}
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	r.SetReadDeadline(deadline)
	buf := make([]byte, stride*height)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("KWin screenshot: reading pixels: %w", err)
	}
	return decode(buf, width, height, stride, format)
}

// decode converts KWin's raw QImage bytes into an opaque RGBA image.
func decode(buf []byte, width, height, stride, format int) (*image.RGBA, error) {
	var bgra bool
	switch format {
	case qimageRGB32, qimageARGB32, qimageARGB32Premultiplied:
		bgra = true
	case qimageRGBX8888, qimageRGBA8888, qimageRGBA8888Premultiplied:
	default:
		return nil, fmt.Errorf("KWin screenshot: unsupported QImage format %d", format)
	}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		src := buf[y*stride : y*stride+width*4]
		dst := img.Pix[y*img.Stride : y*img.Stride+width*4]
		for i := 0; i < len(src); i += 4 {
			if bgra {
				dst[i], dst[i+1], dst[i+2] = src[i+2], src[i+1], src[i]
			} else {
				dst[i], dst[i+1], dst[i+2] = src[i], src[i+1], src[i+2]
			}
			dst[i+3] = 0xff
		}
	}
	return img, nil
}
