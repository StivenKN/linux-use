package pw

import (
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Client-node methods and events (pipewire/extensions/client-node.h).
const (
	clientNodeVersion = 6

	cnUpdate     = 2
	cnPortUpdate = 3
	cnSetActive  = 4

	cnEventTransport      = 0
	cnEventPortSetParam   = 7
	cnEventPortUseBuffers = 8
	cnEventPortSetIO      = 9
	cnEventSetActivation  = 10
	cnEventPortSetMixInfo = 11

	linkEventInfo  = 0
	linkStateError = -2
)

// SPA constants (spa/param, spa/buffer, spa/node).
const (
	objFormat   = 0x40003
	objParamIO  = 0x40006
	paramFormat = 4
	paramEnum   = 3
	paramIO     = 7

	formatMediaType    = 1
	formatMediaSubtype = 2
	formatVideoFormat  = 0x20001
	formatVideoSize    = 0x20003
	formatVideoMaxRate = 0x20005
	mediaVideo         = 2
	subtypeRaw         = 1

	videoRGBx = 7
	videoBGRx = 8
	videoRGBA = 11
	videoBGRA = 12

	ioParamID   = 1
	ioParamSize = 2
	ioBuffers   = 1
	ioAsync     = 10

	dataMemPtr = 1
	dataMemFd  = 2
	dataMemID  = 4

	chunkCorrupted = 1

	statusNeedData = 1
	statusHaveData = 2

	paramInfoRead  = 1 << 1
	paramInfoWrite = 1 << 2
)

// Offsets in struct pw_node_activation (pipewire/private.h); the layout is
// the same on every 64-bit Linux ABI.
const (
	actStatus        = 0
	actState0Status  = 8
	actState0Pending = 16
	actSignalTime    = 32
	actAwakeTime     = 40
	actFinishTime    = 48
	actClientVersion = 540
	actServerVersion = 544
	actMinSize       = 548

	actNotTriggered = 0
	actTriggered    = 1
	actAwake        = 2
	actFinished     = 3
)

// MaxFrameRate caps how often the producer sends frames. Mutter copies every
// frame into shared memory, so this bounds the cost while the screen changes.
const MaxFrameRate = 30

// ErrNoFrame reports that the producer never delivered a frame.
var ErrNoFrame = errors.New("no frame from PipeWire")

// Stream receives one video node's frames through a client-node whose input
// port is linked to the producer. It holds on to the newest frame's buffer
// until the next one arrives, so pixels are only copied when asked for.
//
// Shared memory is only touched under mu, and a mapping is released only
// after it was swapped out under mu, so nothing reads memory that is gone.
type Stream struct {
	c    *Conn
	id   uint32 // client-node proxy
	link uint32 // link proxy, 0 until created

	mu        sync.Mutex
	format    videoFormat
	formatPod []byte             // the negotiated Format, echoed in port updates
	mixes     map[uint32]*mix    // one per link to our input port
	act       []byte             // our pw_node_activation
	actBlk    *memBlock          // act's mapping
	targets   map[uint32]*target // nodes to signal when we finish a cycle
	wake      *os.File           // eventfd the graph signals us on
	wakeDone  chan struct{}      // closed when the wake goroutine exits
	held      *frameRef          // newest frame, kept out of the producer's hands
	last      time.Time          // when held arrived
	changed   chan struct{}      // closed and replaced on a frame, the transport or failure
	err       error
	closeOnce sync.Once
}

type videoFormat struct {
	format        uint32
	width, height int
	interval      time.Duration // minimum time between frames
}

type mix struct {
	ios     [][]byte // spa_io_buffers slots shared with the producer
	ioBlk   *memBlock
	buffers []buffer
	bufBlks []*memBlock // mappings behind buffers
}

type buffer struct {
	chunk []byte // spa_chunk: offset, size, stride, flags
	data  []byte // the plane's pixels
}

type frameRef struct {
	mix *mix
	id  uint32
}

type target struct {
	fd  int
	act []byte
	blk *memBlock
}

// Capture links a new client-node to the output of the given node (a global
// id, as Mutter's PipeWireStreamAdded reports it) and waits for its first frame.
func (c *Conn) Capture(ctx context.Context, node uint32) (*Stream, error) {
	s := &Stream{
		c:       c,
		mixes:   map[uint32]*mix{},
		targets: map[uint32]*target{},
		changed: make(chan struct{}),
	}
	c.mu.Lock()
	c.streams[s] = struct{}{}
	c.mu.Unlock()
	if err := s.start(ctx, node); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Stream) start(ctx context.Context, node uint32) error {
	s.id = s.c.newObject(proxy{event: s.event, fail: s.fail})
	err := s.c.createObject(s.id, "client-node", "PipeWire:Interface:ClientNode", clientNodeVersion, []string{
		"node.name", "linux-use",
		"node.description", "linux-use screen capture",
		"media.type", "Video",
		"media.category", "Capture",
		"media.role", "Screen",
		"media.class", "Stream/Input/Video",
		"node.autoconnect", "false",
	})
	if err != nil {
		return err
	}

	var w builder
	w.Struct(func() {
		w.Int(1 | 2).Int(0) // change mask: params, info; no node params
		w.Struct(func() {
			w.Int(1).Int(0)  // max input, output ports
			w.Long(1 << 0)   // info change mask: flags
			w.Long(0).Int(0) // flags, no props
			w.Int(0)         // no param infos
		})
	})
	if err := s.c.send(s.id, cnUpdate, w.bytes()); err != nil {
		return err
	}
	if err := s.portUpdate(); err != nil {
		return err
	}
	// Activate and link only once the transport set our activation version,
	// so the server never schedules us under the other protocol.
	global, err := s.registered(ctx)
	if err != nil {
		return err
	}
	var a builder
	if err := s.c.send(s.id, cnSetActive, a.Struct(func() { a.Bool(true) }).bytes()); err != nil {
		return err
	}
	s.link = s.c.newObject(proxy{event: s.linkEvent, fail: s.fail})
	err = s.c.createObject(s.link, "link-factory", "PipeWire:Interface:Link", 3, []string{
		"link.output.node", fmt.Sprint(node),
		"link.input.node", fmt.Sprint(global),
	})
	if err != nil {
		return err
	}
	return s.settle(ctx, time.Time{})
}

// registered waits for our node's global id and its transport.
func (s *Stream) registered(ctx context.Context) (uint32, error) {
	for {
		s.c.mu.Lock()
		global, bound := s.c.bound[s.id]
		boundSig := s.c.boundSig
		s.c.mu.Unlock()
		s.mu.Lock()
		ready, changed, err := s.act != nil, s.changed, s.err
		s.mu.Unlock()
		switch {
		case err != nil:
			return 0, err
		case bound && ready:
			return global, nil
		}
		select {
		case <-boundSig:
		case <-changed:
		case <-s.c.closed:
			return 0, s.c.err
		case <-ctx.Done():
			return 0, fmt.Errorf("PipeWire didn't set up the capture node: %w", ctx.Err())
		}
	}
}

// Close unlinks the stream and releases everything it holds. Safe to call twice.
func (s *Stream) Close() error {
	s.closeOnce.Do(func() {
		s.fail(errors.New("stream closed"))
		s.stopWake()
		if s.link != 0 {
			s.c.destroy(s.link)
		}
		if s.id != 0 {
			s.c.destroy(s.id)
		}
		s.mu.Lock()
		for id := range s.mixes {
			s.dropMix(id)
		}
		for id := range s.targets {
			s.dropTarget(id)
		}
		s.c.unref(s.actBlk)
		s.act, s.actBlk = nil, nil
		s.mu.Unlock()
		s.c.mu.Lock()
		delete(s.c.streams, s)
		delete(s.c.proxies, s.id)
		delete(s.c.proxies, s.link)
		s.c.mu.Unlock()
	})
	return nil
}

// Err reports why the stream stopped, or nil while it runs.
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Stream) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
		s.signal()
	}
}

