// Package x11 drives X11 sessions over the core protocol and standard
// extensions: XTEST for input, GetImage for pixels, RandR for the monitor
// layout, EWMH for windows and the CLIPBOARD selection. No external tools
// and no permissions beyond access to the display.
package x11

import (
	"cmp"
	"context"
	"fmt"
	"image"
	"io"
	"log"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"

	"github.com/StivenKN/linux-use/internal/desktop"
	"github.com/StivenKN/linux-use/internal/keys"
)

// xgb logs benign things like a missing ~/.Xauthority to stderr; the
// failures that matter reach us as errors.
func init() { xgb.Logger = log.New(io.Discard, "", 0) }

// Backend implements desktop.Backend, desktop.Windows and desktop.Clipboard.
//
// xgb serializes requests internally, so the event goroutine (which serves
// clipboard requests) and the caller can share the connection; the mutex
// only guards the clipboard text they share.
type Backend struct {
	c          *xgb.Conn
	setup      *xproto.SetupInfo
	root       xproto.Window
	pix        pixfmt
	pixErr     error // why Capture cannot decode the root visual, if it can't
	randrMinor uint32
	atoms      atoms
	win        xproto.Window // hidden window that requests and owns selections

	kbd     keyboard
	buttons map[byte]bool           // X buttons pressed and not yet released
	pending []xtest.FakeInputCookie // input events not yet checked by flush

	sel  chan xgb.Event // SelectionNotify and PropertyNotify for win
	mu   sync.Mutex
	clip *string // text we own the CLIPBOARD with, nil when we don't
	dead atomic.Bool
}

// Open connects to display and checks that the server can be driven. It
// returns an error wrapping desktop.ErrUnsupported when XTEST is missing or
// the server is Xwayland, which can neither see nor drive Wayland windows.
func Open(display string) (*Backend, error) {
	c, err := xgb.NewConnDisplay(display)
	if err != nil {
		return nil, err
	}
	b := &Backend{c: c, buttons: map[byte]bool{}, sel: make(chan xgb.Event, 64)}
	if err := b.init(); err != nil {
		c.Close()
		return nil, err
	}
	go b.events()
	return b, nil
}

func (b *Backend) init() error {
	c := b.c
	if err := xtest.Init(c); err != nil {
		return fmt.Errorf("%w: X server lacks the XTEST extension", desktop.ErrUnsupported)
	}
	b.setup = xproto.Setup(c)
	scr := b.setup.DefaultScreen(c)
	b.root = scr.Root
	b.pix, b.pixErr = rootFormat(b.setup, scr)
	if randr.Init(c) == nil {
		if v, err := randr.QueryVersion(c, 1, 6).Reply(); err == nil && v.MajorVersion == 1 {
			b.randrMinor = v.MinorVersion
		}
	}
	if b.xwayland() {
		return fmt.Errorf("%w: the X server is Xwayland, which only sees X11 apps", desktop.ErrUnsupported)
	}

	var err error
	if b.atoms, err = internAtoms(c); err != nil {
		return err
	}
	if b.win, err = xproto.NewWindowId(c); err != nil {
		return err
	}
	err = xproto.CreateWindowChecked(c, 0, b.win, b.root, -10, -10, 1, 1, 0,
		xproto.WindowClassInputOnly, 0, xproto.CwOverrideRedirect|xproto.CwEventMask,
		[]uint32{1, xproto.EventMaskPropertyChange}).Check()
	if err != nil {
		return err
	}
	b.kbd.bound = map[xproto.Keycode]binding{}
	b.kbd.held = map[keys.Keysym]xproto.Keycode{}
	return b.loadKeymap()
}

// xwayland reports whether the server is Xwayland: newer ones register an
// XWAYLAND extension, older ones name their outputs XWAYLAND0, XWAYLAND1…
func (b *Backend) xwayland() bool {
	if r, err := xproto.QueryExtension(b.c, 8, "XWAYLAND").Reply(); err == nil && r.Present {
		return true
	}
	ds, _ := b.monitors()
	return slices.ContainsFunc(ds, func(d desktop.Display) bool { return strings.HasPrefix(d.Name, "XWAYLAND") })
}

func (b *Backend) Name() string { return "x11" }

// Alive reports whether the X connection is still usable.
func (b *Backend) Alive() bool { return !b.dead.Load() }

