// Package kde drives KDE Plasma's Wayland compositor, KWin: the
// org_kde_kwin_fake_input protocol for input, the org.kde.KWin.ScreenShot2
// D-Bus interface for pixels, org_kde_plasma_window_management for windows
// and ext-data-control for the clipboard.
//
// KWin only offers fake input, window management and screenshots to
// executables named by an installed application entry, and never prompts.
// On first use the backend installs one for its own binary,
// ~/.local/share/applications/linux-use.desktop (hidden from menus), and
// reconnects once KWin has picked it up. Nothing needs restarting.
package kde

import (
	"context"
	"errors"
	"fmt"
	"image"
	"regexp"
	"strconv"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/StivenKN/linux-use/internal/desktop"
	"github.com/StivenKN/linux-use/internal/keys"
	"github.com/StivenKN/linux-use/internal/wl"
)

// org_kde_kwin_fake_input requests.
const (
	opAuthenticate   = 0
	opButton         = 2
	opAxis           = 3
	opMotionAbsolute = 9  // since 3
	opKey            = 10 // since 4: evdev code, mapped through the active layout
	opKeysym         = 12 // since 6 (Plasma 6.5): any keysym, layout-independent
)

const fakeInput = "org_kde_kwin_fake_input"

// Backend implements desktop.Backend, desktop.Windows and desktop.Clipboard.
type Backend struct {
	c       *wl.Conn
	bus     *dbus.Conn
	outputs *wl.Outputs
	seat    uint32

	fake    uint32 // org_kde_kwin_fake_input
	version uint32
	// symKeys is set when keysyms keep the modifiers held at the time
	// (KWin ≥ 6.6.6), so every key, combos included, goes through the
	// user's layout by keysym. Older KWin resets modifiers for each keysym,
	// so keys go by US-layout evdev code instead.
	symKeys bool
	held    map[uint32]bool // evdev codes we hold down

	wins *windows
	clip *wl.Clipboard
}

// Open connects to KWin at display, authorizing this executable first if
// needed. It returns an error wrapping desktop.ErrUnsupported when the
// compositor isn't KWin or KWin won't grant fake input.
func Open(display string) (*Backend, error) {
	c, err := wl.Dial(display)
	if err != nil {
		return nil, err
	}
	if !c.Has("org_kde_plasma_shell") && !c.Has("kde_output_device_v2") {
		c.Close()
		return nil, fmt.Errorf("%w: not KWin", desktop.ErrUnsupported)
	}
	// Keep the entry in sync even when fake input is already granted: the
	// screenshot interface checks it separately.
	path, changed, err := authorize()
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("authorizing linux-use with KWin: %w", err)
	}
	if !c.Has(fakeInput) {
		c.Close()
		if c, err = redial(display, path, changed); err != nil {
			return nil, err
		}
	}
	b := &Backend{c: c, held: map[uint32]bool{}}
	if err := b.init(); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

// redial reconnects until KWin grants fake input after the application entry
// at path changed. KWin decides what a client may bind when it connects, from
// an application cache it re-checks at most every 1.5 s.
func redial(display, path string, changed bool) (*wl.Conn, error) {
	if changed {
		for range 3 {
			time.Sleep(1600 * time.Millisecond)
			c, err := wl.Dial(display)
			if err != nil {
				return nil, err
			}
			if c.Has(fakeInput) {
				return c, nil
			}
			c.Close()
		}
	}
	return nil, fmt.Errorf("%w: KWin does not grant %s to this program although %s lists it; "+
		"KWin reads it from KDE's application cache, which `kbuildsycoca6` rebuilds", desktop.ErrUnsupported, fakeInput, path)
}