// signal wakes everything waiting on s.changed. Callers hold s.mu.
func (s *Stream) signal() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// How Frame decides the screen has settled after input.
const (
	settleQuiet = 150 * time.Millisecond // no new frame for this long
	settleIdle  = 300 * time.Millisecond // input that changed nothing by then never will
	settleLimit = 500 * time.Millisecond // a screen still changing is captured as is
)

// Frame returns the newest frame as RGBA once the screen has settled after
// input, the time we last sent input to the screen. Mutter sends a frame
// only when the screen changes (at most MaxFrameRate per second, with a
// change inside that window sent as a follow-up), and apps repaint a few
// frames after input arrives. So after recent input, Frame waits until
// frames stop coming for settleQuiet; otherwise only for a follow-up frame
// that may still be due. Without a frame (at first, or after the buffers
// were renegotiated) it waits for one until ctx ends.
func (s *Stream) Frame(ctx context.Context, input time.Time) (*image.RGBA, error) {
	if err := s.settle(ctx, input); err != nil {
		return nil, err
	}
	// Copy the plane under the lock, which keeps the producer from getting
	// the buffer back, and convert it after, so a cycle isn't held up.
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return nil, s.err
	}
	if s.held == nil {
		s.mu.Unlock()
		return nil, ErrNoFrame
	}
	f := s.format
	pix, err := copyPlane(s.held.mix.buffers[s.held.id], f)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	toRGBA(pix, f.format)
	return &image.RGBA{Pix: pix, Stride: f.width * 4, Rect: image.Rect(0, 0, f.width, f.height)}, nil
}