// Close releases anything the caller left pressed, returns borrowed
// keycodes to the layout and disconnects.
func (b *Backend) Close() error {
	for _, code := range b.kbd.held {
		b.fake(xproto.KeyRelease, byte(code), 0, 0)
	}
	for btn := range b.buttons {
		b.fake(xproto.ButtonRelease, btn, 0, 0)
	}
	b.restoreSpares()
	err := b.flush()
	b.c.Close()
	return err
}

func (b *Backend) Displays(ctx context.Context) ([]desktop.Display, error) {
	ds, err := b.monitors()
	if err != nil {
		return nil, err
	}
	if len(ds) == 0 {
		g, err := xproto.GetGeometry(b.c, xproto.Drawable(b.root)).Reply()
		if err != nil {
			return nil, err
		}
		ds = []desktop.Display{{Name: "screen", Width: int(g.Width), Height: int(g.Height)}}
	}
	slices.SortFunc(ds, func(a, b desktop.Display) int {
		return cmp.Or(a.X-b.X, a.Y-b.Y, a.Width-b.Width, a.Height-b.Height)
	})
	// Mirrored outputs show up twice; one entry is enough.
	ds = slices.CompactFunc(ds, func(a, b desktop.Display) bool {
		return a.X == b.X && a.Y == b.Y && a.Width == b.Width && a.Height == b.Height
	})
	for i := range ds {
		ds[i].ID, ds[i].Scale = i+1, 1
	}
	return ds, nil
}

// monitors lists RandR monitors (1.5+), else active CRTCs (1.3+), else
// nothing. X11 coordinates are physical, so the layout is used as is.
func (b *Backend) monitors() ([]desktop.Display, error) {
	var ds []desktop.Display
	switch {
	case b.randrMinor >= 5:
		r, err := randr.GetMonitors(b.c, b.root, true).Reply()
		if err != nil {
			return nil, err
		}
		for _, m := range r.Monitors {
			name := fmt.Sprintf("monitor-%d", len(ds)+1)
			if a, err := xproto.GetAtomName(b.c, m.Name).Reply(); err == nil {
				name = a.Name
			}
			ds = append(ds, desktop.Display{Name: name, X: int(m.X), Y: int(m.Y), Width: int(m.Width), Height: int(m.Height)})
		}
	case b.randrMinor >= 3:
		res, err := randr.GetScreenResourcesCurrent(b.c, b.root).Reply()
		if err != nil {
			return nil, err
		}
		for _, crtc := range res.Crtcs {
			ci, err := randr.GetCrtcInfo(b.c, crtc, res.ConfigTimestamp).Reply()
			if err != nil || ci.Mode == 0 || len(ci.Outputs) == 0 {
				continue
			}
			name := fmt.Sprintf("crtc-%d", crtc)
			if o, err := randr.GetOutputInfo(b.c, ci.Outputs[0], res.ConfigTimestamp).Reply(); err == nil {
				name = string(o.Name)
			}
			ds = append(ds, desktop.Display{Name: name, X: int(ci.X), Y: int(ci.Y), Width: int(ci.Width), Height: int(ci.Height)})
		}
	}
	return ds, nil
}

// stripBytes bounds each GetImage reply; xgb holds two copies of it in memory.
const stripBytes = 16 << 20

func (b *Backend) Capture(ctx context.Context, d desktop.Display) (*image.RGBA, error) {
	if b.pixErr != nil {
		return nil, b.pixErr
	}
	if d.Width <= 0 || d.Height <= 0 {
		return nil, fmt.Errorf("display %q has no area", d.Name)
	}
	img := image.NewRGBA(image.Rect(0, 0, d.Width, d.Height))
	step := max(1, stripBytes/b.pix.stride(d.Width))
	for y := 0; y < d.Height; y += step {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h := min(step, d.Height-y)
		r, err := xproto.GetImage(b.c, xproto.ImageFormatZPixmap, xproto.Drawable(b.root),
			int16(d.X), int16(d.Y+y), uint16(d.Width), uint16(h), math.MaxUint32).Reply()
		if err != nil {
			return nil, fmt.Errorf("capture %s: %w", d.Name, err)
		}
		if len(r.Data) < b.pix.stride(d.Width)*h {
			return nil, fmt.Errorf("capture %s: short image reply", d.Name)
		}
		b.pix.decode(img, y, r.Data)
	}
	return img, nil
}

