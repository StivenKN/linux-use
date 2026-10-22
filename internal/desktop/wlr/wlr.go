// Package wlr drives Wayland compositors that expose the wlroots family of
// automation protocols (sway, labwc, Hyprland, river, Wayfire, niri, COSMIC…):
// screencopy or ext-image-copy-capture for pixels, virtual pointer and
// virtual keyboard for input, foreign-toplevel for windows and data-control
// for the clipboard. No permission prompts, no external tools.
package wlr

import (
	"context"
	"errors"
	"fmt"
	"image"
	"strconv"
	"strings"
	"time"

	"github.com/StivenKN/linux-use/internal/desktop"
	"github.com/StivenKN/linux-use/internal/keys"
	"github.com/StivenKN/linux-use/internal/wl"
)

// Backend implements desktop.Backend, desktop.Windows and desktop.Clipboard.
type Backend struct {
	c       *wl.Conn
	outputs *wl.Outputs
	shm     uint32
	seat    uint32

	wlrCopy         uint32
	extSrc, extCopy uint32

	ptr  *wl.Pointer
	kbd  *wl.Keyboard
	mods uint32 // depressed modifier mask we last sent

	extra     map[keys.Keysym]uint32 // keysyms bound beyond the US layout
	nextExtra uint32

	tops *wl.Toplevels
	clip *wl.Clipboard
}

// Open connects to display and checks that the compositor exposes the
// protocols we need. It returns an error wrapping desktop.ErrUnsupported
// when the compositor is reachable but lacks them (GNOME, KDE).
func Open(display string) (*Backend, error) {
	c, err := wl.Dial(display)
	if err != nil {
		return nil, err
	}
	b := &Backend{c: c, extra: map[keys.Keysym]uint32{}, nextExtra: keys.ExtraFirst}
	if err := b.init(); err != nil {
		c.Close()
		return nil, err
	}
	return b, nil
}

