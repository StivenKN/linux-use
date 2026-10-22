package gnome

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"math"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/StivenKN/linux-use/internal/desktop"
)

// shot is a portal Screenshot request awaiting its Response signal.
type shot struct {
	path dbus.ObjectPath
	done chan shotResult // buffered; the signal loop fills it once
}

type shotResult struct {
	code uint32 // 0 success, 1 cancelled, 2 denied or failed
	uri  string
	at   time.Time
}

// Capture reads the display's ScreenCast stream over PipeWire, falling back
// to the screenshot portal if that fails.
func (b *Backend) Capture(ctx context.Context, d desktop.Display) (*image.RGBA, error) {
	img, err := b.capturePipeWire(ctx, d)
	if err != nil && ctx.Err() == nil {
		// GNOME may have ended the session under us (the user stopped
		// screen sharing) before its Closed signal arrived: retry once on a
		// fresh one.
		b.mu.Lock()
		b.stale = true
		b.mu.Unlock()
		img, err = b.capturePipeWire(ctx, d)
	}
	if err == nil {
		b.pwErr = nil
		return img, nil
	}
	if ctx.Err() != nil {
		return nil, err
	}
	if b.pwErr == nil {
		fmt.Fprintf(os.Stderr, "linux-use: PipeWire capture failed, using the screenshot portal: %v\n", err)
	}
	b.pwErr = err
	return b.capturePortal(ctx, d)
}

// PipeWireErr reports why the last capture fell back to the portal, if it did.
func (b *Backend) PipeWireErr() error { return b.pwErr }

// capturePortal takes a full-screen screenshot through the portal and crops it to d.
func (b *Backend) capturePortal(ctx context.Context, d desktop.Display) (*image.RGBA, error) {
	start := time.Now()
	ds, err := b.Displays(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(ds, func(o desktop.Display) bool { return o.Name == d.Name })
	if i < 0 {
		return nil, fmt.Errorf("display %q disappeared", d.Name)
	}
	file, err := b.screenshot(ctx, start)
	if err != nil {
		return nil, err
	}
	defer os.Remove(file)
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decoding portal screenshot: %w", err)
	}
	return crop(img, ds, ds[i]), nil
}

// screenshot returns the path of a fresh full-screen PNG written by GNOME
// Shell on the portal's behalf. The caller deletes it.
func (b *Backend) screenshot(ctx context.Context, since time.Time) (string, error) {
	for {
		b.mu.Lock()
		s := b.shot
		b.mu.Unlock()
		if s == nil {
			var err error
			if s, err = b.requestShot(ctx); err != nil {
				return "", err
			}
		}
		var res shotResult
		select {
		case res = <-s.done:
		case <-ctx.Done():
			// Keep the request: GNOME may be showing its one-time permission
			// dialog, and the next call should wait on it rather than open another.
			return "", fmt.Errorf("the screenshot portal did not answer; if GNOME asks to “Allow … to Take Screenshots?”, approve it once: %w", ctx.Err())
		}
		b.mu.Lock()
		b.shot = nil
		b.mu.Unlock()
		path := uriPath(res.uri)
		if res.at.Before(since) {
			// Answered between calls (the user allowed it late); it's stale.
			os.Remove(path)
			continue
		}
		switch {
		case res.code == 0 && path != "":
			return path, nil
		case res.code == 1:
			return "", errors.New("the screenshot was cancelled")
		}
		return "", errors.New("GNOME denied the screenshot; if you declined its permission dialog, reset it with: " +
			"gdbus call --session --dest org.freedesktop.impl.portal.PermissionStore " +
			"--object-path /org/freedesktop/impl/portal/PermissionStore " +
			"--method org.freedesktop.impl.portal.PermissionStore.Delete screenshot screenshot")
	}
}

// requestShot asks the portal for a non-interactive screenshot. The request
// path is derived from our unique name and a token so the Response can't be
// missed between the call returning and us looking for it.
func (b *Backend) requestShot(ctx context.Context) (*shot, error) {
	b.tokens++
	token := fmt.Sprintf("linux_use_%d", b.tokens)
	sender := strings.ReplaceAll(strings.TrimPrefix(b.bus.Names()[0], ":"), ".", "_")
	s := &shot{
		path: dbus.ObjectPath("/org/freedesktop/portal/desktop/request/" + sender + "/" + token),
		done: make(chan shotResult, 1),
	}
	b.mu.Lock()
	b.shot = s
	b.mu.Unlock()
	err := b.bus.Object("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop").
		CallWithContext(ctx, "org.freedesktop.portal.Screenshot.Screenshot", 0, "", map[string]dbus.Variant{
			"handle_token": dbus.MakeVariant(token),
			"interactive":  dbus.MakeVariant(false),
		}).Err
	if err != nil {
		b.mu.Lock()
		b.shot = nil
		b.mu.Unlock()
		return nil, fmt.Errorf("screenshot portal: %w", err)
	}
	return s, nil
}

// shotResponse routes a portal Response to the pending request, if it is ours.
func (b *Backend) shotResponse(sig *dbus.Signal) {
	if len(sig.Body) < 2 {
		return
	}
	code, _ := sig.Body[0].(uint32)
	results, _ := sig.Body[1].(map[string]dbus.Variant)
	uri, _ := results["uri"].Value().(string)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.shot != nil && b.shot.path == sig.Path {
		select {
		case b.shot.done <- shotResult{code, uri, time.Now()}:
		default:
		}
	}
}

// dropShot deletes the file of a screenshot that arrived after its caller left.
func (b *Backend) dropShot() {
	b.mu.Lock()
	s := b.shot
	b.shot = nil
	b.mu.Unlock()
	if s == nil {
		return
	}
	select {
	case res := <-s.done:
		os.Remove(uriPath(res.uri))
	default:
	}
}

func uriPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return u.Path
}

// crop cuts display d out of a screenshot of the whole layout. GNOME renders
// the layout at the highest monitor scale, so monitors with a lower scale are
// resampled down to their own device pixels.
func crop(img image.Image, ds []desktop.Display, d desktop.Display) *image.RGBA {
	x0, y0, x1 := ds[0].X, ds[0].Y, ds[0].X+ds[0].Width
	for _, o := range ds {
		x0, y0, x1 = min(x0, o.X), min(y0, o.Y), max(x1, o.X+o.Width)
	}
	b := img.Bounds()
	f := float64(b.Dx()) / float64(x1-x0)
	at := func(v, origin, base int) int { return base + int(math.Round(float64(v-origin)*f)) }
	src := image.Rect(
		at(d.X, x0, b.Min.X), at(d.Y, y0, b.Min.Y),
		at(d.X+d.Width, x0, b.Min.X), at(d.Y+d.Height, y0, b.Min.Y),
	).Intersect(b)
	w := int(math.Round(float64(d.Width) * d.Scale))
	h := int(math.Round(float64(d.Height) * d.Scale))
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	if src.Dx() == w && src.Dy() == h {
		draw.Draw(out, out.Bounds(), img, src.Min, draw.Src)
		return out
	}
	for y := range h {
		for x := range w {
			out.Set(x, y, img.At(src.Min.X+x*src.Dx()/w, src.Min.Y+y*src.Dy()/h))
		}
	}
	return out
}
