// Package wl is a small, dependency-free Wayland client. It implements just
// enough of the wire protocol to drive compositors for screen capture,
// virtual input, toplevel listing and clipboard access.
//
// A Conn runs one reader goroutine that dispatches events to per-object
// handlers. Requests may be sent from any goroutine.
package wl

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var le = binary.LittleEndian

// Handler receives events for one object. It runs on the reader goroutine,
// so it must not block on other Wayland round trips.
type Handler func(opcode uint16, e *Event)

// Global is a compositor global advertised through wl_registry.
type Global struct {
	Name      uint32
	Interface string
	Version   uint32
}

// Conn is a client connection to a Wayland compositor.
type Conn struct {
	sock *net.UnixConn
	wmu  sync.Mutex

	mu       sync.Mutex
	registry uint32
	handlers map[uint32]Handler
	nextID   uint32
	freeIDs  []uint32
	fds      []int
	globals  map[uint32]Global
	onGlobal []func(g Global, added bool)

	closed chan struct{}
	err    error
}

// SocketPath resolves the compositor socket for a WAYLAND_DISPLAY value.
func SocketPath(display string) string {
	if filepath.IsAbs(display) {
		return display
	}
	return filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), display)
}

// Dial connects to the compositor socket for display (e.g. "wayland-0")
// and fetches the initial set of globals.
func Dial(display string) (*Conn, error) {
	if display == "" {
		return nil, errors.New("no Wayland display")
	}
	sock, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: SocketPath(display), Net: "unix"})
	if err != nil {
		return nil, err
	}
	c := &Conn{
		sock:     sock,
		handlers: map[uint32]Handler{},
		nextID:   2, // 1 is wl_display
		globals:  map[uint32]Global{},
		closed:   make(chan struct{}),
	}
	c.handlers[1] = c.handleDisplay
	go c.readLoop()

	registry := c.NewObject(c.handleRegistry)
	if err := c.Request(1, 1).Uint(registry).Send(); err != nil {
		c.Close()
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Roundtrip(ctx); err != nil {
		c.Close()
		return nil, err
	}
	c.registry = registry
	return c, nil
}

// Close shuts the connection down.
func (c *Conn) Close() error {
	c.fail(errors.New("connection closed"))
	return nil
}

// Err returns the fatal error that closed the connection, if any.
func (c *Conn) Err() error {
	select {
	case <-c.closed:
		return c.err
	default:
		return nil
	}
}

// Done is closed when the connection dies.
func (c *Conn) Done() <-chan struct{} { return c.closed }

func (c *Conn) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closed:
		return
	default:
	}
	c.err = err
	close(c.closed)
	c.sock.Close()
	for _, fd := range c.fds {
		unix.Close(fd)
	}
	c.fds = nil
}

// NewObject allocates a client object id and registers its event handler.
// A nil handler ignores all events.
func (c *Conn) NewObject(h Handler) uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var id uint32
	if n := len(c.freeIDs); n > 0 {
		id = c.freeIDs[n-1]
		c.freeIDs = c.freeIDs[:n-1]
	} else {
		id = c.nextID
		c.nextID++
	}
	c.handlers[id] = h
	return id
}

// SetHandler registers a handler for a server-created object (new_id events).
func (c *Conn) SetHandler(id uint32, h Handler) {
	c.mu.Lock()
	c.handlers[id] = h
	c.mu.Unlock()
}

// Roundtrip blocks until the compositor has processed every request sent so far.
func (c *Conn) Roundtrip(ctx context.Context) error {
	done := make(chan struct{})
	cb := c.NewObject(func(op uint16, _ *Event) {
		if op == 0 {
			close(done)
		}
	})
	if err := c.Request(1, 0).Uint(cb).Send(); err != nil {
		return err
	}
	return c.Wait(ctx, done)
}

