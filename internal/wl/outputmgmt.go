package wl

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type head struct {
	id, mode  uint32
	name      string
	enabled   bool
	x, y      int32
	transform int32
	scale     float64
}

// SetOutputMode resizes an output through zwlr_output_manager_v1. Custom
// modes let headless outputs take any size; physical outputs accept only
// modes they advertise. Pass an empty name to resize the first output.
func SetOutputMode(ctx context.Context, c *Conn, name string, width, height int) error {
	var (
		mu     sync.Mutex
		heads  = map[uint32]*head{}
		serial uint32
		ready  = make(chan struct{})
	)
	mgr, _, err := c.BindFirst("zwlr_output_manager_v1", 2, func(op uint16, e *Event) {
		switch op {
		case 0: // head(new_id)
			h := &head{id: e.Uint(), scale: 1}
			mu.Lock()
			heads[h.id] = h
			mu.Unlock()
			c.SetHandler(h.id, func(op uint16, e *Event) {
				mu.Lock()
				defer mu.Unlock()
				switch op {
				case 0:
					h.name = e.Str()
				case 3: // mode(new_id): modes are only referenced, not inspected
					c.SetHandler(e.Uint(), nil)
				case 4:
					h.enabled = e.Int() != 0
				case 5:
					h.mode = e.Uint()
				case 6:
					h.x, h.y = e.Int(), e.Int()
				case 7:
					h.transform = e.Int()
				case 8:
					h.scale = e.Fixed()
				}
			})
		case 1: // done
			mu.Lock()
			serial = e.Uint()
			mu.Unlock()
			select {
			case <-ready:
			default:
				close(ready)
			}
		}
	})
	if err != nil {
		return err
	}
	defer c.Request(mgr, 1).Send() // stop
	if err := c.Wait(ctx, ready); err != nil {
		return err
	}

	result := make(chan error, 1)
	cfg := c.NewObject(func(op uint16, _ *Event) {
		switch op {
		case 0:
			result <- nil
		case 1:
			result <- errors.New("compositor rejected the output mode")
		case 2:
			result <- errors.New("output configuration changed concurrently; retry")
		}
	})
	mu.Lock()
	c.Request(mgr, 0).Uint(cfg).Uint(serial).Send()
	found := false
	for _, h := range heads {
		target := !found && (h.name == name || name == "")
		if !h.enabled && !target {
			c.Request(cfg, 1).Uint(h.id).Send()
			continue
		}
		ch := c.NewObject(nil)
		c.Request(cfg, 0).Uint(ch).Uint(h.id).Send()
		if target {
			found = true
			c.Request(ch, 1).Int(int32(width)).Int(int32(height)).Int(0).Send()
		} else if h.mode != 0 {
			c.Request(ch, 0).Uint(h.mode).Send()
		}
		c.Request(ch, 2).Int(h.x).Int(h.y).Send()
		c.Request(ch, 3).Int(h.transform).Send()
		c.Request(ch, 4).Fixed(h.scale).Send()
	}
	mu.Unlock()
	if !found {
		c.Request(cfg, 4).Send()
		return fmt.Errorf("no output named %q", name)
	}
	c.Request(cfg, 2).Send() // apply
	defer c.Request(cfg, 4).Send()
	select {
	case err := <-result:
		return err
	case <-c.Done():
		return c.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
