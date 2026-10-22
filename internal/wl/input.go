package wl

import (
	"time"

	"golang.org/x/sys/unix"
)

var epoch = time.Now()

func now() uint32 { return uint32(time.Since(epoch).Milliseconds()) }

// Pointer is a zwlr_virtual_pointer_v1.
type Pointer struct {
	c  *Conn
	id uint32
}

// NewPointer creates a virtual pointer on seat (0 lets the compositor choose).
func NewPointer(c *Conn, mgr, seat uint32) (*Pointer, error) {
	id := c.NewObject(nil)
	if err := c.Request(mgr, 0).Uint(seat).Uint(id).Send(); err != nil {
		return nil, err
	}
	return &Pointer{c: c, id: id}, nil
}

// MoveAbs warps the pointer to (x, y) within an extent spanning the whole
// output layout.
func (p *Pointer) MoveAbs(x, y, width, height uint32) error {
	p.c.Request(p.id, 1).Uint(now()).Uint(x).Uint(y).Uint(width).Uint(height).Send()
	return p.frame()
}

// Button presses or releases a Linux input button code (BTN_LEFT = 0x110...).
func (p *Pointer) Button(code uint32, pressed bool) error {
	state := uint32(0)
	if pressed {
		state = 1
	}
	p.c.Request(p.id, 2).Uint(now()).Uint(code).Uint(state).Send()
	return p.frame()
}

// Scroll sends wheel clicks on axis 0 (vertical) or 1 (horizontal). Positive
// values scroll down or right.
func (p *Pointer) Scroll(axis uint32, clicks int) error {
	t := now()
	p.c.Request(p.id, 5).Uint(0).Send() // axis_source wheel
	p.c.Request(p.id, 7).Uint(t).Uint(axis).Fixed(float64(clicks) * 15).Int(int32(clicks)).Send()
	return p.frame()
}

func (p *Pointer) frame() error { return p.c.Request(p.id, 4).Send() }

// Keyboard is a zwp_virtual_keyboard_v1. Keycodes are Linux evdev codes.
type Keyboard struct {
	c  *Conn
	id uint32
}

// NewKeyboard creates a virtual keyboard on seat.
func NewKeyboard(c *Conn, mgr, seat uint32) (*Keyboard, error) {
	id := c.NewObject(nil)
	if err := c.Request(mgr, 0).Uint(seat).Uint(id).Send(); err != nil {
		return nil, err
	}
	return &Keyboard{c: c, id: id}, nil
}

// SetKeymap uploads an XKB v1 text keymap.
func (k *Keyboard) SetKeymap(keymap string) error {
	data := append([]byte(keymap), 0)
	fd, err := Memfd("linux-use-keymap", data, len(data))
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return k.c.Request(k.id, 0).Uint(1).FD(fd).Uint(uint32(len(data))).Send()
}

// Key presses or releases an evdev keycode.
func (k *Keyboard) Key(code uint32, pressed bool) error {
	state := uint32(0)
	if pressed {
		state = 1
	}
	return k.c.Request(k.id, 1).Uint(now()).Uint(code).Uint(state).Send()
}

// Modifiers sets the depressed modifier mask. Virtual key presses do not
// update compositor modifier state on their own, so callers send this
// alongside modifier key events.
func (k *Keyboard) Modifiers(depressed uint32) error {
	return k.c.Request(k.id, 2).Uint(depressed).Uint(0).Uint(0).Uint(0).Send()
}
