package x11

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"

	"github.com/StivenKN/linux-use/internal/desktop"
)

// Windows lists the window manager's managed windows (EWMH
// _NET_CLIENT_LIST), oldest first.
func (b *Backend) Windows(ctx context.Context) ([]desktop.Window, error) {
	a := b.atoms
	list, err := b.prop(b.root, a.clientList)
	if err != nil {
		return nil, err
	}
	if list.Format != 32 {
		return nil, errors.New("the window manager does not publish _NET_CLIENT_LIST (EWMH)")
	}
	active := b.activeWindow()

	// Titles in order of preference: the app's UTF-8 title, the one the
	// window manager shows (it decodes legacy encodings for us), then the
	// ICCCM one, which is Latin-1 or worse.
	titleAtoms := []xproto.Atom{a.wmName, a.wmVisibleName, xproto.AtomWmName}

	// Ask for every property up front so the replies arrive in one go.
	type props struct {
		titles       []xproto.GetPropertyCookie
		class, state xproto.GetPropertyCookie
	}
	ids := words(list.Value)
	cookies := make([]props, len(ids))
	for i, id := range ids {
		w := xproto.Window(id)
		c := props{class: b.propCookie(w, xproto.AtomWmClass), state: b.propCookie(w, a.wmState)}
		for _, t := range titleAtoms {
			c.titles = append(c.titles, b.propCookie(w, t))
		}
		cookies[i] = c
	}
	ws := make([]desktop.Window, 0, len(ids))
	for i, id := range ids {
		c := cookies[i]
		class, err := c.class.Reply()
		state, _ := c.state.Reply()
		var title string
		for _, ck := range c.titles {
			if r, _ := ck.Reply(); r != nil && title == "" {
				title = b.text(r)
			}
		}
		if err != nil {
			continue // closed since we listed it
		}
		// WM_CLASS is "instance\0class\0".
		parts := strings.Split(strings.TrimRight(string(class.Value), "\x00"), "\x00")
		appID := parts[len(parts)-1]
		ws = append(ws, desktop.Window{
			ID:        strconv.FormatUint(uint64(id), 10),
			Title:     title,
			AppID:     appID,
			Focused:   xproto.Window(id) == active,
			Minimized: state != nil && slices.Contains(words(state.Value), uint32(a.hidden)),
		})
	}
	return ws, nil
}

// Activate asks the window manager to focus the window (EWMH
// _NET_ACTIVE_WINDOW from a pager, which also restores minimized windows),
// then maps and raises it for window managers that ignore the request.
func (b *Backend) Activate(ctx context.Context, id string) error {
	n, err := strconv.ParseUint(id, 10, 32)
	if err != nil {
		return fmt.Errorf("bad window id %q", id)
	}
	w := xproto.Window(n)
	attrs, err := xproto.GetWindowAttributes(b.c, w).Reply()
	if err != nil {
		return fmt.Errorf("no window %s", id)
	}
	msg := xproto.ClientMessageEvent{
		Format: 32, Window: w, Type: b.atoms.activeWindow,
		Data: xproto.ClientMessageDataUnionData32New([]uint32{2, xproto.TimeCurrentTime, uint32(b.activeWindow()), 0, 0}),
	}
	xproto.SendEvent(b.c, false, b.root,
		xproto.EventMaskSubstructureRedirect|xproto.EventMaskSubstructureNotify, string(msg.Bytes()))
	if attrs.MapState == xproto.MapStateUnmapped {
		xproto.MapWindow(b.c, w)
	}
	xproto.ConfigureWindow(b.c, w, xproto.ConfigWindowStackMode, []uint32{xproto.StackModeAbove})
	return b.flush()
}

func (b *Backend) activeWindow() xproto.Window {
	r, err := b.prop(b.root, b.atoms.activeWindow)
	if err != nil || r.Format != 32 || len(r.Value) < 4 {
		return 0
	}
	return xproto.Window(xgb.Get32(r.Value))
}

func (b *Backend) propCookie(w xproto.Window, p xproto.Atom) xproto.GetPropertyCookie {
	return xproto.GetProperty(b.c, false, w, p, xproto.GetPropertyTypeAny, 0, 1<<16)
}

func (b *Backend) prop(w xproto.Window, p xproto.Atom) (*xproto.GetPropertyReply, error) {
	return b.propCookie(w, p).Reply()
}

// words decodes a format-32 property value.
func words(v []byte) []uint32 {
	out := make([]uint32, len(v)/4)
	for i := range out {
		out[i] = xgb.Get32(v[4*i:])
	}
	return out
}

// text decodes a text property. Anything but UTF8_STRING is read as
// Latin-1, which is exact for STRING and for COMPOUND_TEXT without escapes.
func (b *Backend) text(r *xproto.GetPropertyReply) string {
	if r.Type == b.atoms.utf8 {
		return string(r.Value)
	}
	return fromLatin1(r.Value)
}
