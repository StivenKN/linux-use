// Package computer implements computer-use actions on top of a desktop
// backend: coordinate mapping between screenshots and the screen, chords,
// drags, app launching and reconnecting when the session restarts.
package computer

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/StivenKN/linux-use/internal/apps"
	"github.com/StivenKN/linux-use/internal/desktop"
	"github.com/StivenKN/linux-use/internal/keys"
	"github.com/StivenKN/linux-use/internal/session"
)

// Connector finds the session and opens a backend for it.
type Connector func(ctx context.Context) (desktop.Backend, session.Session, error)

// Computer serializes all actions against one session.
type Computer struct {
	mu      sync.Mutex
	connect Connector
	b       desktop.Backend
	sess    session.Session
	lock    *os.File // held while we drive the session
	display string   // selected display name; "" means the first one
	shot    *Shot    // geometry of the last screenshot
	pinned  bool     // inside a batch: screenshots don't move the mapping
	latest  *Shot    // newest screenshot taken while pinned
	held    bool     // left button held by MouseDown
	cursor  image.Point
}

// New returns a Computer that connects lazily on first use.
func New(connect Connector) *Computer {
	return &Computer{connect: connect}
}

// Close releases the backend.
func (c *Computer) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.b != nil {
		c.b.Close()
		c.b = nil
	}
	if c.lock != nil {
		c.lock.Close()
		c.lock = nil
	}
}

// Session returns the session in use (connecting if needed).
func (c *Computer) Session(ctx context.Context) (session.Session, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensure(ctx); err != nil {
		return session.Session{}, "", err
	}
	return c.sess, c.b.Name(), nil
}

func (c *Computer) ensure(ctx context.Context) error {
	if c.b != nil {
		return nil
	}
	b, s, err := c.connect(ctx)
	if err != nil {
		return err
	}
	if c.lock == nil || s.Key() != c.sess.Key() {
		l, err := lock(s)
		if err != nil {
			b.Close()
			return err
		}
		if c.lock != nil {
			c.lock.Close()
		}
		c.lock = l
	}
	c.b, c.sess, c.shot = b, s, nil
	return nil
}

// do runs f with a live backend, reconnecting once if the session went away
// (e.g. the agent desktop was restarted).
func (c *Computer) do(ctx context.Context, f func(b desktop.Backend) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensure(ctx); err != nil {
		return err
	}
	err := f(c.b)
	if err != nil && !c.alive() {
		c.b.Close()
		c.b = nil
		if err := c.ensure(ctx); err != nil {
			return err
		}
		err = f(c.b)
	}
	return err
}

func (c *Computer) alive() bool {
	type aliver interface{ Alive() bool }
	if a, ok := c.b.(aliver); ok {
		return a.Alive()
	}
	return true
}

// Displays lists the session's monitors.
func (c *Computer) Displays(ctx context.Context) ([]desktop.Display, error) {
	var ds []desktop.Display
	err := c.do(ctx, func(b desktop.Backend) (err error) {
		ds, err = b.Displays(ctx)
		return err
	})
	return ds, err
}

// current returns the selected display.
func (c *Computer) current(ctx context.Context, b desktop.Backend) (desktop.Display, error) {
	ds, err := b.Displays(ctx)
	if err != nil {
		return desktop.Display{}, err
	}
	if c.display != "" {
		for _, d := range ds {
			if d.Name == c.display {
				return d, nil
			}
		}
		c.display = ""
	}
	return ds[0], nil
}

// SelectDisplay switches which monitor screenshots and coordinates refer to,
// by name or 1-based number; "" returns to the first monitor.
func (c *Computer) SelectDisplay(ctx context.Context, which string) (desktop.Display, error) {
	var picked desktop.Display
	err := c.do(ctx, func(b desktop.Backend) error {
		ds, err := b.Displays(ctx)
		if err != nil {
			return err
		}
		if which == "" {
			c.display, c.shot = "", nil
			picked = ds[0]
			return nil
		}
		for _, d := range ds {
			if d.Name == which || strconv.Itoa(d.ID) == which {
				picked = d
				c.display, c.shot = d.Name, nil
				return nil
			}
		}
		names := make([]string, len(ds))
		for i, d := range ds {
			names[i] = fmt.Sprintf("%d (%s)", d.ID, d.Name)
		}
		return fmt.Errorf("no display %q; available: %s", which, strings.Join(names, ", "))
	})
	return picked, err
}