// settle waits as Frame describes.
func (s *Stream) settle(ctx context.Context, input time.Time) error {
	var deadline, idle time.Time
	for {
		s.mu.Lock()
		have, last, changed, err := s.held != nil, s.last, s.changed, s.err
		followUp := s.format.interval + 10*time.Millisecond
		s.mu.Unlock()
		if err != nil {
			return err
		}
		until := time.Now().Add(time.Hour) // no frame yet; ctx bounds it
		if have {
			quiet := followUp
			if deadline.IsZero() {
				deadline = time.Now().Add(followUp)
				if time.Since(input) < settleLimit {
					deadline, idle = input.Add(settleLimit), input.Add(settleIdle)
				}
			}
			if !idle.IsZero() {
				quiet = max(quiet, settleQuiet)
			}
			until = deadline
			switch {
			case last.After(input):
				until = minTime(until, last.Add(quiet))
			case !idle.IsZero():
				until = minTime(until, idle)
			}
		}
		wait := time.Until(until)
		if wait <= 0 {
			return nil
		}
		t := time.NewTimer(wait)
		select {
		case <-changed:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			if !have {
				return fmt.Errorf("%w: %w", ErrNoFrame, ctx.Err())
			}
			return ctx.Err()
		}
		t.Stop()
	}
}

// copyPlane copies a frame's rows out of shared memory, dropping the
// stride padding. Callers hold s.mu.
func copyPlane(b buffer, f videoFormat) ([]byte, error) {
	offset, size, stride := le.Uint32(b.chunk), le.Uint32(b.chunk[4:]), int(int32(le.Uint32(b.chunk[8:])))
	row := f.width * 4
	if stride == 0 {
		stride = row
	}
	offset %= uint32(len(b.data))
	need := uint64(stride)*uint64(f.height-1) + uint64(row)
	if stride < row || uint64(offset)+need > uint64(len(b.data)) || uint64(size) < need {
		return nil, fmt.Errorf("PipeWire frame of %d bytes (stride %d) doesn't hold %dx%d pixels", size, stride, f.width, f.height)
	}
	src, pix := b.data[offset:], make([]byte, row*f.height)
	for y := range f.height {
		copy(pix[y*row:(y+1)*row], src[y*stride:])
	}
	return pix, nil
}

// toRGBA converts packed 32-bit pixels to opaque RGBA in place.
func toRGBA(pix []byte, format uint32) {
	bgr := format == videoBGRx || format == videoBGRA
	for i := 0; i+3 < len(pix); i += 4 {
		if bgr {
			pix[i], pix[i+2] = pix[i+2], pix[i]
		}
		pix[i+3] = 0xff
	}
}

