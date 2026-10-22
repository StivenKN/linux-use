// Package pw is a small, dependency-free PipeWire client. It speaks just
// enough of the native protocol (version 3) to link a client-node to another
// node's video output and read its frames from shared memory, which is how
// Mutter's ScreenCast API delivers the screen.
//
// A Conn runs one reader goroutine that handles core events and dispatches
// the rest to per-object handlers. Requests may be sent from any goroutine.
package pw

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// Core methods and events (pipewire/core.h).
const (
	coreHello        = 1
	coreSync         = 2
	corePong         = 3
	coreCreateObject = 6
	coreDestroy      = 7

	coreEventDone       = 1
	coreEventPing       = 2
	coreEventError      = 3
	coreEventRemoveID   = 4
	coreEventBoundID    = 5
	coreEventAddMem     = 6
	coreEventRemoveMem  = 7
	coreEventBoundProps = 8

	clientUpdateProperties = 2

	coreVersion = 4
	invalidID   = 0xffffffff
)

// handler receives one event for an object on the reader goroutine.
type handler func(op uint8, f *fields, m *message)

type proxy struct {
	event handler
	fail  func(error) // the server reported an error for this object
}

// message carries the file descriptors sent with one event. Handlers claim
// the ones they keep; the reader closes the rest.
type message struct {
	fds   []int
	taken []bool
}

func (m *message) fd(i int) int {
	if i < 0 || i >= len(m.fds) {
		return -1
	}
	m.taken[i] = true
	return m.fds[i]
}

// memBlock is a block of shared memory announced by the server. It is
// mapped whole on first use and unmapped when the server removes it.
type memBlock struct {
	fd    int
	typ   uint32
	flags uint32
	data  []byte
}

const memWritable = 1 << 1 // PW_MEMBLOCK_FLAG_WRITABLE

// Conn is a client connection to the PipeWire daemon.
type Conn struct {
	sock *net.UnixConn
	wmu  sync.Mutex // serializes writes and seq
	seq  uint32

	mu       sync.Mutex
	nextID   uint32
	proxies  map[uint32]proxy
	bound    map[uint32]uint32 // proxy id -> global id
	mem      map[uint32]*memBlock
	syncs    map[int32]chan struct{}
	syncSeq  int32
	fdq      []int // received fds not yet claimed by a message
	streams  map[*Stream]struct{}
	boundSig chan struct{} // closed and replaced when bound changes

	closed chan struct{}
	err    error
}

// SocketPath returns the daemon socket the way libpipewire finds it.
func SocketPath() string {
	name := os.Getenv("PIPEWIRE_REMOTE")
	if name == "" {
		name = "pipewire-0"
	}
	if filepath.IsAbs(name) {
		return name
	}
	dir := os.Getenv("PIPEWIRE_RUNTIME_DIR")
	if dir == "" {
		dir = os.Getenv("XDG_RUNTIME_DIR")
	}
	return filepath.Join(dir, name)
}

