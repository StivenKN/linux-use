package wl

import (
	"context"
	"errors"
	"fmt"
	"image"
	"slices"
	"sync"
)

type shmFormat struct {
	format              uint32
	width, height, size int
	stride              int
}

var readableFormats = []uint32{FormatXRGB8888, FormatARGB8888, FormatXBGR8888, FormatABGR8888}

// CaptureWLR grabs one frame of an output through zwlr_screencopy_manager_v1.
func CaptureWLR(ctx context.Context, c *Conn, mgr, shm, output uint32, cursor bool) (*image.RGBA, error) {
	var (
		mu      sync.Mutex
		offers  []shmFormat
		yInvert bool
		done    = make(chan struct{})
		failed  bool
	)
	frame := c.NewObject(func(op uint16, e *Event) {
		mu.Lock()
		defer mu.Unlock()
		switch op {
		case 0: // buffer
			f := shmFormat{format: e.Uint(), width: int(e.Uint()), height: int(e.Uint()), stride: int(e.Uint())}
			offers = append(offers, f)
		case 1: // flags
			yInvert = e.Uint()&1 != 0
		case 2: // ready
			close(done)
		case 3: // failed
			failed = true
			close(done)
		}
	})
	defer c.Request(frame, 1).Send() // destroy

	overlay := int32(0)
	if cursor {
		overlay = 1
	}
	if err := c.Request(mgr, 0).Uint(frame).Int(overlay).Uint(output).Send(); err != nil {
		return nil, err
	}
	// Buffer constraints are sent immediately; a round trip collects them for
	// every protocol version (v3 adds buffer_done, older ones do not).
	if err := c.Roundtrip(ctx); err != nil {
		return nil, err
	}
	mu.Lock()
	pick, ok := pickFormat(offers)
	mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("screencopy offered no readable shm format (%v)", offers)
	}
	buf, err := NewShmBuffer(c, shm, pick.width, pick.height, pick.stride, pick.format)
	if err != nil {
		return nil, err
	}
	defer buf.Destroy()
	if err := c.Request(frame, 0).Uint(buf.ID).Send(); err != nil {
		return nil, err
	}
	if err := c.Wait(ctx, done); err != nil {
		return nil, err
	}
	if failed {
		return nil, errors.New("compositor failed to copy the screen")
	}
	return toRGBA(buf, yInvert), nil
}

// CaptureExt grabs one frame through ext-image-copy-capture-v1, the
// standardized successor of wlr-screencopy.
func CaptureExt(ctx context.Context, c *Conn, srcMgr, copyMgr, shm, output uint32, cursor bool) (*image.RGBA, error) {
	source := c.NewObject(nil)
	if err := c.Request(srcMgr, 0).Uint(source).Uint(output).Send(); err != nil {
		return nil, err
	}
	defer c.Request(source, 0).Send()

	var (
		mu            sync.Mutex
		width, height int
		formats       []uint32
		constraints   = make(chan struct{})
		stopped       bool
	)
	session := c.NewObject(func(op uint16, e *Event) {
		mu.Lock()
		defer mu.Unlock()
		switch op {
		case 0: // buffer_size
			width, height = int(e.Uint()), int(e.Uint())
		case 1: // shm_format
			formats = append(formats, e.Uint())
		case 4: // done
			select {
			case <-constraints:
			default:
				close(constraints)
			}
		case 5: // stopped
			stopped = true
		}
	})
	defer c.Request(session, 1).Send()

	options := uint32(0)
	if cursor {
		options = 1
	}
	if err := c.Request(copyMgr, 0).Uint(session).Uint(source).Uint(options).Send(); err != nil {
		return nil, err
	}
	if err := c.Wait(ctx, constraints); err != nil {
		return nil, err
	}
	mu.Lock()
	var offers []shmFormat
	for _, f := range formats {
		offers = append(offers, shmFormat{format: f, width: width, height: height, stride: width * 4})
	}
	pick, ok := pickFormat(offers)
	isStopped := stopped
	mu.Unlock()
	if isStopped {
		return nil, errors.New("capture session stopped by compositor")
	}
	if !ok {
		return nil, fmt.Errorf("image-copy-capture offered no readable shm format (%v)", formats)
	}

	buf, err := NewShmBuffer(c, shm, pick.width, pick.height, pick.stride, pick.format)
	if err != nil {
		return nil, err
	}
	defer buf.Destroy()

	done := make(chan struct{})
	var failReason int64 = -1
	frame := c.NewObject(func(op uint16, e *Event) {
		switch op {
		case 3: // ready
			close(done)
		case 4: // failed
			failReason = int64(e.Uint())
			close(done)
		}
	})
	defer c.Request(frame, 0).Send()
	c.Request(frame, 1).Uint(buf.ID).Send()                                       // attach_buffer
	c.Request(frame, 2).Int(0).Int(0).Int(int32(width)).Int(int32(height)).Send() // damage_buffer
	if err := c.Request(frame, 3).Send(); err != nil {                            // capture
		return nil, err
	}
	if err := c.Wait(ctx, done); err != nil {
		return nil, err
	}
	if failReason >= 0 {
		return nil, fmt.Errorf("compositor failed to capture the screen (reason %d)", failReason)
	}
	return toRGBA(buf, false), nil
}

func pickFormat(offers []shmFormat) (shmFormat, bool) {
	for _, want := range readableFormats {
		i := slices.IndexFunc(offers, func(f shmFormat) bool { return f.format == want })
		if i >= 0 {
			return offers[i], true
		}
	}
	return shmFormat{}, false
}

// toRGBA converts a 32-bit shm buffer into an opaque RGBA image.
func toRGBA(b *ShmBuffer, yInvert bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, b.Width, b.Height))
	bgra := b.Format == FormatARGB8888 || b.Format == FormatXRGB8888
	for y := 0; y < b.Height; y++ {
		sy := y
		if yInvert {
			sy = b.Height - 1 - y
		}
		src := b.Data[sy*b.Stride : sy*b.Stride+b.Width*4]
		dst := img.Pix[y*img.Stride : y*img.Stride+b.Width*4]
		for i := 0; i < len(src); i += 4 {
			if bgra {
				dst[i], dst[i+1], dst[i+2] = src[i+2], src[i+1], src[i]
			} else {
				dst[i], dst[i+1], dst[i+2] = src[i], src[i+1], src[i+2]
			}
			dst[i+3] = 0xff
		}
	}
	return img
}