// portUpdate tells the server about our input port: the formats we accept,
// the io area we need and, once negotiated, the format in use.
func (s *Stream) portUpdate() error {
	var e builder
	e.Object(objFormat, paramEnum, func() {
		e.Prop(formatMediaType).ID(mediaVideo)
		e.Prop(formatMediaSubtype).ID(subtypeRaw)
		e.Prop(formatVideoFormat).Choice(choiceEnum, typeID,
			idValue(videoBGRx), idValue(videoBGRx), idValue(videoBGRA), idValue(videoRGBx), idValue(videoRGBA))
		e.Prop(formatVideoMaxRate).Choice(choiceRange, typeFraction,
			fractionValue(MaxFrameRate, 1), fractionValue(1, 1), fractionValue(MaxFrameRate, 1))
	})
	var io builder
	io.Object(objParamIO, paramIO, func() {
		io.Prop(ioParamID).ID(ioBuffers)
		io.Prop(ioParamSize).Int(8)
	})
	params := [][]byte{e.bytes(), io.bytes()}
	formatFlags := uint32(paramInfoWrite)
	s.mu.Lock()
	if s.formatPod != nil {
		params = append(params, s.formatPod)
		formatFlags |= paramInfoRead
	}
	s.mu.Unlock()

	var w builder
	w.Struct(func() {
		w.Int(0).Int(0) // input port 0
		w.Int(1 | 2)    // change mask: params, info
		w.Int(int32(len(params)))
		for _, p := range params {
			w.Raw(p)
		}
		w.Struct(func() {
			w.Long(1<<0 | 1<<1 | 1<<2 | 1<<3) // flags, rate, props, params changed
			w.Long(0).Int(0).Int(1)           // flags, rate 0/1
			w.Int(1).String("port.name").String("input_0")
			w.Int(3)
			w.ID(paramEnum).Int(paramInfoRead)
			w.ID(paramFormat).Int(int32(formatFlags))
			w.ID(paramIO).Int(paramInfoRead)
		})
	})
	return s.c.send(s.id, cnPortUpdate, w.bytes())
}

// event handles client-node events on the connection's reader goroutine.
func (s *Stream) event(op uint8, f *fields, m *message) {
	var err error
	switch op {
	case cnEventTransport:
		read, write := m.fd(f.Fd()), m.fd(f.Fd())
		memID, offset, size := f.Uint(), f.Uint(), f.Uint()
		if write >= 0 {
			unix.Close(write) // only drivers use it
		}
		if err = f.Err(); err == nil {
			err = s.transport(read, memID, offset, size)
		} else if read >= 0 {
			unix.Close(read)
		}
	case cnEventPortSetParam:
		f.Uint() // direction
		f.Uint() // port
		id := f.ID()
		f.Uint() // flags
		p := f.Pod()
		if err = f.Err(); err == nil && id == paramFormat {
			if err = s.setFormat(p); err == nil {
				err = s.portUpdate()
			}
		}
	case cnEventPortUseBuffers:
		err = s.useBuffers(f)
	case cnEventPortSetIO:
		f.Uint() // direction
		f.Uint() // port
		mixID, id, memID, offset, size := f.Uint(), f.ID(), f.Uint(), f.Uint(), f.Uint()
		if err = f.Err(); err == nil && (id == ioBuffers || id == ioAsync) {
			err = s.setIO(mixID, id, memID, offset, size)
		}
	case cnEventSetActivation:
		node, idx := f.Uint(), f.Fd()
		memID, offset, size := f.Uint(), f.Uint(), f.Uint()
		fd := m.fd(idx)
		if err = f.Err(); err == nil {
			err = s.setActivation(node, fd, memID, offset, size)
		} else if fd >= 0 {
			unix.Close(fd)
		}
	case cnEventPortSetMixInfo:
		f.Uint() // direction
		f.Uint() // port
		mixID, peer := f.Uint(), f.Uint()
		if err = f.Err(); err == nil {
			s.mu.Lock()
			if peer == invalidID {
				s.dropMix(mixID)
			} else {
				s.mix(mixID)
			}
			s.mu.Unlock()
		}
	}
	if err != nil {
		s.fail(fmt.Errorf("PipeWire stream: %w", err))
	}
}

