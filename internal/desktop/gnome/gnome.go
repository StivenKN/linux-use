// Package gnome drives GNOME Shell (Mutter) Wayland sessions, which lack the
// wlroots automation protocols, through D-Bus on the user's session bus:
//
//   - input and clipboard: org.gnome.Mutter.RemoteDesktop, with a linked
//     ScreenCast session recording every monitor so absolute pointer motion
//     works. No prompt; GNOME shows its screen-sharing indicator while the
//     session is open.
//   - displays: org.gnome.Mutter.DisplayConfig.
//   - pixels: the frames of those same ScreenCast streams, read over
//     PipeWire (internal/pw). No prompt, flash or sound. If that fails, the
//     org.freedesktop.portal.Screenshot portal is the fallback
//     (org.gnome.Shell.Screenshot only serves allowlisted callers): it asks
//     once ("Allow … to Take Screenshots?"), and every capture flashes the
//     screen and plays the shutter sound.
//
// org.gnome.Shell.Introspect is allowlisted too, so windows come from the
// linux-use GNOME Shell extension (extension.go), installed with consent.
package gnome

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/godbus/dbus/v5"

	"github.com/StivenKN/linux-use/internal/desktop"
	"github.com/StivenKN/linux-use/internal/keys"
	"github.com/StivenKN/linux-use/internal/pw"
	"github.com/StivenKN/linux-use/internal/wl"
)

const (
	remoteDesktop = "org.gnome.Mutter.RemoteDesktop"
	screenCast    = "org.gnome.Mutter.ScreenCast"
	displayConfig = "org.gnome.Mutter.DisplayConfig"
)

var (
	_ desktop.Backend   = (*Backend)(nil)
	_ desktop.Windows   = (*Backend)(nil)
	_ desktop.Clipboard = (*Backend)(nil)
)

// Backend implements desktop.Backend, desktop.Windows and desktop.Clipboard.
type Backend struct {
	bus  *dbus.Conn
	sigs chan *dbus.Signal

	mu      sync.Mutex                 // guards the fields below, shared with the signal loop
	rd      *remote                    // live input session, opened on first use
	stale   bool                       // monitors changed since rd was opened
	clip    string                     // text we offer on the clipboard
	ownClip bool                       // whether Mutter still has our offer as the selection
	served  dbus.Sequence              // arrival of the latest clipboard read we answered
	shot    *shot                      // portal screenshot request still awaiting a reply
	nodes   map[dbus.ObjectPath]uint32 // PipeWire node of each ScreenCast stream
	nodeSig chan struct{}              // closed and replaced when nodes changes

	servedCh chan struct{} // wakes paste when served advances
	tokens   int           // portal request token counter
	pw       *pw.Conn      // PipeWire connection, dialed on first capture
	pwErr    error         // why the last PipeWire capture failed, if it did
	pwOff    bool          // PipeWire never delivered a frame: use the portal
	input    time.Time     // when we last sent input, so captures can show its effect
}

// Open checks that display is served by Mutter and that its RemoteDesktop
// API is on the session bus. It returns an error wrapping
// desktop.ErrUnsupported for any other compositor.
func Open(display string) (*Backend, error) {
	c, err := wl.Dial(display)
	if err != nil {
		return nil, err
	}
	mutter := c.Has("gtk_shell1") // only Mutter implements it
	c.Close()
	if !mutter {
		return nil, fmt.Errorf("%w: compositor is not GNOME Shell", desktop.ErrUnsupported)
	}

	bus, err := sessionBus()
	if err != nil {
		return nil, err
	}
	var has bool
	if err := bus.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, remoteDesktop).Store(&has); err != nil || !has {
		bus.Close()
		return nil, fmt.Errorf("%w: %s is not on the session bus", desktop.ErrUnsupported, remoteDesktop)
	}
	b := &Backend{
		bus:      bus,
		sigs:     make(chan *dbus.Signal, 64),
		servedCh: make(chan struct{}, 1),
		nodes:    map[dbus.ObjectPath]uint32{},
		nodeSig:  make(chan struct{}),
	}
	for _, m := range [][]dbus.MatchOption{
		{dbus.WithMatchSender(remoteDesktop), dbus.WithMatchInterface(remoteDesktop + ".Session")},
		{dbus.WithMatchInterface(screenCast + ".Stream"), dbus.WithMatchMember("PipeWireStreamAdded")},
		{dbus.WithMatchInterface(displayConfig), dbus.WithMatchMember("MonitorsChanged")},
		{dbus.WithMatchInterface("org.freedesktop.portal.Request"), dbus.WithMatchMember("Response")},
	} {
		if err := bus.AddMatchSignal(m...); err != nil {
			bus.Close()
			return nil, err
		}
	}
	bus.Signal(b.sigs)
	go b.loop()
	return b, nil
}

// sessionBus connects to the user's session bus without autolaunching one.
func sessionBus() (*dbus.Conn, error) {
	addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if addr == "" {
		p := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "bus")
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("%w: no D-Bus session bus", desktop.ErrUnsupported)
		}
		addr = "unix:path=" + p
	}
	bus, err := dbus.Connect(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: session bus: %v", desktop.ErrUnsupported, err)
	}
	return bus, nil
}

func (b *Backend) Name() string { return "gnome" }

// Close ends the input session, which makes Mutter release anything still held.
func (b *Backend) Close() error {
	b.mu.Lock()
	rd := b.rd
	b.rd = nil
	b.mu.Unlock()
	if rd != nil {
		rd.stop()
	}
	if b.pw != nil {
		b.pw.Close()
	}
	b.dropShot()
	return b.bus.Close()
}

