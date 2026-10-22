package wl

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var textMimes = []string{"text/plain;charset=utf-8", "UTF8_STRING", "text/plain", "TEXT", "STRING"}

// Clipboard reads and writes the regular selection through a data-control
// protocol (ext-data-control-v1 or zwlr_data_control_manager_v1; both share
// the same wire layout).
type Clipboard struct {
	c      *Conn
	mgr    uint32
	device uint32

	mu        sync.Mutex
	offers    map[uint32][]string
	selection uint32
	primary   uint32
	text      string // what we last offered, served on paste requests
}

// NewClipboard binds whichever data-control manager the compositor offers.
func NewClipboard(c *Conn, seat uint32) (*Clipboard, error) {
	cb := &Clipboard{c: c, offers: map[uint32][]string{}}
	var err error
	cb.mgr, _, err = c.BindFirst("ext_data_control_manager_v1", 1, nil)
	if err != nil {
		cb.mgr, _, err = c.BindFirst("zwlr_data_control_manager_v1", 2, nil)
	}
	if err != nil {
		return nil, errors.New("compositor has no clipboard (data-control) protocol")
	}
	cb.device = c.NewObject(cb.handleDevice)
	if err := c.Request(cb.mgr, 1).Uint(cb.device).Uint(seat).Send(); err != nil {
		return nil, err
	}
	return cb, nil
}

func (cb *Clipboard) handleDevice(op uint16, e *Event) {
	switch op {
	case 0: // data_offer(new_id)
		id := e.Uint()
		cb.mu.Lock()
		cb.offers[id] = nil
		cb.mu.Unlock()
		cb.c.SetHandler(id, func(op uint16, e *Event) {
			if op == 0 {
				mime := e.Str()
				cb.mu.Lock()
				cb.offers[id] = append(cb.offers[id], mime)
				cb.mu.Unlock()
			}
		})
	case 1: // selection
		cb.replace(&cb.selection, e.Uint())
	case 3: // primary_selection
		cb.replace(&cb.primary, e.Uint())
	}
}

// replace swaps the current offer in slot and destroys the old one.
func (cb *Clipboard) replace(slot *uint32, id uint32) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	old := *slot
	*slot = id
	if old != 0 && old != cb.selection && old != cb.primary {
		delete(cb.offers, old)
		cb.c.Request(old, 1).Send() // offer.destroy
		cb.c.Forget(old)
	}
}

// Read returns the current clipboard text ("" when empty or non-text).
func (cb *Clipboard) Read(ctx context.Context) (string, error) {
	if err := cb.c.Roundtrip(ctx); err != nil {
		return "", err
	}
	cb.mu.Lock()
	offer := cb.selection
	mimes := cb.offers[offer]
	if offer == 0 {
		cb.mu.Unlock()
		return "", nil
	}
	mime := ""
	for _, m := range textMimes {
		if slices.Contains(mimes, m) {
			mime = m
			break
		}
	}
	if mime == "" {
		cb.mu.Unlock()
		return "", nil
	}
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_CLOEXEC); err != nil {
		cb.mu.Unlock()
		return "", err
	}
	// Send while holding the lock so the offer can't be destroyed meanwhile.
	err := cb.c.Request(offer, 0).String(mime).FD(p[1]).Send()
	cb.mu.Unlock()
	unix.Close(p[1])
	if err != nil {
		unix.Close(p[0])
		return "", err
	}
	unix.SetNonblock(p[0], true)
	f := os.NewFile(uintptr(p[0]), "clipboard")
	defer f.Close()
	deadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	f.SetReadDeadline(deadline)
	b, err := io.ReadAll(io.LimitReader(f, 16<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Write takes ownership of the clipboard and serves text to pasting clients
// until another client replaces the selection.
func (cb *Clipboard) Write(ctx context.Context, text string) error {
	source := cb.c.NewObject(nil)
	cb.c.SetHandler(source, func(op uint16, e *Event) {
		switch op {
		case 0: // send(mime, fd)
			e.Str()
			fd := e.FD()
			if fd < 0 {
				return
			}
			go func() {
				f := os.NewFile(uintptr(fd), "paste")
				f.WriteString(text)
				f.Close()
			}()
		case 1: // cancelled
			cb.c.Request(source, 1).Send()
		}
	})
	if err := cb.c.Request(cb.mgr, 0).Uint(source).Send(); err != nil {
		return err
	}
	for _, m := range textMimes {
		cb.c.Request(source, 0).String(m).Send()
	}
	if err := cb.c.Request(cb.device, 0).Uint(source).Send(); err != nil {
		return err
	}
	return cb.c.Roundtrip(ctx)
}
