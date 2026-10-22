package gnome

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/StivenKN/linux-use/internal/desktop"
)

// remote is one Mutter RemoteDesktop session plus the linked ScreenCast
// session whose monitor streams anchor absolute pointer motion. Mutter
// releases every key and button the session still holds when it ends,
// including when our bus connection drops.
type remote struct {
	rd, sc   dbus.BusObject
	displays []desktop.Display
	streams  []dbus.ObjectPath // parallel to displays
	closed   bool              // Mutter ended it; guarded by Backend.mu
}

// notify runs f against the live input session, opening one if there is
// none, Mutter closed the last one, or the monitors changed since.
func (b *Backend) notify(ctx context.Context, f func(*remote) error) error {
	r, err := b.remote(ctx)
	if err != nil {
		return err
	}
	if err := f(r); err != nil {
		if gone(err) {
			b.mu.Lock()
			r.closed = true
			b.mu.Unlock()
			return fmt.Errorf("GNOME ended the remote desktop session, retry: %w", err)
		}
		return err
	}
	return nil
}

func (b *Backend) remote(ctx context.Context) (*remote, error) {
	b.mu.Lock()
	r := b.rd
	if r != nil && !r.closed && !b.stale {
		b.mu.Unlock()
		return r, nil
	}
	// Clear stale first so a monitor change during open marks the new session.
	b.rd, b.stale, b.ownClip = nil, false, false
	b.mu.Unlock()
	if r != nil {
		r.stop()
	}
	r, err := b.open(ctx)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.rd = r
	b.mu.Unlock()
	return r, nil
}

// open creates and starts a RemoteDesktop session recording every monitor.
func (b *Backend) open(ctx context.Context) (*remote, error) {
	ds, err := b.Displays(ctx)
	if err != nil {
		return nil, err
	}
	var path dbus.ObjectPath
	err = b.bus.Object(remoteDesktop, "/org/gnome/Mutter/RemoteDesktop").
		CallWithContext(ctx, remoteDesktop+".CreateSession", 0).Store(&path)
	if err != nil {
		return nil, fmt.Errorf("RemoteDesktop.CreateSession: %w", err)
	}
	r := &remote{rd: b.bus.Object(remoteDesktop, path), displays: ds}
	ok := false
	defer func() {
		if !ok {
			r.stop()
		}
	}()

	id, err := r.rd.GetProperty(remoteDesktop + ".Session.SessionId")
	if err != nil {
		return nil, fmt.Errorf("RemoteDesktop session id: %w", err)
	}
	var scPath dbus.ObjectPath
	err = b.bus.Object(screenCast, "/org/gnome/Mutter/ScreenCast").
		CallWithContext(ctx, screenCast+".CreateSession", 0, map[string]dbus.Variant{"remote-desktop-session-id": id}).
		Store(&scPath)
	if err != nil {
		return nil, fmt.Errorf("ScreenCast.CreateSession: %w", err)
	}
	r.sc = b.bus.Object(screenCast, scPath)
	for _, d := range ds {
		var stream dbus.ObjectPath
		err := r.sc.CallWithContext(ctx, screenCast+".Session.RecordMonitor", 0,
			d.Name, map[string]dbus.Variant{"cursor-mode": dbus.MakeVariant(uint32(0))}).Store(&stream)
		if err != nil {
			return nil, fmt.Errorf("ScreenCast.RecordMonitor %s: %w", d.Name, err)
		}
		r.streams = append(r.streams, stream)
	}
	if err := r.call(ctx, "Start"); err != nil {
		return nil, fmt.Errorf("RemoteDesktop.Start: %w", err)
	}
	if err := r.call(ctx, "EnableClipboard", map[string]dbus.Variant{}); err != nil {
		return nil, fmt.Errorf("RemoteDesktop.EnableClipboard: %w", err)
	}
	// Mutter creates the virtual pointer and keyboard on their first event.
	// Without physical devices (headless or remote sessions) that is when
	// the seat gains those capabilities, and clients drop events until they
	// bind them. Create both with no-op events (VoidSymbol has no keycode,
	// so Mutter logs it and presses nothing) and give clients a moment.
	if err := r.call(ctx, "NotifyPointerMotionRelative", 0.0, 0.0); err != nil {
		return nil, err
	}
	if err := r.call(ctx, "NotifyKeyboardKeysym", uint32(0xffffff), true); err != nil {
		return nil, err
	}
	time.Sleep(100 * time.Millisecond)
	ok = true
	return r, nil
}

func (r *remote) call(ctx context.Context, method string, args ...any) error {
	return r.rd.CallWithContext(ctx, remoteDesktop+".Session."+method, 0, args...).Err
}

// press sends a key or button state change through method
// (NotifyKeyboardKeysym or NotifyPointerButton). Anything left down
// auto-repeats or drags until the session ends, so releases ignore the
// caller's context and a failed press is released in case Mutter got it.
func (r *remote) press(ctx context.Context, method string, code any, down bool) error {
	if !down {
		return r.release(method, code)
	}
	err := r.call(ctx, method, code, true)
	if err != nil {
		r.release(method, code)
	}
	return err
}

func (r *remote) release(method string, code any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return r.call(ctx, method, code, false)
}

// stop ends the session; Mutter tears down the linked ScreenCast session.
func (r *remote) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r.call(ctx, "Stop")
}

// at returns the monitor containing (x, y), or the nearest one, and its stream.
func (r *remote) at(x, y int) (desktop.Display, dbus.ObjectPath) {
	best, bestDist := 0, math.MaxInt
	for i, d := range r.displays {
		dx := max(d.X-x, 0, x-(d.X+d.Width-1))
		dy := max(d.Y-y, 0, y-(d.Y+d.Height-1))
		if dx+dy < bestDist {
			best, bestDist = i, dx+dy
		}
	}
	return r.displays[best], r.streams[best]
}

// gone reports whether err means the session object no longer exists.
func gone(err error) bool {
	var e dbus.Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Name {
	case "org.freedesktop.DBus.Error.UnknownObject", "org.freedesktop.DBus.Error.UnknownMethod",
		"org.freedesktop.DBus.Error.ServiceUnknown":
		return true
	}
	return false
}

// loop handles Mutter and portal signals for the backend's lifetime; godbus
// closes sigs when the bus connection closes.
func (b *Backend) loop() {
	for sig := range b.sigs {
		switch sig.Name {
		case remoteDesktop + ".Session.Closed":
			b.mu.Lock()
			if b.rd != nil && b.rd.rd.Path() == sig.Path {
				b.rd.closed = true
				b.ownClip = false
			}
			b.mu.Unlock()
		case displayConfig + ".MonitorsChanged":
			b.mu.Lock()
			b.stale = true
			b.mu.Unlock()
		case remoteDesktop + ".Session.SelectionOwnerChanged":
			b.selectionOwnerChanged(sig)
		case remoteDesktop + ".Session.SelectionTransfer":
			b.selectionTransfer(sig)
		case "org.freedesktop.portal.Request.Response":
			b.shotResponse(sig)
		}
	}
}