// Displays reads the logical monitor layout from DisplayConfig.
func (b *Backend) Displays(ctx context.Context) ([]desktop.Display, error) {
	var (
		serial   uint32
		monitors []struct {
			ID    monitorID
			Modes []struct {
				ID              string
				Width, Height   int32
				Refresh, Scale  float64
				SupportedScales []float64
				Props           map[string]dbus.Variant
			}
			Props map[string]dbus.Variant
		}
		logical []struct {
			X, Y      int32
			Scale     float64
			Transform uint32
			Primary   bool
			Monitors  []monitorID
			Props     map[string]dbus.Variant
		}
		props map[string]dbus.Variant
	)
	err := b.bus.Object(displayConfig, "/org/gnome/Mutter/DisplayConfig").
		CallWithContext(ctx, displayConfig+".GetCurrentState", 0).
		Store(&serial, &monitors, &logical, &props)
	if err != nil {
		return nil, fmt.Errorf("DisplayConfig.GetCurrentState: %w", err)
	}
	// Layout mode 1 is logical (positions in logical pixels, the default),
	// 2 is physical (positions in device pixels, clients scale themselves).
	physical := props["layout-mode"].Value() == uint32(2)

	var ds []desktop.Display
	for _, lm := range logical {
		if len(lm.Monitors) == 0 {
			continue
		}
		w, h := 0, 0
		for _, m := range monitors {
			if m.ID != lm.Monitors[0] {
				continue
			}
			for _, md := range m.Modes {
				if cur, _ := md.Props["is-current"].Value().(bool); cur {
					w, h = int(md.Width), int(md.Height)
				}
			}
		}
		if w == 0 {
			continue // monitor is off
		}
		if lm.Transform%2 == 1 { // 90 or 270 degrees, possibly flipped
			w, h = h, w
		}
		d := desktop.Display{Name: lm.Monitors[0].Connector, X: int(lm.X), Y: int(lm.Y), Width: w, Height: h, Scale: 1}
		if !physical && lm.Scale > 0 {
			d.Width = int(math.Round(float64(w) / lm.Scale))
			d.Height = int(math.Round(float64(h) / lm.Scale))
			d.Scale = lm.Scale
		}
		ds = append(ds, d)
	}
	if len(ds) == 0 {
		return nil, errors.New("GNOME reports no active monitors")
	}
	slices.SortFunc(ds, func(a, b desktop.Display) int {
		if a.X != b.X {
			return a.X - b.X
		}
		return a.Y - b.Y
	})
	for i := range ds {
		ds[i].ID = i + 1
	}
	return ds, nil
}

// monitorID is DisplayConfig's (connector, vendor, product, serial) tuple.
type monitorID struct{ Connector, Vendor, Product, Serial string }

// Move warps the pointer through the ScreenCast stream of the monitor under
// (x, y); stream coordinates are that monitor's device pixels.
func (b *Backend) Move(ctx context.Context, x, y int) error {
	return b.inject(ctx, func(r *remote) error {
		d, stream := r.at(x, y)
		px := float64(min(max(x-d.X, 0), d.Width-1)) * d.Scale
		py := float64(min(max(y-d.Y, 0), d.Height-1)) * d.Scale
		// Mutter can pin one axis of a warp to the edge when the pointer has
		// no prior position (the first motion after GNOME Shell starts). The
		// repeat starts from wherever the first landed and always arrives.
		for range 2 {
			if err := r.call(ctx, "NotifyPointerMotionAbsolute", string(stream), px, py); err != nil {
				return err
			}
		}
		return nil
	})
}

func (b *Backend) Button(ctx context.Context, btn desktop.Button, down bool) error {
	return b.inject(ctx, func(r *remote) error {
		return r.press(ctx, "NotifyPointerButton", int32(btn), down)
	})
}

func (b *Backend) Scroll(ctx context.Context, dx, dy int) error {
	return b.inject(ctx, func(r *remote) error {
		if dy != 0 {
			if err := r.call(ctx, "NotifyPointerAxisDiscrete", uint32(0), int32(dy)); err != nil {
				return err
			}
		}
		if dx != 0 {
			return r.call(ctx, "NotifyPointerAxisDiscrete", uint32(1), int32(dx))
		}
		return nil
	})
}

// Key presses or releases a keysym. Mutter maps it onto the active layout,
// adding Shift or AltGr itself, and silently drops keysyms the layout lacks.
func (b *Backend) Key(ctx context.Context, sym keys.Keysym, down bool) error {
	return b.inject(ctx, func(r *remote) error {
		return r.press(ctx, "NotifyKeyboardKeysym", uint32(sym), down)
	})
}

// Type enters ASCII as keysyms, which works on any Latin layout. Mutter drops
// keysyms the active layout lacks without telling us, and there is no way to
// ask for the layout, so text with anything else is pasted instead.
func (b *Backend) Type(ctx context.Context, text string) error {
	if strings.ContainsFunc(text, func(r rune) bool { return r > unicode.MaxASCII }) {
		return b.paste(ctx, text)
	}
	return b.inject(ctx, func(r *remote) error {
		for _, ch := range text {
			sym := uint32(keys.FromRune(ch))
			if err := r.press(ctx, "NotifyKeyboardKeysym", sym, true); err != nil {
				return err
			}
			if err := r.press(ctx, "NotifyKeyboardKeysym", sym, false); err != nil {
				return err
			}
		}
		return nil
	})
}