// Image is an encoded screenshot.
type Image struct {
	Data []byte
	MIME string
	Shot Shot
}

// Screenshot captures the selected display, scaled to fit model limits.
func (c *Computer) Screenshot(ctx context.Context) (Image, error) {
	var out Image
	err := c.do(ctx, func(b desktop.Backend) error {
		d, err := c.current(ctx, b)
		if err != nil {
			return err
		}
		raw, err := b.Capture(ctx, d)
		if err != nil {
			return err
		}
		shot := shotFor(d)
		data, err := encode(resize(raw, shot.Width, shot.Height))
		if err != nil {
			return err
		}
		if c.pinned {
			c.latest = &shot
		} else {
			c.shot = &shot
		}
		out = Image{Data: data, MIME: "image/jpeg", Shot: shot}
		return nil
	})
	return out, err
}

// Zoom captures a region given in last-screenshot coordinates at full
// resolution, upscaled (or downscaled) to fit model limits.
func (c *Computer) Zoom(ctx context.Context, x0, y0, x1, y1 float64) (Image, error) {
	var out Image
	err := c.do(ctx, func(b desktop.Backend) error {
		shot, err := c.geometry(ctx, b)
		if err != nil {
			return err
		}
		if x1 <= x0 || y1 <= y0 {
			return errors.New("region must be [x0, y0, x1, y1] with x1 > x0 and y1 > y0")
		}
		raw, err := b.Capture(ctx, shot.Display)
		if err != nil {
			return err
		}
		// Image coordinates → physical pixels of the captured display.
		px := float64(raw.Bounds().Dx()) / float64(shot.Width)
		py := float64(raw.Bounds().Dy()) / float64(shot.Height)
		r := image.Rect(int(x0*px), int(y0*py), int(x1*px+0.5), int(y1*py+0.5)).Intersect(raw.Bounds())
		if r.Empty() {
			return errors.New("region is outside the screen")
		}
		w, h := zoomSize(r.Dx(), r.Dy())
		data, err := encode(resize(raw.SubImage(r), w, h))
		if err != nil {
			return err
		}
		out = Image{Data: data, MIME: "image/jpeg", Shot: Shot{Display: shot.Display, Width: w, Height: h}}
		return nil
	})
	return out, err
}

// Pin freezes the coordinate mapping for a batch: coordinates keep referring
// to the screenshot taken before it. The returned func unpins and adopts the
// newest screenshot taken meanwhile.
func (c *Computer) Pin() (unpin func()) {
	c.mu.Lock()
	c.pinned, c.latest = true, nil
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		if c.latest != nil {
			c.shot = c.latest
		}
		c.pinned, c.latest = false, nil
		c.mu.Unlock()
	}
}

// Geometry reports the current coordinate mapping, if any screenshot was taken.
func (c *Computer) Geometry() (Shot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.shot == nil {
		return Shot{}, false
	}
	return *c.shot, true
}

// geometry returns the last screenshot's mapping, or the mapping a
// screenshot of the current display would have.
func (c *Computer) geometry(ctx context.Context, b desktop.Backend) (Shot, error) {
	if c.shot != nil {
		return *c.shot, nil
	}
	d, err := c.current(ctx, b)
	if err != nil {
		return Shot{}, err
	}
	s := shotFor(d)
	c.shot = &s
	return s, nil
}

// Point is a coordinate in screenshot space.
type Point struct{ X, Y float64 }