// Dial connects to the PipeWire daemon and completes the handshake.
func Dial(ctx context.Context) (*Conn, error) {
	sock, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: SocketPath(), Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("connecting to PipeWire: %w", err)
	}
	c := &Conn{
		sock:     sock,
		nextID:   2, // 0 is the core, 1 our client object
		proxies:  map[uint32]proxy{},
		bound:    map[uint32]uint32{},
		mem:      map[uint32]*memBlock{},
		syncs:    map[int32]chan struct{}{},
		streams:  map[*Stream]struct{}{},
		boundSig: make(chan struct{}),
		closed:   make(chan struct{}),
	}
	go c.readLoop()

	var w builder
	err = c.send(0, coreHello, w.Struct(func() { w.Int(coreVersion) }).bytes())
	if err == nil {
		var p builder
		err = c.send(1, clientUpdateProperties, p.Struct(func() {
			p.Dict([]string{
				"application.name", "linux-use",
				"application.process.id", fmt.Sprint(os.Getpid()),
			})
		}).bytes())
	}
	if err == nil {
		err = c.Sync(ctx)
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Close stops every stream, then drops the connection and its shared memory.
func (c *Conn) Close() error {
	c.mu.Lock()
	streams := make([]*Stream, 0, len(c.streams))
	for s := range c.streams {
		streams = append(streams, s)
	}
	c.mu.Unlock()
	for _, s := range streams {
		s.Close()
	}
	c.fail(errors.New("connection closed"))
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, m := range c.mem {
		m.release()
		delete(c.mem, id)
	}
	for _, fd := range c.fdq {
		unix.Close(fd)
	}
	c.fdq = nil
	return nil
}

// Err returns why the connection died, or nil while it is alive.
func (c *Conn) Err() error {
	select {
	case <-c.closed:
		return c.err
	default:
		return nil
	}
}

// fail marks the connection dead. Shared memory stays mapped until Close,
// because stream goroutines may still be reading it.
func (c *Conn) fail(err error) {
	c.mu.Lock()
	select {
	case <-c.closed:
		c.mu.Unlock()
		return
	default:
	}
	c.err = err
	close(c.closed)
	proxies := c.proxies
	c.proxies = map[uint32]proxy{}
	c.mu.Unlock()
	c.sock.Close()
	for _, p := range proxies {
		if p.fail != nil {
			p.fail(err)
		}
	}
}

// send writes one method call on object id.
func (c *Conn) send(id uint32, op uint8, body []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	msg := make([]byte, 16, 16+len(body))
	le.PutUint32(msg, id)
	le.PutUint32(msg[4:], uint32(op)<<24|uint32(len(body))&0xffffff)
	le.PutUint32(msg[8:], c.seq)
	c.seq++
	if _, err := c.sock.Write(append(msg, body...)); err != nil {
		return fmt.Errorf("PipeWire: %w", err)
	}
	return nil
}

// newObject allocates a proxy id. Ids are never reused: the server expects
// them dense and in order, and a capture session creates only a handful.
func (c *Conn) newObject(p proxy) uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextID
	c.nextID++
	c.proxies[id] = p
	return id
}

// createObject asks factory to create an object for proxy id.
func (c *Conn) createObject(id uint32, factory, iface string, version uint32, props []string) error {
	var w builder
	w.Struct(func() {
		w.String(factory).String(iface).Int(int32(version))
		w.Dict(props)
		w.Int(int32(id))
	})
	return c.send(0, coreCreateObject, w.bytes())
}

// destroy asks the server to destroy the object behind a proxy.
func (c *Conn) destroy(id uint32) error {
	var w builder
	return c.send(0, coreDestroy, w.Struct(func() { w.Int(int32(id)) }).bytes())
}