// Wait blocks until done is closed, the connection dies, or ctx ends.
func (c *Conn) Wait(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-c.closed:
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Globals returns every global currently advertised with the given interface.
func (c *Conn) Globals(iface string) []Global {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Global
	for _, g := range c.globals {
		if g.Interface == iface {
			out = append(out, g)
		}
	}
	return out
}

// Has reports whether the compositor advertises an interface.
func (c *Conn) Has(iface string) bool { return len(c.Globals(iface)) > 0 }

// OnGlobal subscribes to globals being added or removed after Dial.
func (c *Conn) OnGlobal(f func(g Global, added bool)) {
	c.mu.Lock()
	c.onGlobal = append(c.onGlobal, f)
	c.mu.Unlock()
}

// Bind binds a global at min(version, advertised) and returns the new object id
// and the negotiated version.
func (c *Conn) Bind(g Global, version uint32, h Handler) (uint32, uint32, error) {
	v := min(version, g.Version)
	id := c.NewObject(h)
	err := c.Request(c.registry, 0).Uint(g.Name).String(g.Interface).Uint(v).Uint(id).Send()
	return id, v, err
}

// BindFirst binds the first advertised global of an interface.
func (c *Conn) BindFirst(iface string, version uint32, h Handler) (uint32, uint32, error) {
	gs := c.Globals(iface)
	if len(gs) == 0 {
		return 0, 0, fmt.Errorf("compositor does not support %s", iface)
	}
	return c.Bind(gs[0], version, h)
}

func (c *Conn) handleDisplay(op uint16, e *Event) {
	switch op {
	case 0: // error
		obj, code, msg := e.Uint(), e.Uint(), e.Str()
		go c.fail(fmt.Errorf("wayland protocol error on object %d (code %d): %s", obj, code, msg))
	case 1: // delete_id
		id := e.Uint()
		c.mu.Lock()
		delete(c.handlers, id)
		c.freeIDs = append(c.freeIDs, id)
		c.mu.Unlock()
	}
}

func (c *Conn) handleRegistry(op uint16, e *Event) {
	c.mu.Lock()
	var g Global
	added := op == 0
	switch op {
	case 0:
		g = Global{Name: e.Uint(), Interface: e.Str(), Version: e.Uint()}
		c.globals[g.Name] = g
	case 1:
		g = c.globals[e.Uint()]
		delete(c.globals, g.Name)
	}
	subs := c.onGlobal
	c.mu.Unlock()
	for _, f := range subs {
		f(g, added)
	}
}

func (c *Conn) readLoop() {
	var pending []byte
	chunk := make([]byte, 8192)
	oob := make([]byte, unix.CmsgSpace(28*4))
	for {
		n, oobn, _, _, err := c.sock.ReadMsgUnix(chunk, oob)
		if err != nil {
			c.fail(fmt.Errorf("compositor connection lost: %w", err))
			return
		}
		if oobn > 0 {
			c.queueFDs(oob[:oobn])
		}
		data := chunk[:n]
		if len(pending) > 0 {
			pending = append(pending, data...)
			data = pending
		}
		for len(data) >= 8 {
			size := int(le.Uint32(data[4:]) >> 16)
			if size < 8 {
				c.fail(errors.New("malformed wayland message"))
				return
			}
			if len(data) < size {
				break
			}
			c.dispatch(le.Uint32(data), uint16(le.Uint32(data[4:])), data[8:size])
			data = data[size:]
		}
		pending = append(pending[:0], data...)
	}
}

func (c *Conn) queueFDs(oob []byte) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return
	}
	for _, m := range msgs {
		fds, err := unix.ParseUnixRights(&m)
		if err != nil {
			continue
		}
		c.mu.Lock()
		c.fds = append(c.fds, fds...)
		c.mu.Unlock()
	}
}

func (c *Conn) dispatch(id uint32, op uint16, payload []byte) {
	c.mu.Lock()
	h := c.handlers[id]
	c.mu.Unlock()
	if h != nil {
		h(op, &Event{c: c, b: payload})
	}
}

func (c *Conn) popFD() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.fds) == 0 {
		return -1
	}
	fd := c.fds[0]
	c.fds = c.fds[1:]
	return fd
}

// Event is a decoder over one event's arguments, read in protocol order.
type Event struct {
	c   *Conn
	b   []byte
	off int
}

func (e *Event) Uint() uint32 {
	if e.off+4 > len(e.b) {
		return 0
	}
	v := le.Uint32(e.b[e.off:])
	e.off += 4
	return v
}

func (e *Event) Int() int32 { return int32(e.Uint()) }

func (e *Event) Fixed() float64 { return float64(e.Int()) / 256 }

func (e *Event) Array() []byte {
	n := int(e.Uint())
	if n == 0 || e.off+n > len(e.b) {
		return nil
	}
	v := append([]byte(nil), e.b[e.off:e.off+n]...)
	e.off += (n + 3) &^ 3
	return v
}

func (e *Event) Str() string {
	b := e.Array()
	if len(b) == 0 {
		return ""
	}
	return string(b[:len(b)-1]) // drop NUL terminator
}

// FD takes the next file descriptor received with this event. The caller owns it.
func (e *Event) FD() int { return e.c.popFD() }

// Req builds one request message.
type Req struct {
	c   *Conn
	b   []byte
	fds []int
}

// Request starts a request with opcode op on object id.
func (c *Conn) Request(id uint32, op uint16) *Req {
	b := make([]byte, 8, 64)
	le.PutUint32(b, id)
	le.PutUint32(b[4:], uint32(op))
	return &Req{c: c, b: b}
}

func (r *Req) Uint(v uint32) *Req { r.b = le.AppendUint32(r.b, v); return r }

func (r *Req) Int(v int32) *Req { return r.Uint(uint32(v)) }

func (r *Req) Fixed(v float64) *Req { return r.Int(int32(math.Round(v * 256))) }

func (r *Req) String(s string) *Req { return r.Array(append([]byte(s), 0)) }

func (r *Req) Array(v []byte) *Req {
	r.Uint(uint32(len(v)))
	r.b = append(r.b, v...)
	for len(r.b)%4 != 0 {
		r.b = append(r.b, 0)
	}
	return r
}

// FD attaches a file descriptor. It is duplicated by the kernel on send; the
// caller keeps ownership of fd.
func (r *Req) FD(fd int) *Req { r.fds = append(r.fds, fd); return r }

// Send writes the request to the compositor.
func (r *Req) Send() error {
	le.PutUint32(r.b[4:], uint32(len(r.b))<<16|le.Uint32(r.b[4:])&0xffff)
	if err := r.c.Err(); err != nil {
		return err
	}
	r.c.wmu.Lock()
	defer r.c.wmu.Unlock()
	var err error
	if len(r.fds) > 0 {
		_, _, err = r.c.sock.WriteMsgUnix(r.b, unix.UnixRights(r.fds...), nil)
	} else {
		_, err = r.c.sock.Write(r.b)
	}
	return err
}