// fake sends one XTEST event without waiting for it; flush checks it. x and
// y only matter for motion.
func (b *Backend) fake(typ, detail byte, x, y int) {
	clamp := func(v int) int16 { return int16(min(max(v, math.MinInt16), math.MaxInt16)) }
	b.pending = append(b.pending, xtest.FakeInputChecked(b.c, typ, detail, xproto.TimeCurrentTime, b.root, clamp(x), clamp(y), 0))
}

// flush waits until the server has processed every request sent so far,
// so actions are observable when a call returns, and reports the first
// input event it rejected.
func (b *Backend) flush() error {
	defer func() { b.pending = b.pending[:0] }()
	if _, err := xproto.GetInputFocus(b.c).Reply(); err != nil {
		return err
	}
	for _, ck := range b.pending {
		if err := ck.Check(); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) Move(ctx context.Context, x, y int) error {
	b.fake(xproto.MotionNotify, 0, x, y)
	return b.flush()
}

// buttonCodes maps evdev buttons to core X pointer buttons.
var buttonCodes = map[desktop.Button]byte{
	desktop.Left: 1, desktop.Middle: 2, desktop.Right: 3, desktop.Back: 8, desktop.Forward: 9,
}

func (b *Backend) Button(ctx context.Context, btn desktop.Button, down bool) error {
	n, ok := buttonCodes[btn]
	if !ok {
		return fmt.Errorf("unsupported button %#x", uint32(btn))
	}
	if down {
		b.buttons[n] = true
		b.fake(xproto.ButtonPress, n, 0, 0)
	} else {
		delete(b.buttons, n)
		b.fake(xproto.ButtonRelease, n, 0, 0)
	}
	return b.flush()
}

// Scroll clicks the wheel buttons: 4/5 scroll up/down, 6/7 left/right.
func (b *Backend) Scroll(ctx context.Context, dx, dy int) error {
	click := func(n int, neg, pos byte) {
		btn := pos
		if n < 0 {
			btn, n = neg, -n
		}
		for range n {
			b.fake(xproto.ButtonPress, btn, 0, 0)
			b.fake(xproto.ButtonRelease, btn, 0, 0)
		}
	}
	click(dy, 4, 5)
	click(dx, 6, 7)
	return b.flush()
}

// atoms are the interned atoms the backend uses beyond the predefined ones.
type atoms struct {
	clipboard, utf8, targets, text, incr, prop xproto.Atom
	clientList, activeWindow, wmState, hidden  xproto.Atom
	wmName, wmVisibleName                      xproto.Atom
}

func internAtoms(c *xgb.Conn) (atoms, error) {
	var a atoms
	names := []struct {
		name string
		dst  *xproto.Atom
	}{
		{"CLIPBOARD", &a.clipboard}, {"UTF8_STRING", &a.utf8}, {"TARGETS", &a.targets},
		{"TEXT", &a.text}, {"INCR", &a.incr}, {"LINUX_USE_SELECTION", &a.prop},
		{"_NET_CLIENT_LIST", &a.clientList}, {"_NET_ACTIVE_WINDOW", &a.activeWindow},
		{"_NET_WM_STATE", &a.wmState}, {"_NET_WM_STATE_HIDDEN", &a.hidden},
		{"_NET_WM_NAME", &a.wmName}, {"_NET_WM_VISIBLE_NAME", &a.wmVisibleName},
	}
	cookies := make([]xproto.InternAtomCookie, len(names))
	for i, n := range names {
		cookies[i] = xproto.InternAtom(c, false, uint16(len(n.name)), n.name)
	}
	for i, ck := range cookies {
		r, err := ck.Reply()
		if err != nil {
			return a, fmt.Errorf("intern %s: %w", names[i].name, err)
		}
		*names[i].dst = r.Atom
	}
	return a, nil
}

// events drains the connection for as long as it is open: it notes keymap
// changes, serves clipboard requests and hands selection replies to
// ReadClipboard. X errors from unchecked requests are dropped here.
func (b *Backend) events() {
	defer b.dead.Store(true)
	for {
		ev, xerr := b.c.WaitForEvent()
		switch e := ev.(type) {
		case nil:
			if xerr == nil {
				return // connection closed
			}
		case xproto.MappingNotifyEvent:
			if e.Request == xproto.MappingKeyboard {
				b.kbd.stale.Store(true)
			}
		case xproto.SelectionRequestEvent:
			b.serveSelection(e)
		case xproto.SelectionClearEvent:
			b.selectionCleared(e)
		case xproto.SelectionNotifyEvent, xproto.PropertyNotifyEvent:
			select {
			case b.sel <- ev:
			default:
			}
		}
	}
}