func (c *Computer) moveTo(ctx context.Context, b desktop.Backend, p Point) error {
	shot, err := c.geometry(ctx, b)
	if err != nil {
		return err
	}
	if err := shot.Check(p); err != nil {
		return err
	}
	x, y := shot.ToScreen(p.X, p.Y)
	if err := b.Move(ctx, x, y); err != nil {
		return err
	}
	c.cursor = image.Pt(x, y)
	return nil
}

// Move moves the pointer.
func (c *Computer) Move(ctx context.Context, p Point) error {
	return c.do(ctx, func(b desktop.Backend) error { return c.moveTo(ctx, b, p) })
}

// settle is how long the pointer rests at its target before a click, so
// hover effects and focus changes land first.
const settle = 50 * time.Millisecond

// Click clicks button count times at p (or at the current position when p
// is nil) while holding the modifier keys in mods (e.g. "ctrl+shift").
func (c *Computer) Click(ctx context.Context, p *Point, button desktop.Button, count int, mods string) error {
	return c.do(ctx, func(b desktop.Backend) error {
		if p != nil {
			if err := c.moveTo(ctx, b, *p); err != nil {
				return err
			}
			time.Sleep(settle)
		}
		return c.withMods(ctx, b, mods, func() error {
			for i := range count {
				if i > 0 {
					time.Sleep(40 * time.Millisecond)
				}
				err := b.Button(ctx, button, true)
				// Release even if the press failed to flush (e.g. the call
				// was cancelled): the press may still have gone out.
				if rerr := b.Button(context.WithoutCancel(ctx), button, false); err == nil {
					err = rerr
				}
				if err != nil {
					return err
				}
			}
			return nil
		})
	})
}

// MouseDown presses and holds the left button at the current position.
func (c *Computer) MouseDown(ctx context.Context) error {
	return c.do(ctx, func(b desktop.Backend) error {
		if c.held {
			return errors.New("the left mouse button is already held; call left_mouse_up first")
		}
		if err := b.Button(ctx, desktop.Left, true); err != nil {
			return err
		}
		c.held = true
		return nil
	})
}

// MouseUp releases the left button; safe when it isn't held.
func (c *Computer) MouseUp(ctx context.Context) error {
	return c.do(ctx, func(b desktop.Backend) error {
		c.held = false
		return b.Button(ctx, desktop.Left, false)
	})
}

// Drag presses at from (or the current position), glides to to with an
// ease-out curve at 60 fps so toolkits see real motion and cross their drag
// thresholds, and releases.
func (c *Computer) Drag(ctx context.Context, from *Point, to Point, mods string) error {
	return c.do(ctx, func(b desktop.Backend) error {
		shot, err := c.geometry(ctx, b)
		if err != nil {
			return err
		}
		if from != nil {
			if err := c.moveTo(ctx, b, *from); err != nil {
				return err
			}
			time.Sleep(settle)
		}
		if err := shot.Check(to); err != nil {
			return err
		}
		start := c.cursor
		ex, ey := shot.ToScreen(to.X, to.Y)
		return c.withMods(ctx, b, mods, func() error {
			defer b.Button(context.WithoutCancel(ctx), desktop.Left, false)
			if err := b.Button(ctx, desktop.Left, true); err != nil {
				return err
			}
			time.Sleep(settle)
			dx, dy := float64(ex-start.X), float64(ey-start.Y)
			dur := min(math.Hypot(dx, dy)/2000, 0.5) // seconds, like the built-in tools
			frames := max(int(dur*60), 1)
			for i := 1; i <= frames; i++ {
				t := float64(i) / float64(frames)
				e := 1 - math.Pow(1-t, 3) // ease-out cubic
				if err := b.Move(ctx, start.X+int(math.Round(dx*e)), start.Y+int(math.Round(dy*e))); err != nil {
					return err
				}
				if i < frames {
					time.Sleep(time.Second / 60)
				}
			}
			c.cursor = image.Pt(ex, ey)
			time.Sleep(settle)
			return nil
		})
	})
}