// Sync waits until the server has processed every request sent so far.
func (c *Conn) Sync(ctx context.Context) error {
	c.mu.Lock()
	c.syncSeq++
	seq := c.syncSeq
	done := make(chan struct{})
	c.syncs[seq] = done
	c.mu.Unlock()
	var w builder
	if err := c.send(0, coreSync, w.Struct(func() { w.Int(0).Int(seq) }).bytes()); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-c.closed:
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// globalID waits for the server to tell us the global id of a proxy's object.
func (c *Conn) globalID(ctx context.Context, id uint32) (uint32, error) {
	for {
		c.mu.Lock()
		g, ok := c.bound[id]
		sig := c.boundSig
		c.mu.Unlock()
		if ok {
			return g, nil
		}
		select {
		case <-sig:
		case <-c.closed:
			return 0, c.err
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

// mapMem returns size bytes at offset in shared memory block id.
func (c *Conn) mapMem(id, offset, size uint32) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.mem[id]
	if m == nil {
		return nil, fmt.Errorf("PipeWire: unknown memory block %d", id)
	}
	if m.data == nil {
		if m.typ != dataMemFd {
			return nil, fmt.Errorf("PipeWire: memory block %d has type %d, not a memfd", id, m.typ)
		}
		var st unix.Stat_t
		if err := unix.Fstat(m.fd, &st); err != nil {
			return nil, err
		}
		prot := unix.PROT_READ
		if m.flags&memWritable != 0 {
			prot |= unix.PROT_WRITE
		}
		data, err := unix.Mmap(m.fd, 0, int(st.Size), prot, unix.MAP_SHARED)
		if err != nil {
			return nil, fmt.Errorf("PipeWire: mapping memory block %d: %w", id, err)
		}
		m.data = data
	}
	end := uint64(offset) + uint64(size)
	if end > uint64(len(m.data)) {
		return nil, fmt.Errorf("PipeWire: range %d+%d is outside memory block %d", offset, size, id)
	}
	return m.data[offset:end:end], nil
}

func (m *memBlock) release() {
	if m.data != nil {
		unix.Munmap(m.data)
		m.data = nil
	}
	unix.Close(m.fd)
}

func (c *Conn) readLoop() {
	var pending []byte
	chunk := make([]byte, 64<<10)
	oob := make([]byte, unix.CmsgSpace(28*4))
	for {
		n, oobn, _, _, err := c.sock.ReadMsgUnix(chunk, oob)
		if err != nil {
			c.fail(fmt.Errorf("PipeWire connection lost: %w", err))
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
		for len(data) >= 16 {
			size := int(le.Uint32(data[4:]) & 0xffffff)
			if len(data) < 16+size {
				break
			}
			c.dispatch(le.Uint32(data), uint8(le.Uint32(data[4:])>>24), data[16:16+size], int(le.Uint32(data[12:])))
			data = data[16+size:]
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
		c.fdq = append(c.fdq, fds...)
		c.mu.Unlock()
	}
}

func (c *Conn) dispatch(id uint32, op uint8, body []byte, nfds int) {
	c.mu.Lock()
	nfds = min(nfds, len(c.fdq))
	m := &message{fds: c.fdq[:nfds:nfds], taken: make([]bool, nfds)}
	c.fdq = c.fdq[nfds:]
	p, ok := c.proxies[id]
	c.mu.Unlock()
	defer func() {
		for i, fd := range m.fds {
			if !m.taken[i] {
				unix.Close(fd)
			}
		}
	}()
	f := parseStruct(body)
	switch {
	case id == 0:
		c.coreEvent(op, f, m)
	case ok && p.event != nil:
		p.event(op, f, m)
	}
}

func (c *Conn) coreEvent(op uint8, f *fields, m *message) {
	switch op {
	case coreEventDone:
		f.Uint()
		seq := f.Int()
		c.mu.Lock()
		if done := c.syncs[seq]; done != nil {
			close(done)
			delete(c.syncs, seq)
		}
		c.mu.Unlock()
	case coreEventPing:
		id, seq := f.Int(), f.Int()
		var w builder
		c.send(0, corePong, w.Struct(func() { w.Int(id).Int(seq) }).bytes())
	case coreEventError:
		id, _, res, msg := f.Uint(), f.Int(), f.Int(), f.String()
		err := fmt.Errorf("PipeWire: %s (%v)", msg, syscall.Errno(-res))
		if id == 0 {
			c.fail(err)
			return
		}
		c.mu.Lock()
		p := c.proxies[id]
		c.mu.Unlock()
		if p.fail != nil {
			p.fail(err)
		}
	case coreEventRemoveID:
		id := f.Uint()
		c.mu.Lock()
		delete(c.proxies, id)
		delete(c.bound, id)
		c.mu.Unlock()
	case coreEventBoundID, coreEventBoundProps:
		id, global := f.Uint(), f.Uint()
		if f.Err() != nil {
			return
		}
		c.mu.Lock()
		c.bound[id] = global
		close(c.boundSig)
		c.boundSig = make(chan struct{})
		c.mu.Unlock()
	case coreEventAddMem:
		id, typ, idx, flags := f.Uint(), f.ID(), f.Fd(), f.Uint()
		fd := m.fd(idx)
		if f.Err() != nil || fd < 0 {
			return
		}
		c.mu.Lock()
		if old := c.mem[id]; old != nil {
			old.release()
		}
		c.mem[id] = &memBlock{fd: fd, typ: typ, flags: flags}
		c.mu.Unlock()
	case coreEventRemoveMem:
		id := f.Uint()
		c.mu.Lock()
		if m := c.mem[id]; m != nil {
			m.release()
			delete(c.mem, id)
		}
		c.mu.Unlock()
	}
}
