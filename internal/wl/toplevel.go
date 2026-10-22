package wl

import (
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
)

// Toplevel is a window reported by zwlr_foreign_toplevel_manager_v1.
type Toplevel struct {
	ID         uint32 // short sequential id, stable for the window's lifetime
	handle     uint32
	Title      string
	AppID      string
	Activated  bool
	Maximized  bool
	Minimized  bool
	Fullscreen bool
}

// Toplevels tracks every window the compositor exposes.
type Toplevels struct {
	c    *Conn
	mu   sync.Mutex
	m    map[uint32]*Toplevel // by short id; committed state, updated on each done event
	next uint32
}

// TrackToplevels binds the wlr foreign toplevel manager. Call Roundtrip
// afterwards before reading the list.
func TrackToplevels(c *Conn) (*Toplevels, error) {
	t := &Toplevels{c: c, m: map[uint32]*Toplevel{}}
	_, _, err := c.BindFirst("zwlr_foreign_toplevel_manager_v1", 3, func(op uint16, e *Event) {
		if op == 0 { // toplevel(new_id)
			id := e.Uint()
			c.SetHandler(id, t.handle(id))
		}
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Toplevels) handle(handle uint32) Handler {
	t.mu.Lock()
	t.next++
	id := t.next
	t.mu.Unlock()
	pending := &Toplevel{ID: id, handle: handle}
	return func(op uint16, e *Event) {
		switch op {
		case 0:
			pending.Title = e.Str()
		case 1:
			pending.AppID = e.Str()
		case 4: // state array of uint32
			s := e.Array()
			pending.Maximized, pending.Minimized, pending.Activated, pending.Fullscreen = false, false, false, false
			for i := 0; i+4 <= len(s); i += 4 {
				switch binary.LittleEndian.Uint32(s[i:]) {
				case 0:
					pending.Maximized = true
				case 1:
					pending.Minimized = true
				case 2:
					pending.Activated = true
				case 3:
					pending.Fullscreen = true
				}
			}
		case 5: // done
			v := *pending
			t.mu.Lock()
			t.m[id] = &v
			t.mu.Unlock()
		case 6: // closed
			t.mu.Lock()
			delete(t.m, id)
			t.mu.Unlock()
			t.c.Request(handle, 7).Send() // destroy
		}
	}
}

// List returns all windows in a stable order.
func (t *Toplevels) List() []Toplevel {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Toplevel, 0, len(t.m))
	for _, v := range t.m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Activate focuses and raises a window, unminimizing it first.
func (t *Toplevels) Activate(id, seat uint32) error {
	h, err := t.lookup(id)
	if err != nil {
		return err
	}
	t.c.Request(h, 3).Send() // unset_minimized
	return t.c.Request(h, 4).Uint(seat).Send()
}

// Close asks a window to close.
func (t *Toplevels) Close(id uint32) error {
	h, err := t.lookup(id)
	if err != nil {
		return err
	}
	return t.c.Request(h, 5).Send()
}

func (t *Toplevels) lookup(id uint32) (uint32, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if w, ok := t.m[id]; ok {
		return w.handle, nil
	}
	return 0, fmt.Errorf("no window with id %d", id)
}
