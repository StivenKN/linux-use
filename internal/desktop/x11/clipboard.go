package x11

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// ReadClipboard returns the CLIPBOARD selection as text, asking its owner
// for UTF8_STRING and falling back to Latin-1 STRING.
func (b *Backend) ReadClipboard(ctx context.Context) (string, error) {
	r, err := xproto.GetSelectionOwner(b.c, b.atoms.clipboard).Reply()
	if err != nil {
		return "", err
	}
	switch r.Owner {
	case xproto.WindowNone:
		return "", nil
	case b.win:
		b.mu.Lock()
		clip := b.clip
		b.mu.Unlock()
		if clip != nil {
			return *clip, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, target := range []xproto.Atom{b.atoms.utf8, xproto.AtomString} {
		data, typ, err := b.convert(ctx, target)
		if err != nil {
			return "", err
		}
		if typ == xproto.AtomString {
			return fromLatin1(data), nil
		}
		if typ != xproto.AtomNone {
			return string(data), nil
		}
	}
	return "", errors.New("the clipboard holds no text")
}

// convert asks the CLIPBOARD owner for target and reads the answer off our
// window, following the INCR protocol for large transfers. typ is None when
// the owner can't provide target.
func (b *Backend) convert(ctx context.Context, target xproto.Atom) (data []byte, typ xproto.Atom, err error) {
	for drained := false; !drained; { // events left over from an earlier transfer
		select {
		case <-b.sel:
		default:
			drained = true
		}
	}
	xproto.ConvertSelection(b.c, b.win, b.atoms.clipboard, target, b.atoms.prop, xproto.TimeCurrentTime)
	for {
		ev, err := b.nextSel(ctx)
		if err != nil {
			return nil, 0, err
		}
		if n, ok := ev.(xproto.SelectionNotifyEvent); ok && n.Selection == b.atoms.clipboard {
			if n.Property == xproto.AtomNone {
				return nil, xproto.AtomNone, nil
			}
			break
		}
	}
	r, err := xproto.GetProperty(b.c, true, b.win, b.atoms.prop, xproto.GetPropertyTypeAny, 0, math.MaxUint32/4).Reply()
	if err != nil {
		return nil, 0, err
	}
	if r.Type != b.atoms.incr {
		return r.Value, r.Type, nil
	}
	// INCR: deleting the property (done above) asks the owner for the next
	// chunk; an empty chunk ends the transfer.
	for {
		ev, err := b.nextSel(ctx)
		if err != nil {
			return nil, 0, err
		}
		p, ok := ev.(xproto.PropertyNotifyEvent)
		if !ok || p.Atom != b.atoms.prop || p.State != xproto.PropertyNewValue {
			continue
		}
		r, err := xproto.GetProperty(b.c, true, b.win, b.atoms.prop, xproto.GetPropertyTypeAny, 0, math.MaxUint32/4).Reply()
		if err != nil {
			return nil, 0, err
		}
		if len(r.Value) == 0 {
			return data, typ, nil
		}
		data, typ = append(data, r.Value...), r.Type
	}
}

func (b *Backend) nextSel(ctx context.Context) (xgb.Event, error) {
	select {
	case ev := <-b.sel:
		return ev, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("clipboard owner did not answer: %w", ctx.Err())
	}
}

// WriteClipboard takes ownership of CLIPBOARD; the event goroutine then
// serves text to whoever pastes until another client takes over or the
// connection closes. Without a clipboard manager the text goes with us.
func (b *Backend) WriteClipboard(ctx context.Context, text string) error {
	// Holding the lock across the round trips orders this against a
	// SelectionClear from a previous owner being handled concurrently.
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clip = &text
	xproto.SetSelectionOwner(b.c, b.win, b.atoms.clipboard, xproto.TimeCurrentTime)
	r, err := xproto.GetSelectionOwner(b.c, b.atoms.clipboard).Reply()
	if err == nil && r.Owner != b.win {
		err = errors.New("another client kept the clipboard")
	}
	if err != nil {
		b.clip = nil
	}
	return err
}

// selectionCleared forgets our text once another client owns CLIPBOARD.
func (b *Backend) selectionCleared(e xproto.SelectionClearEvent) {
	if e.Selection != b.atoms.clipboard {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, err := xproto.GetSelectionOwner(b.c, b.atoms.clipboard).Reply(); err == nil && r.Owner != b.win {
		b.clip = nil
	}
}

// serveSelection answers a paste request with the text we own.
func (b *Backend) serveSelection(e xproto.SelectionRequestEvent) {
	prop := e.Property
	if prop == xproto.AtomNone {
		prop = e.Target // obsolete requestors
	}
	b.mu.Lock()
	clip := b.clip
	b.mu.Unlock()
	if clip == nil || e.Selection != b.atoms.clipboard || !b.answer(e.Requestor, prop, e.Target, *clip) {
		prop = xproto.AtomNone
	}
	ev := xproto.SelectionNotifyEvent{
		Time: e.Time, Requestor: e.Requestor, Selection: e.Selection, Target: e.Target, Property: prop,
	}
	xproto.SendEvent(b.c, false, e.Requestor, 0, string(ev.Bytes()))
}

// answer stores text in the requestor's property in the target's format.
func (b *Backend) answer(w xproto.Window, prop, target xproto.Atom, text string) bool {
	a := b.atoms
	switch target {
	case a.targets:
		list := []xproto.Atom{a.targets, a.utf8, xproto.AtomString, a.text}
		buf := make([]byte, 4*len(list))
		for i, t := range list {
			xgb.Put32(buf[4*i:], uint32(t))
		}
		xproto.ChangeProperty(b.c, xproto.PropModeReplace, w, prop, xproto.AtomAtom, 32, uint32(len(list)), buf)
	case a.utf8, a.text:
		b.putBytes(w, prop, a.utf8, []byte(text))
	case xproto.AtomString:
		b.putBytes(w, prop, xproto.AtomString, toLatin1(text))
	default:
		return false
	}
	return true
}

// putBytes writes data to a property in appends that each fit the server's
// maximum request length; xgb doesn't do BIG-REQUESTS.
func (b *Backend) putBytes(w xproto.Window, prop, typ xproto.Atom, data []byte) {
	chunk := int(b.setup.MaximumRequestLength)*4 - 32
	mode := byte(xproto.PropModeReplace)
	for {
		n := min(len(data), chunk)
		xproto.ChangeProperty(b.c, mode, w, prop, typ, 8, uint32(n), data[:n])
		if data, mode = data[n:], xproto.PropModeAppend; len(data) == 0 {
			return
		}
	}
}

func fromLatin1(b []byte) string {
	rs := make([]rune, len(b))
	for i, c := range b {
		rs[i] = rune(c)
	}
	return string(rs)
}

func toLatin1(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xff {
			r = '?'
		}
		out = append(out, byte(r))
	}
	return out
}