func (s *Stream) linkEvent(op uint8, f *fields, _ *message) {
	if op != linkEventInfo {
		return
	}
	for range 6 { // ids, change mask
		f.Pod()
	}
	state, msg := f.Int(), f.String()
	if f.Err() == nil && state == linkStateError {
		s.fail(fmt.Errorf("PipeWire couldn't link to the screen cast: %s", msg))
	}
}

// transport sets up our activation record and the eventfd we're woken on.
func (s *Stream) transport(read int, memID, offset, size uint32) error {
	if read < 0 {
		return errors.New("transport without an eventfd")
	}
	act, blk, err := s.c.mapMem(memID, offset, size)
	if err == nil && len(act) < actMinSize {
		err = fmt.Errorf("activation record of %d bytes", len(act))
	}
	if err == nil {
		err = unix.SetNonblock(read, true)
	}
	if err != nil {
		s.c.unref(blk)
		unix.Close(read)
		return err
	}
	// Version 0 is the original activation protocol, which every server still
	// speaks: the server marks us ready and we only answer wakeups.
	atomic.StoreUint32(word(act, actClientVersion), 0)

	s.stopWake()
	wake, done := os.NewFile(uintptr(read), "pipewire-eventfd"), make(chan struct{})
	s.mu.Lock()
	if s.err != nil { // closed meanwhile
		s.mu.Unlock()
		s.c.unref(blk)
		wake.Close()
		return nil
	}
	s.c.unref(s.actBlk)
	s.act, s.actBlk, s.wake, s.wakeDone = act, blk, wake, done
	s.signal()
	s.mu.Unlock()
	go s.run(wake, done)
	return nil
}

func (s *Stream) stopWake() {
	s.mu.Lock()
	wake, done := s.wake, s.wakeDone
	s.wake, s.wakeDone = nil, nil
	s.mu.Unlock()
	if wake != nil {
		wake.Close()
		<-done
	}
}

func (s *Stream) setFormat(p pod) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.mixes {
		s.clearBuffers(m)
	}
	s.formatPod = nil
	if p.typ != typeObject {
		return nil // format cleared
	}
	_, _, props, err := p.object()
	if err != nil {
		return err
	}
	format, _ := props[formatVideoFormat].uint32Of(typeID)
	w, h, _ := props[formatVideoSize].pair(typeRectangle)
	num, den, _ := props[formatVideoMaxRate].pair(typeFraction)
	switch format {
	case videoBGRx, videoBGRA, videoRGBx, videoRGBA:
	default:
		return fmt.Errorf("unsupported video format %d", format)
	}
	if w == 0 || h == 0 || w > 1<<15 || h > 1<<15 {
		return fmt.Errorf("invalid video size %dx%d", w, h)
	}
	interval := time.Second / MaxFrameRate
	if num > 0 && den > 0 {
		interval = time.Duration(den) * time.Second / time.Duration(num)
	}
	s.format = videoFormat{format: format, width: int(w), height: int(h), interval: interval}
	s.formatPod = p.raw()
	return nil
}

