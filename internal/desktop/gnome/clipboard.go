package gnome

import (
	"cmp"
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"

	"github.com/StivenKN/linux-use/internal/keys"
)

var textMimes = []string{"text/plain;charset=utf-8", "UTF8_STRING", "text/plain", "TEXT", "STRING"}

// ReadClipboard returns the clipboard text ("" when empty or non-text).
func (b *Backend) ReadClipboard(ctx context.Context) (string, error) {
	b.mu.Lock()
	own, text := b.ownClip, b.clip
	b.mu.Unlock()
	if own {
		return text, nil
	}
	text = ""
	err := b.notify(ctx, func(r *remote) error {
		for _, mime := range textMimes {
			var fd dbus.UnixFD
			err := r.rd.CallWithContext(ctx, remoteDesktop+".Session.SelectionRead", 0, mime).Store(&fd)
			var de dbus.Error
			if errors.As(err, &de) && (de.Name == "org.freedesktop.DBus.Error.FileNotFound" ||
				de.Name == "org.freedesktop.DBus.Error.Failed") {
				continue // the clipboard is empty or not offered as this type
			}
			if err != nil {
				return err
			}
			data, err := transfer(ctx, int(fd), func(f *os.File) ([]byte, error) { return io.ReadAll(f) })
			text = string(data)
			return err
		}
		return nil
	})
	return text, err
}

// WriteClipboard offers text as the clipboard selection. Mutter asks us for
// the data on each paste, so it stays available while the backend is open.
func (b *Backend) WriteClipboard(ctx context.Context, text string) error {
	return b.notify(ctx, func(r *remote) error {
		b.mu.Lock()
		b.clip = text
		b.mu.Unlock()
		err := r.call(ctx, "SetSelection", map[string]dbus.Variant{"mime-types": dbus.MakeVariant(textMimes)})
		if err == nil {
			b.mu.Lock()
			b.ownClip = true
			b.mu.Unlock()
		}
		return err
	})
}

func (b *Backend) selectionOwnerChanged(sig *dbus.Signal) {
	if len(sig.Body) < 1 {
		return
	}
	opts, _ := sig.Body[0].(map[string]dbus.Variant)
	own, _ := opts["session-is-owner"].Value().(bool)
	b.mu.Lock()
	if b.rd != nil && b.rd.rd.Path() == sig.Path {
		b.ownClip = own
	}
	b.mu.Unlock()
}

// selectionTransfer serves our clipboard text to a pasting client.
func (b *Backend) selectionTransfer(sig *dbus.Signal) {
	if len(sig.Body) < 2 {
		return
	}
	serial, _ := sig.Body[1].(uint32)
	b.mu.Lock()
	r, text := b.rd, b.clip
	b.mu.Unlock()
	if r == nil || r.rd.Path() != sig.Path {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var fd dbus.UnixFD
		if err := r.rd.CallWithContext(ctx, remoteDesktop+".Session.SelectionWrite", 0, serial).Store(&fd); err != nil {
			return
		}
		_, err := transfer(ctx, int(fd), func(f *os.File) ([]byte, error) {
			_, err := io.WriteString(f, text)
			return nil, err
		})
		r.call(ctx, "SelectionWriteDone", serial, err == nil)
		if err == nil {
			b.mu.Lock()
			b.served = max(b.served, sig.Sequence)
			b.mu.Unlock()
			select {
			case b.servedCh <- struct{}{}:
			default:
			}
		}
	}()
}

// paste types text through the clipboard: it offers text, presses Ctrl+V,
// waits for the focused window to read it, then offers the clipboard text it
// replaced again. Waiting keeps later keystrokes from overtaking the paste.
func (b *Backend) paste(ctx context.Context, text string) error {
	prev, prevErr := b.ReadClipboard(ctx)
	if err := b.WriteClipboard(ctx, text); err != nil {
		return err
	}
	// Mutter's clipboard manager reads every new selection right away, so
	// only a read requested after our "v" press reached Mutter is the paste.
	var pressed dbus.Sequence
	err := b.notify(ctx, func(r *remote) error {
		const key = "NotifyKeyboardKeysym"
		ctrl, v := uint32(keys.ControlL), uint32('v')
		if err := r.press(ctx, key, ctrl, true); err != nil {
			return err
		}
		defer r.release(key, ctrl)
		c := r.rd.CallWithContext(ctx, remoteDesktop+".Session."+key, 0, v, true)
		pressed = c.ResponseSequence
		return cmp.Or(c.Err, r.release(key, v))
	})
	if err != nil {
		return err
	}
	timeout := time.After(time.Second)
	for {
		b.mu.Lock()
		done := b.served > pressed
		b.mu.Unlock()
		if done {
			break
		}
		select {
		case <-b.servedCh:
		case <-timeout:
			return errors.New("text outside ASCII is typed by pasting, but the focused window ignored the Ctrl+V we sent; " +
				"the text is on the clipboard (terminals paste with Ctrl+Shift+V)")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	time.Sleep(50 * time.Millisecond) // the window inserts what it read
	if prevErr == nil && prev != "" {
		return b.WriteClipboard(ctx, prev)
	}
	return nil
}

// transfer runs fn on a selection pipe from Mutter, bounded by ctx.
func transfer(ctx context.Context, fd int, fn func(*os.File) ([]byte, error)) ([]byte, error) {
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "selection")
	defer f.Close()
	if dl, ok := ctx.Deadline(); ok {
		f.SetDeadline(dl)
	} else {
		f.SetDeadline(time.Now().Add(5 * time.Second))
	}
	data, err := fn(f)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		err = errors.New("clipboard transfer timed out")
	}
	return data, err
}
