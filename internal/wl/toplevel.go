package wl

import (
	"encoding/binary"
	"sort"
	"sync"
)

// Toplevel is a window reported by zwlr_foreign_toplevel_manager_v1.
type Toplevel struct {
	ID         uint32
	Title      string
	AppID      string
	Activated  bool
	Maximized  bool
	Minimized  bool
	Fullscreen bool
}

// Toplevels tracks every window the compositor exposes.
type Toplevels struct {
	c  *Conn
	mu sync.Mutex
	m  map[uint32]*Toplevel // committed state, updated on each done event
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

func (t *Toplevels) handle(id uint32) Handler {
	pending := &Toplevel{ID: id}
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
			t.c.Request(id, 7).Send() // destroy
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
	t.c.Request(id, 3).Send() // unset_minimized
	return t.c.Request(id, 4).Uint(seat).Send()
}

// Close asks a window to close.
func (t *Toplevels) Close(id uint32) error { return t.c.Request(id, 5).Send() }