func (b *Backend) init() error {
	c := b.c
	var missing []string
	need := func(ifaces ...string) bool {
		for _, i := range ifaces {
			if c.Has(i) {
				return true
			}
		}
		missing = append(missing, strings.Join(ifaces, " or "))
		return false
	}
	need("wl_shm")
	need("wl_seat")
	need("zwlr_screencopy_manager_v1", "ext_image_copy_capture_manager_v1")
	need("zwlr_virtual_pointer_manager_v1")
	need("zwp_virtual_keyboard_manager_v1")
	if len(missing) > 0 {
		return fmt.Errorf("%w: compositor lacks %s", desktop.ErrUnsupported, strings.Join(missing, ", "))
	}

	b.shm, _, _ = c.BindFirst("wl_shm", 1, nil)
	b.seat, _, _ = c.BindFirst("wl_seat", 1, nil)
	if c.Has("zwlr_screencopy_manager_v1") {
		b.wlrCopy, _, _ = c.BindFirst("zwlr_screencopy_manager_v1", 3, nil)
	} else {
		var err error
		if b.extSrc, _, err = c.BindFirst("ext_output_image_capture_source_manager_v1", 1, nil); err != nil {
			return fmt.Errorf("%w: %v", desktop.ErrUnsupported, err)
		}
		b.extCopy, _, _ = c.BindFirst("ext_image_copy_capture_manager_v1", 1, nil)
	}
	ptrMgr, _, _ := c.BindFirst("zwlr_virtual_pointer_manager_v1", 1, nil)
	kbdMgr, _, _ := c.BindFirst("zwp_virtual_keyboard_manager_v1", 1, nil)

	var err error
	if b.ptr, err = wl.NewPointer(c, ptrMgr, b.seat); err != nil {
		return err
	}
	if b.kbd, err = wl.NewKeyboard(c, kbdMgr, b.seat); err != nil {
		return err
	}
	if err := b.kbd.SetKeymap(keys.Keymap(nil)); err != nil {
		return err
	}
	b.outputs = wl.TrackOutputs(c)
	if c.Has("zwlr_foreign_toplevel_manager_v1") {
		b.tops, _ = wl.TrackToplevels(c)
	}
	b.clip, _ = wl.NewClipboard(c, b.seat)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Two round trips: one binds globals, the next collects their initial events.
	for range 2 {
		if err := c.Roundtrip(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) Name() string { return "wlroots" }

func (b *Backend) Close() error { return b.c.Close() }

// Alive reports whether the compositor connection is still usable.
func (b *Backend) Alive() bool { return b.c.Err() == nil }

func (b *Backend) Displays(ctx context.Context) ([]desktop.Display, error) {
	if err := b.c.Roundtrip(ctx); err != nil {
		return nil, err
	}
	outs := b.outputs.List()
	ds := make([]desktop.Display, 0, len(outs))
	for i, o := range outs {
		scale := float64(o.ModeWidth) / float64(max(o.Width, 1))
		if o.Rotated() {
			scale = float64(o.ModeHeight) / float64(max(o.Width, 1))
		}
		ds = append(ds, desktop.Display{
			ID: i + 1, Name: o.Name, X: o.X, Y: o.Y, Width: o.Width, Height: o.Height, Scale: scale,
		})
	}
	if len(ds) == 0 {
		return nil, errors.New("compositor reports no outputs")
	}
	return ds, nil
}

func (b *Backend) output(name string) (wl.Output, error) {
	for _, o := range b.outputs.List() {
		if o.Name == name {
			return o, nil
		}
	}
	return wl.Output{}, fmt.Errorf("display %q disappeared", name)
}

func (b *Backend) Capture(ctx context.Context, d desktop.Display) (*image.RGBA, error) {
	o, err := b.output(d.Name)
	if err != nil {
		return nil, err
	}
	if b.wlrCopy != 0 {
		return wl.CaptureWLR(ctx, b.c, b.wlrCopy, b.shm, o.ID, false)
	}
	return wl.CaptureExt(ctx, b.c, b.extSrc, b.extCopy, b.shm, o.ID, false)
}

// layout returns the bounding box of all outputs, which is what absolute
// virtual-pointer motion is relative to.
func (b *Backend) layout() (x0, y0, w, h int) {
	outs := b.outputs.List()
	if len(outs) == 0 {
		return 0, 0, 1, 1
	}
	x0, y0 = outs[0].X, outs[0].Y
	x1, y1 := x0, y0
	for _, o := range outs {
		x0, y0 = min(x0, o.X), min(y0, o.Y)
		x1, y1 = max(x1, o.X+o.Width), max(y1, o.Y+o.Height)
	}
	return x0, y0, x1 - x0, y1 - y0
}

func (b *Backend) Move(ctx context.Context, x, y int) error {
	x0, y0, w, h := b.layout()
	px := min(max(x-x0, 0), w-1)
	py := min(max(y-y0, 0), h-1)
	return b.flush(ctx, b.ptr.MoveAbs(uint32(px), uint32(py), uint32(w), uint32(h)))
}

func (b *Backend) Button(ctx context.Context, btn desktop.Button, down bool) error {
	return b.flush(ctx, b.ptr.Button(uint32(btn), down))
}

func (b *Backend) Scroll(ctx context.Context, dx, dy int) error {
	if dy != 0 {
		if err := b.ptr.Scroll(0, dy); err != nil {
			return err
		}
	}
	if dx != 0 {
		if err := b.ptr.Scroll(1, dx); err != nil {
			return err
		}
	}
	return b.flush(ctx, nil)
}

// stroke resolves a keysym to a keycode, binding it to a spare key (and
// re-uploading the keymap) when the US layout can't produce it.
func (b *Backend) stroke(sym keys.Keysym) (keys.Stroke, error) {
	if s, ok := keys.US(sym); ok {
		return s, nil
	}
	if code, ok := b.extra[sym]; ok {
		return keys.Stroke{Code: code}, nil
	}
	if b.nextExtra > keys.ExtraLast {
		clear(b.extra)
		b.nextExtra = keys.ExtraFirst
	}
	code := b.nextExtra
	b.nextExtra++
	b.extra[sym] = code
	if err := b.kbd.SetKeymap(keys.Keymap(b.extra)); err != nil {
		return keys.Stroke{}, err
	}
	return keys.Stroke{Code: code}, nil
}

func (b *Backend) setMods(m uint32) error {
	if m == b.mods {
		return nil
	}
	b.mods = m
	return b.kbd.Modifiers(m)
}

func (b *Backend) Key(ctx context.Context, sym keys.Keysym, down bool) error {
	s, err := b.stroke(sym)
	if err != nil {
		return err
	}
	if mask := keys.ModMask(sym); mask != 0 {
		m := b.mods &^ mask
		if down {
			m |= mask
		}
		// Press order matters to clients: modifier state first on press, key first on release.
		if down {
			b.setMods(m)
			return b.flush(ctx, b.kbd.Key(s.Code, true))
		}
		b.kbd.Key(s.Code, false)
		return b.flush(ctx, b.setMods(m))
	}
	held := b.mods
	if down && s.Shift {
		b.setMods(held | 1)
	}
	err = b.kbd.Key(s.Code, down)
	if down && s.Shift {
		b.setMods(held)
	}
	return b.flush(ctx, err)
}

func (b *Backend) Type(ctx context.Context, text string) error {
	held := b.mods
	defer b.setMods(held)
	for i, r := range text {
		s, err := b.stroke(keys.FromRune(r))
		if err != nil {
			return err
		}
		m := held
		if s.Shift {
			m |= 1
		}
		b.setMods(m)
		b.kbd.Key(s.Code, true)
		if err := b.kbd.Key(s.Code, false); err != nil {
			return err
		}
		// Let clients keep up on long strings; Wayland queues events but some
		// toolkits coalesce bursts badly.
		if i%32 == 31 {
			if err := b.flush(ctx, nil); err != nil {
				return err
			}
		}
	}
	return b.flush(ctx, nil)
}

// flush waits for the compositor to process everything sent so far so that
// actions are observable when the call returns.
func (b *Backend) flush(ctx context.Context, err error) error {
	if err != nil {
		return err
	}
	return b.c.Roundtrip(ctx)
}

func (b *Backend) Windows(ctx context.Context) ([]desktop.Window, error) {
	if b.tops == nil {
		return nil, errors.New("this compositor does not expose its window list")
	}
	if err := b.c.Roundtrip(ctx); err != nil {
		return nil, err
	}
	var ws []desktop.Window
	for _, t := range b.tops.List() {
		ws = append(ws, desktop.Window{
			ID: strconv.Itoa(int(t.ID)), Title: t.Title, AppID: t.AppID,
			Focused: t.Activated, Minimized: t.Minimized,
		})
	}
	return ws, nil
}

func (b *Backend) Activate(ctx context.Context, id string) error {
	if b.tops == nil {
		return errors.New("this compositor does not expose its window list")
	}
	n, err := strconv.Atoi(id)
	if err != nil {
		return fmt.Errorf("bad window id %q", id)
	}
	return b.flush(ctx, b.tops.Activate(uint32(n), b.seat))
}

func (b *Backend) ReadClipboard(ctx context.Context) (string, error) {
	if b.clip == nil {
		return "", errors.New("this compositor does not expose the clipboard")
	}
	return b.clip.Read(ctx)
}

func (b *Backend) WriteClipboard(ctx context.Context, text string) error {
	if b.clip == nil {
		return errors.New("this compositor does not expose the clipboard")
	}
	return b.clip.Write(ctx, text)
}