// useBuffers maps the buffers the producer will fill for one link.
func (s *Stream) useBuffers(f *fields) error {
	f.Uint() // direction
	f.Uint() // port
	mixID, _, n := f.Uint(), f.Uint(), f.Uint()
	if n > 64 {
		return fmt.Errorf("%d buffers", n)
	}
	bufs, blks, err := s.mapBuffers(f, n)
	if err != nil {
		s.c.unref(blks...)
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.mix(mixID)
	s.clearBuffers(m)
	m.buffers, m.bufBlks = bufs, blks
	return nil
}

// mapBuffers reads n buffer descriptions and maps their memory. It returns
// the mappings it made even on error, for the caller to release.
func (s *Stream) mapBuffers(f *fields, n uint32) ([]buffer, []*memBlock, error) {
	var blks []*memBlock
	bufs := make([]buffer, 0, n)
	for range n {
		memID, offset, size, nMetas := f.Uint(), f.Uint(), f.Uint(), f.Uint()
		metas := uint32(0)
		for range nMetas {
			f.ID()
			metas += (f.Uint() + 7) &^ 7
		}
		nDatas := f.Uint()
		if err := f.Err(); err != nil {
			return nil, blks, err
		}
		region, blk, err := s.c.mapMem(memID, offset, size)
		if err != nil {
			return nil, blks, err
		}
		blks = append(blks, blk)
		var b buffer
		for j := range nDatas {
			typ, dataID, _, mapOffset, maxSize := f.ID(), f.Uint(), f.Uint(), f.Uint(), f.Uint()
			if err := f.Err(); err != nil {
				return nil, blks, err
			}
			if j > 0 {
				continue // packed RGB has one plane
			}
			chunk := uint64(metas) + 16*uint64(j)
			if chunk+16 > uint64(len(region)) {
				return nil, blks, errors.New("buffer chunk outside its memory")
			}
			b.chunk = region[chunk : chunk+16]
			switch typ {
			case dataMemID:
				b.data, blk, err = s.c.mapMem(dataID, mapOffset, maxSize)
				blks = append(blks, blk)
			case dataMemPtr:
				if uint64(dataID)+uint64(maxSize) > uint64(len(region)) {
					err = errors.New("buffer data outside its memory")
				} else {
					b.data = region[dataID : dataID+maxSize]
				}
			default:
				err = fmt.Errorf("buffer data type %d", typ)
			}
			if err != nil {
				return nil, blks, err
			}
		}
		if len(b.data) == 0 {
			return nil, blks, errors.New("buffer without data")
		}
		bufs = append(bufs, b)
	}
	return bufs, blks, nil
}

func (s *Stream) setIO(mixID, id, memID, offset, size uint32) error {
	var ios [][]byte
	var blk *memBlock
	if memID != invalidID {
		need := uint32(8)
		if id == ioAsync {
			need = 16
		}
		if size < need {
			return fmt.Errorf("io area of %d bytes", size)
		}
		var area []byte
		var err error
		if area, blk, err = s.c.mapMem(memID, offset, size); err != nil {
			return err
		}
		ios = [][]byte{area[:8]}
		if id == ioAsync {
			ios = append(ios, area[8:16])
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.mix(mixID)
	s.c.unref(m.ioBlk)
	m.ios, m.ioBlk = ios, blk
	return nil
}

func (s *Stream) setActivation(node uint32, fd int, memID, offset, size uint32) error {
	s.mu.Lock()
	s.dropTarget(node)
	s.mu.Unlock()
	if memID == invalidID {
		if fd >= 0 {
			unix.Close(fd)
		}
		return nil
	}
	act, blk, err := s.c.mapMem(memID, offset, size)
	if err == nil && len(act) < actMinSize {
		err = fmt.Errorf("activation record of %d bytes", len(act))
	}
	if err == nil && fd < 0 {
		err = errors.New("activation without an eventfd")
	}
	if err != nil {
		s.c.unref(blk)
		if fd >= 0 {
			unix.Close(fd)
		}
		return err
	}
	s.mu.Lock()
	s.targets[node] = &target{fd: fd, act: act, blk: blk}
	s.mu.Unlock()
	return nil
}

// mix returns the link with id, adding it if new. Callers hold s.mu.
func (s *Stream) mix(id uint32) *mix {
	m := s.mixes[id]
	if m == nil {
		m = &mix{}
		s.mixes[id] = m
	}
	return m
}

// dropMix forgets a link to our port. Callers hold s.mu.
func (s *Stream) dropMix(id uint32) {
	m := s.mixes[id]
	if m == nil {
		return
	}
	s.clearBuffers(m)
	s.c.unref(m.ioBlk)
	delete(s.mixes, id)
}

// clearBuffers forgets a link's buffers, as a new format or buffer set
// invalidates them. Callers hold s.mu.
func (s *Stream) clearBuffers(m *mix) {
	if s.held != nil && s.held.mix == m {
		s.held = nil
	}
	s.c.unref(m.bufBlks...)
	m.buffers, m.bufBlks = nil, nil
}

// dropTarget stops signaling a node. Callers hold s.mu.
func (s *Stream) dropTarget(id uint32) {
	if t := s.targets[id]; t != nil {
		unix.Close(t.fd)
		s.c.unref(t.blk)
		delete(s.targets, id)
	}
}

// run answers the graph's wakeups until the eventfd is closed.
func (s *Stream) run(wake *os.File, done chan struct{}) {
	defer close(done)
	var b [8]byte
	for {
		if _, err := wake.Read(b[:]); err != nil {
			return
		}
		s.process()
	}
}

// process is one graph cycle, mirroring libpipewire's process_node: claim
// the cycle, take the new frame, mark ourselves finished and signal the
// nodes waiting on us (the driver).
func (s *Stream) process() {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.act
	if a == nil || !atomic.CompareAndSwapUint32(word(a, actStatus), actTriggered, actAwake) {
		return
	}
	atomic.StoreUint64(dword(a, actAwakeTime), now())
	for _, m := range s.mixes {
		for _, io := range m.ios {
			s.take(m, io)
		}
	}
	atomic.StoreUint32(word(a, actState0Status), statusNeedData)
	done := now()
	if !atomic.CompareAndSwapUint32(word(a, actStatus), actAwake, actFinished) {
		return
	}
	atomic.StoreUint64(dword(a, actFinishTime), done)
	for _, t := range s.targets {
		t.trigger(done)
	}
}

// take claims the buffer the producer put in io, hands back the frame we
// held before, and asks for more. Callers hold s.mu.
func (s *Stream) take(m *mix, io []byte) {
	if int32(atomic.LoadUint32(word(io, 0))) != statusHaveData {
		return
	}
	id := atomic.LoadUint32(word(io, 4))
	back := id
	if int(id) < len(m.buffers) && m.buffers[id].hasFrame() {
		back = invalidID
		if s.held != nil && s.held.mix == m {
			back = s.held.id
		}
		s.newFrame(&frameRef{mix: m, id: id})
	}
	atomic.StoreUint32(word(io, 4), back)
	atomic.StoreUint32(word(io, 0), statusNeedData)
}

// newFrame makes ref the held frame and wakes Frame. Callers hold s.mu.
func (s *Stream) newFrame(ref *frameRef) {
	s.held = ref
	s.last = time.Now()
	s.signal()
}

// hasFrame reports whether the producer filled the buffer; Mutter queues
// empty ones when only metadata changed.
func (b buffer) hasFrame() bool {
	return le.Uint32(b.chunk[4:]) > 0 && le.Uint32(b.chunk[12:])&chunkCorrupted == 0
}

// trigger counts down a node's pending inputs and wakes it at zero
// (trigger_target_v0/v1 in pipewire/private.h).
func (t *target) trigger(nsec uint64) {
	a := t.act
	if atomic.AddInt32((*int32)(unsafe.Pointer(&a[actState0Pending])), -1) != 0 {
		return
	}
	if atomic.LoadUint32(word(a, actServerVersion)) >= 1 {
		if !atomic.CompareAndSwapUint32(word(a, actStatus), actNotTriggered, actTriggered) {
			return
		}
	} else {
		atomic.StoreUint32(word(a, actStatus), actTriggered)
	}
	atomic.StoreUint64(dword(a, actSignalTime), nsec)
	one := [8]byte{1}
	unix.Write(t.fd, one[:])
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func word(b []byte, off int) *uint32  { return (*uint32)(unsafe.Pointer(&b[off])) }
func dword(b []byte, off int) *uint64 { return (*uint64)(unsafe.Pointer(&b[off])) }

// now is CLOCK_MONOTONIC in nanoseconds, the graph's clock.
func now() uint64 {
	var ts unix.Timespec
	unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return uint64(ts.Nano())
}