// Scroll scrolls amount wheel clicks in direction at p (or where the pointer is).
func (c *Computer) Scroll(ctx context.Context, p *Point, direction string, amount int, mods string) error {
	var dx, dy int
	switch strings.ToLower(direction) {
	case "up":
		dy = -amount
	case "down":
		dy = amount
	case "left":
		dx = -amount
	case "right":
		dx = amount
	default:
		return fmt.Errorf("scroll direction must be up, down, left or right, got %q", direction)
	}
	return c.do(ctx, func(b desktop.Backend) error {
		if p != nil {
			if err := c.moveTo(ctx, b, *p); err != nil {
				return err
			}
		}
		return c.withMods(ctx, b, mods, func() error { return b.Scroll(ctx, dx, dy) })
	})
}

// Type enters text as if typed; Windows line endings become one Return.
func (c *Computer) Type(ctx context.Context, text string) error {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return c.do(ctx, func(b desktop.Backend) error { return b.Type(ctx, text) })
}

// Key presses a chord like "ctrl+shift+t" repeat times. Space-separated
// chords ("ctrl+a Delete") are pressed in sequence.
func (c *Computer) Key(ctx context.Context, combos string, repeat int) error {
	chords, err := keys.ParseChords(combos)
	if err != nil {
		return err
	}
	return c.do(ctx, func(b desktop.Backend) error {
		for i := range max(repeat, 1) {
			for j, syms := range chords {
				if i+j > 0 {
					time.Sleep(8 * time.Millisecond)
				}
				if err := chord(ctx, b, syms, 0); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// HoldKey holds a chord down for d.
func (c *Computer) HoldKey(ctx context.Context, combo string, d time.Duration) error {
	chords, err := keys.ParseChords(combo)
	if err != nil {
		return err
	}
	if len(chords) != 1 {
		return fmt.Errorf("hold_key takes one chord, got %q", combo)
	}
	return c.do(ctx, func(b desktop.Backend) error { return chord(ctx, b, chords[0], d) })
}

// chord presses syms in order, waits hold, and releases them in reverse,
// always releasing what was pressed even if a step fails.
func chord(ctx context.Context, b desktop.Backend, syms []keys.Keysym, hold time.Duration) error {
	var pressed []keys.Keysym
	defer func() {
		for _, s := range slices.Backward(pressed) {
			b.Key(context.WithoutCancel(ctx), s, false)
		}
	}()
	for _, s := range syms {
		pressed = append(pressed, s) // released by the defer even if the press errors
		if err := b.Key(ctx, s, true); err != nil {
			return err
		}
	}
	if hold > 0 {
		select {
		case <-time.After(hold):
		case <-ctx.Done():
		}
	}
	return nil
}

// withMods holds the modifier keys in mods (e.g. "ctrl+shift") around f.
// Only modifiers are accepted: anything else would let a click or scroll
// type keys.
func (c *Computer) withMods(ctx context.Context, b desktop.Backend, mods string, f func() error) error {
	syms, err := Modifiers(mods)
	if err != nil {
		return err
	}
	var pressed []keys.Keysym
	defer func() {
		for _, s := range slices.Backward(pressed) {
			b.Key(context.WithoutCancel(ctx), s, false)
		}
	}()
	for _, s := range syms {
		pressed = append(pressed, s)
		if err := b.Key(ctx, s, true); err != nil {
			return err
		}
	}
	return f()
}

// Modifiers parses a modifier chord such as "ctrl+shift" for clicks, drags
// and scrolls, rejecting non-modifier keys. "" is no modifiers.
func Modifiers(mods string) ([]keys.Keysym, error) {
	if strings.TrimSpace(mods) == "" {
		return nil, nil
	}
	chords, err := keys.ParseChords(mods)
	if err != nil {
		return nil, err
	}
	if len(chords) != 1 {
		return nil, fmt.Errorf("modifiers must be one chord like \"ctrl+shift\", got %q", mods)
	}
	for _, s := range chords[0] {
		if keys.ModMask(s) == 0 {
			return nil, fmt.Errorf("%q is not a modifier; text on clicks, drags and scrolls holds modifier keys only (shift, ctrl, alt, super)", mods)
		}
	}
	return chords[0], nil
}

// CursorPosition returns the pointer in last-screenshot coordinates. Wayland
// does not let clients read the pointer, so this is where we last put it.
func (c *Computer) CursorPosition(ctx context.Context) (int, int, error) {
	var x, y int
	err := c.do(ctx, func(b desktop.Backend) error {
		shot, err := c.geometry(ctx, b)
		if err != nil {
			return err
		}
		x, y = shot.FromScreen(c.cursor.X, c.cursor.Y)
		return nil
	})
	return x, y, err
}

// Windows lists top-level windows if the backend can.
func (c *Computer) Windows(ctx context.Context) ([]desktop.Window, error) {
	var ws []desktop.Window
	err := c.do(ctx, func(b desktop.Backend) error {
		w, ok := b.(desktop.Windows)
		if !ok {
			return fmt.Errorf("the %s backend cannot list windows", b.Name())
		}
		var err error
		ws, err = w.Windows(ctx)
		return err
	})
	return ws, err
}

// Activate focuses a window by id.
func (c *Computer) Activate(ctx context.Context, id string) error {
	return c.do(ctx, func(b desktop.Backend) error {
		w, ok := b.(desktop.Windows)
		if !ok {
			return fmt.Errorf("the %s backend cannot focus windows", b.Name())
		}
		return w.Activate(ctx, id)
	})
}

// ReadClipboard returns the clipboard text.
func (c *Computer) ReadClipboard(ctx context.Context) (string, error) {
	var s string
	err := c.do(ctx, func(b desktop.Backend) error {
		cb, ok := b.(desktop.Clipboard)
		if !ok {
			return fmt.Errorf("the %s backend has no clipboard access", b.Name())
		}
		var err error
		s, err = cb.ReadClipboard(ctx)
		return err
	})
	return s, err
}

// WriteClipboard replaces the clipboard text.
func (c *Computer) WriteClipboard(ctx context.Context, text string) error {
	return c.do(ctx, func(b desktop.Backend) error {
		cb, ok := b.(desktop.Clipboard)
		if !ok {
			return fmt.Errorf("the %s backend has no clipboard access", b.Name())
		}
		return cb.WriteClipboard(ctx, text)
	})
}

// OpenApplication brings an app to the front: it focuses an existing window
// of the app when there is one, otherwise launches it (by name, desktop id
// or command) and focuses the new window. It returns what happened.
func (c *Computer) OpenApplication(ctx context.Context, name string) (string, error) {
	var msg string
	err := c.do(ctx, func(b desktop.Backend) error {
		app, found := apps.Find(name)
		argv := strings.Fields(name)
		if found {
			argv = apps.ParseExec(app.Exec)
		} else {
			app = apps.App{ID: name, Name: name, Exec: name}
		}
		wins, canSee := b.(desktop.Windows)
		var before []desktop.Window
		if canSee {
			before, _ = wins.Windows(ctx)
			for _, w := range before {
				if app.Owns(w.AppID) {
					if err := wins.Activate(ctx, w.ID); err != nil {
						return err
					}
					msg = fmt.Sprintf("%s is already running; focused its window %q", app.Name, w.Title)
					return nil
				}
			}
		}
		if err := apps.Launch(argv, c.sess.Env()); err != nil {
			return fmt.Errorf("could not open %q: %w", name, err)
		}
		msg = "Launched " + app.Name
		if !canSee {
			return nil
		}
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
			now, err := wins.Windows(ctx)
			if err != nil {
				return nil
			}
			for _, w := range now {
				if !slices.ContainsFunc(before, func(o desktop.Window) bool { return o.ID == w.ID }) {
					wins.Activate(ctx, w.ID)
					msg = fmt.Sprintf("Opened %s: window %q (%s)", app.Name, w.Title, w.AppID)
					return nil
				}
			}
		}
		msg += "; no window appeared within 15s (it may still be starting)"
		return nil
	})
	return msg, err
}