func (b *Backend) init() error {
	c := b.c
	var err error
	if b.fake, b.version, err = c.BindFirst(fakeInput, 6, nil); err != nil {
		return err
	}
	if b.version < 4 {
		return fmt.Errorf("%w: KWin's fake input is too old (version %d, need 4)", desktop.ErrUnsupported, b.version)
	}
	c.Request(b.fake, opAuthenticate).String("linux-use").String("Computer use for Claude Code").Send()

	if b.bus, err = dbus.ConnectSessionBus(); err != nil {
		return fmt.Errorf("KWin screenshots need the D-Bus session bus: %w", err)
	}
	b.symKeys = b.version >= 6 && keysymsKeepModifiers(b.bus)

	b.seat, _, _ = c.BindFirst("wl_seat", 1, nil)
	b.outputs = wl.TrackOutputs(c)
	b.wins, _ = trackWindows(c)
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

// keysymsKeepModifiers reports whether KWin applies held modifiers to keysym
// events, which it does from 6.6.6 on.
func keysymsKeepModifiers(bus *dbus.Conn) bool {
	var info string
	if bus.Object("org.kde.KWin", "/KWin").Call("org.kde.KWin.supportInformation", 0).Store(&info) != nil {
		return false
	}
	m := regexp.MustCompile(`KWin version: (\d+)\.(\d+)\.(\d+)`).FindStringSubmatch(info)
	if m == nil {
		return false
	}
	v := [3]int{}
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v[0] > 6 || v[0] == 6 && (v[1] > 6 || v[1] == 6 && v[2] >= 6)
}

func (b *Backend) Name() string { return "kde" }

// Close disconnects. KWin releases every key and button a fake input device
// still holds when its client goes away, so nothing stays pressed.
func (b *Backend) Close() error {
	if b.bus != nil {
		b.bus.Close()
	}
	return b.c.Close()
}

// Alive reports whether the compositor and session bus connections are usable.
func (b *Backend) Alive() bool { return b.c.Err() == nil && b.bus.Connected() }

func (b *Backend) Displays(ctx context.Context) ([]desktop.Display, error) {
	if err := b.c.Roundtrip(ctx); err != nil {
		return nil, err
	}
	outs := b.outputs.List()
	ds := make([]desktop.Display, 0, len(outs))
	for i, o := range outs {
		phys := o.ModeWidth
		if o.Rotated() {
			phys = o.ModeHeight
		}
		ds = append(ds, desktop.Display{
			ID: i + 1, Name: o.Name, X: o.X, Y: o.Y, Width: o.Width, Height: o.Height,
			Scale: float64(phys) / float64(max(o.Width, 1)),
		})
	}
	if len(ds) == 0 {
		return nil, errors.New("KWin reports no outputs")
	}
	return ds, nil
}

// Capture returns the output's pixels at native resolution; KWin renders it
// upright, so transforms need no undoing.
func (b *Backend) Capture(ctx context.Context, d desktop.Display) (*image.RGBA, error) {
	return screenshot(ctx, b.bus, d.Name)
}

func (b *Backend) Move(ctx context.Context, x, y int) error {
	return b.flush(ctx, b.c.Request(b.fake, opMotionAbsolute).Fixed(float64(x)).Fixed(float64(y)).Send())
}

func (b *Backend) Button(ctx context.Context, btn desktop.Button, down bool) error {
	return b.flush(ctx, b.c.Request(b.fake, opButton).Uint(uint32(btn)).Uint(state(down)).Send())
}

// Scroll sends one axis event per wheel click, 15 units each like a mouse
// wheel under libinput. Positive dy scrolls down, positive dx right.
func (b *Backend) Scroll(ctx context.Context, dx, dy int) error {
	for axis, n := range [2]int{dy, dx} {
		step := 15.0
		if n < 0 {
			n, step = -n, -step
		}
		for range n {
			if err := b.c.Request(b.fake, opAxis).Uint(uint32(axis)).Fixed(step).Send(); err != nil {
				return err
			}
		}
	}
	return b.flush(ctx, nil)
}

func (b *Backend) Key(ctx context.Context, sym keys.Keysym, down bool) error {
	if b.symKeys {
		return b.flush(ctx, b.keysym(sym, down))
	}
	if s, ok := keys.US(sym); ok {
		return b.flush(ctx, b.stroke(s, down))
	}
	if b.version >= 6 {
		return b.flush(ctx, b.keysym(sym, down))
	}
	return fmt.Errorf("this KWin can only press keys of the US layout; %#x is not one", uint32(sym))
}

// Type enters text through the user's layout by keysym. For characters the
// layout lacks, KWin 6.6+ binds a spare key on the fly (6.5 drops them), but
// X11 apps under Xwayland miss those: KWin restores the keymap before they
// reread it. KWin before Plasma 6.5 only takes key positions, so there text
// is limited to the US layout.
func (b *Backend) Type(ctx context.Context, text string) error {
	for i, r := range text {
		sym := keys.FromRune(r)
		var err error
		if b.version >= 6 {
			if err = b.keysym(sym, true); err == nil {
				err = b.keysym(sym, false)
			}
		} else {
			s, ok := keys.US(sym)
			if !ok {
				return fmt.Errorf("this KWin (before Plasma 6.5) can't type %q: only US-layout characters; "+
					"paste it instead (write_clipboard, then ctrl+v)", r)
			}
			if err = b.stroke(s, true); err == nil {
				err = b.stroke(s, false)
			}
		}
		if err != nil {
			return err
		}
		// Let clients keep up on long strings.
		if i%32 == 31 {
			if err := b.flush(ctx, nil); err != nil {
				return err
			}
		}
	}
	return b.flush(ctx, nil)
}

func (b *Backend) keysym(sym keys.Keysym, down bool) error {
	return b.c.Request(b.fake, opKeysym).Uint(uint32(sym)).Uint(state(down)).Send()
}

// stroke presses or releases a US-layout key by evdev code, holding Shift
// around the press when the character needs it and Shift isn't held already.
func (b *Backend) stroke(s keys.Stroke, down bool) error {
	const shiftL, shiftR = 42, 54
	if !down {
		return b.code(s.Code, false)
	}
	shift := s.Shift && !b.held[shiftL] && !b.held[shiftR]
	if shift {
		b.code(shiftL, true)
	}
	err := b.code(s.Code, true)
	if shift {
		b.code(shiftL, false)
	}
	return err
}

func (b *Backend) code(code uint32, down bool) error {
	if down {
		b.held[code] = true
	} else {
		delete(b.held, code)
	}
	return b.c.Request(b.fake, opKey).Uint(code).Uint(state(down)).Send()
}

func state(down bool) uint32 {
	if down {
		return 1
	}
	return 0
}

// flush waits for KWin to process everything sent so far so that actions
// are observable when the call returns.
func (b *Backend) flush(ctx context.Context, err error) error {
	if err != nil {
		return err
	}
	return b.c.Roundtrip(ctx)
}

var errNoWindows = errors.New("KWin does not expose its window list to linux-use")

func (b *Backend) Windows(ctx context.Context) ([]desktop.Window, error) {
	if b.wins == nil {
		return nil, errNoWindows
	}
	if err := b.wins.sync(ctx); err != nil {
		return nil, err
	}
	return b.wins.list(), nil
}

func (b *Backend) Activate(ctx context.Context, id string) error {
	if b.wins == nil {
		return errNoWindows
	}
	return b.flush(ctx, b.wins.activate(id))
}

var errNoClipboard = errors.New("KWin does not expose the clipboard (no data-control protocol)")

func (b *Backend) ReadClipboard(ctx context.Context) (string, error) {
	if b.clip == nil {
		return "", errNoClipboard
	}
	return b.clip.Read(ctx)
}

func (b *Backend) WriteClipboard(ctx context.Context, text string) error {
	if b.clip == nil {
		return errNoClipboard
	}
	return b.clip.Write(ctx, text)
}
