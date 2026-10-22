package wl

import (
	"sort"
	"sync"
)

// Output describes one wl_output, merged with its xdg_output logical geometry.
type Output struct {
	ID          uint32 // wl_output object id
	global      uint32
	Name        string
	Description string
	// Logical position and size in the compositor's global coordinate space.
	X, Y, Width, Height int
	// Current mode in physical pixels, before transform.
	ModeWidth, ModeHeight int
	Scale                 int
	Transform             int
	hasLogical            bool
}

// Rotated reports whether the output's transform swaps width and height.
func (o Output) Rotated() bool { return o.Transform%2 == 1 }

// Outputs tracks every wl_output the compositor advertises, including hotplug.
type Outputs struct {
	c       *Conn
	xdgMgr  uint32
	mu      sync.Mutex
	outputs map[uint32]*Output // keyed by registry name
}

// TrackOutputs binds all current and future outputs. Call Roundtrip afterwards
// (twice when xdg-output is present) before reading geometry.
func TrackOutputs(c *Conn) *Outputs {
	t := &Outputs{c: c, outputs: map[uint32]*Output{}}
	if gs := c.Globals("zxdg_output_manager_v1"); len(gs) > 0 {
		t.xdgMgr, _, _ = c.Bind(gs[0], 3, nil)
	}
	c.OnGlobal(func(g Global, added bool) {
		if g.Interface != "wl_output" {
			return
		}
		if added {
			t.add(g)
			return
		}
		t.mu.Lock()
		delete(t.outputs, g.Name)
		t.mu.Unlock()
	})
	for _, g := range c.Globals("wl_output") {
		t.add(g)
	}
	return t
}

func (t *Outputs) add(g Global) {
	o := &Output{global: g.Name, Scale: 1}
	t.mu.Lock()
	t.outputs[g.Name] = o
	t.mu.Unlock()

	id, _, err := t.c.Bind(g, 4, func(op uint16, e *Event) {
		t.mu.Lock()
		defer t.mu.Unlock()
		switch op {
		case 0: // geometry
			x, y := e.Int(), e.Int()
			e.Int()
			e.Int()
			e.Int()
			e.Str()
			e.Str()
			o.Transform = int(e.Int())
			if !o.hasLogical {
				o.X, o.Y = int(x), int(y)
			}
		case 1: // mode
			flags, w, h := e.Uint(), e.Int(), e.Int()
			if flags&1 != 0 {
				o.ModeWidth, o.ModeHeight = int(w), int(h)
			}
		case 3: // scale
			o.Scale = int(e.Int())
		case 4: // name
			o.Name = e.Str()
		case 5: // description
			o.Description = e.Str()
		}
	})
	if err != nil {
		return
	}
	t.mu.Lock()
	o.ID = id
	t.mu.Unlock()

	if t.xdgMgr == 0 {
		return
	}
	xdg := t.c.NewObject(func(op uint16, e *Event) {
		t.mu.Lock()
		defer t.mu.Unlock()
		switch op {
		case 0: // logical_position
			o.X, o.Y = int(e.Int()), int(e.Int())
			o.hasLogical = true
		case 1: // logical_size
			o.Width, o.Height = int(e.Int()), int(e.Int())
			o.hasLogical = true
		case 3: // name
			if o.Name == "" {
				o.Name = e.Str()
			}
		}
	})
	t.c.Request(t.xdgMgr, 1).Uint(xdg).Uint(id).Send()
}

// List returns outputs sorted by position (left to right, top to bottom).
func (t *Outputs) List() []Output {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Output, 0, len(t.outputs))
	for _, o := range t.outputs {
		if o.ID == 0 || o.ModeWidth == 0 {
			continue
		}
		v := *o
		if v.Width == 0 || v.Height == 0 {
			// No xdg-output: derive logical size from mode, transform and scale.
			w, h := v.ModeWidth, v.ModeHeight
			if v.Rotated() {
				w, h = h, w
			}
			v.Width, v.Height = w/max(v.Scale, 1), h/max(v.Scale, 1)
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].X != out[j].X {
			return out[i].X < out[j].X
		}
		return out[i].Y < out[j].Y
	})
	return out
}
