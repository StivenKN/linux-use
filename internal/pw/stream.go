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

// Stream receives one video node's frames through a client-node whose input
// port is linked to the producer. It holds on to the newest frame's buffer
// until the next one arrives, so pixels are only copied when asked for.
type Stream struct {
	c    *Conn
	id   uint32 // client-node proxy
	link uint32 // link proxy, 0 until created

	mu        sync.Mutex
	format    videoFormat
	formatPod []byte             // the negotiated Format, echoed in port updates
	mixes     map[uint32]*mix    // one per link to our input port
	act       []byte             // our pw_node_activation
	targets   map[uint32]*target // nodes to signal when we finish a cycle
	wake      *os.File           // eventfd the graph signals us on
	wakeDone  chan struct{}      // closed when the wake goroutine exits
	held      *frameRef          // newest frame, kept out of the producer's hands
	frames    uint64
	last      time.Time
	fresh     chan struct{} // closed and replaced on every new frame
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
	buffers []buffer
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
}

// Capture links a new client-node to the output of the given node (a global
// id, as Mutter's PipeWireStreamAdded reports it) and waits for its first frame.
func (c *Conn) Capture(ctx context.Context, node uint32) (*Stream, error) {
	s := &Stream{
		c:       c,
		mixes:   map[uint32]*mix{},
		targets: map[uint32]*target{},
		fresh:   make(chan struct{}),
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
	var a builder
	if err := s.c.send(s.id, cnSetActive, a.Struct(func() { a.Bool(true) }).bytes()); err != nil {
		return err
	}
	global, err := s.c.globalID(ctx, s.id)
	if err != nil {
		return err
	}
	if err := s.Err(); err != nil {
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
	_, err = s.Frame(ctx, time.Time{})
	return err
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
		for id, t := range s.targets {
			unix.Close(t.fd)
			delete(s.targets, id)
		}
		s.mixes, s.held, s.act = map[uint32]*mix{}, nil, nil
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
		close(s.fresh)
		s.fresh = make(chan struct{})
	}
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
// that may still be due.
func (s *Stream) Frame(ctx context.Context, input time.Time) (*image.RGBA, error) {
	var deadline, idle time.Time
	for {
		s.mu.Lock()
		n, last, fresh, err := s.frames, s.last, s.fresh, s.err
		followUp := s.format.interval + 10*time.Millisecond
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		until := time.Now().Add(time.Hour) // the first frame; ctx bounds it
		if n > 0 {
			quiet := followUp
			if deadline.IsZero() {
				deadline, idle = time.Now().Add(followUp), time.Time{}
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
			break
		}
		t := time.NewTimer(wait)
		select {
		case <-fresh:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			if n == 0 {
				return nil, fmt.Errorf("no frame from PipeWire: %w", ctx.Err())
			}
			return nil, ctx.Err()
		}
		t.Stop()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	if s.held == nil {
		return nil, errors.New("PipeWire stream has no frame")
	}
	return s.copyFrame(s.held.mix.buffers[s.held.id])
}

// copyFrame converts a buffer to RGBA. Callers hold s.mu.
func (s *Stream) copyFrame(b buffer) (*image.RGBA, error) {
	f := s.format
	offset, size, stride := le.Uint32(b.chunk), le.Uint32(b.chunk[4:]), int(int32(le.Uint32(b.chunk[8:])))
	if stride == 0 {
		stride = f.width * 4
	}
	offset %= uint32(len(b.data))
	if stride < f.width*4 || uint64(offset)+uint64(stride)*uint64(f.height-1)+uint64(f.width*4) > uint64(len(b.data)) ||
		uint64(size) < uint64(stride)*uint64(f.height-1)+uint64(f.width*4) {
		return nil, fmt.Errorf("PipeWire frame of %d bytes (stride %d) doesn't hold %dx%d pixels", size, stride, f.width, f.height)
	}
	img := image.NewRGBA(image.Rect(0, 0, f.width, f.height))
	src := b.data[offset:]
	for y := range f.height {
		row, out := src[y*stride:y*stride+f.width*4], img.Pix[y*img.Stride:]
		switch f.format {
		case videoBGRx, videoBGRA:
			for x := 0; x < len(row); x += 4 {
				out[x], out[x+1], out[x+2], out[x+3] = row[x+2], row[x+1], row[x], 0xff
			}
		default: // RGBx, RGBA
			for x := 0; x < len(row); x += 4 {
				out[x], out[x+1], out[x+2], out[x+3] = row[x], row[x+1], row[x+2], 0xff
			}
		}
	}
	return img, nil
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
			} else if s.mixes[mixID] == nil {
				s.mixes[mixID] = &mix{}
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
	act, err := s.c.mapMem(memID, offset, size)
	if err == nil && len(act) < actMinSize {
		err = fmt.Errorf("activation record of %d bytes", len(act))
	}
	if err == nil {
		err = unix.SetNonblock(read, true)
	}
	if err != nil {
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
		wake.Close()
		return nil
	}
	s.act, s.wake, s.wakeDone = act, wake, done
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
	s.clearBuffers()
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
			return err
		}
		region, err := s.c.mapMem(memID, offset, size)
		if err != nil {
			return err
		}
		var b buffer
		for j := range nDatas {
			typ, dataID, _, mapOffset, maxSize := f.ID(), f.Uint(), f.Uint(), f.Uint(), f.Uint()
			if err := f.Err(); err != nil {
				return err
			}
			if j > 0 {
				continue // packed RGB has one plane
			}
			chunk := uint64(metas) + 16*uint64(j)
			if chunk+16 > uint64(len(region)) {
				return errors.New("buffer chunk outside its memory")
			}
			b.chunk = region[chunk : chunk+16]
			switch typ {
			case dataMemID:
				b.data, err = s.c.mapMem(dataID, mapOffset, maxSize)
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
				return err
			}
		}
		if len(b.data) == 0 {
			return errors.New("buffer without data")
		}
		bufs = append(bufs, b)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.mixes[mixID]
	if m == nil {
		m = &mix{}
		s.mixes[mixID] = m
	}
	if s.held != nil && s.held.mix == m {
		s.held = nil
	}
	m.buffers = bufs
	return nil
}

func (s *Stream) setIO(mixID, id, memID, offset, size uint32) error {
	var ios [][]byte
	if memID != invalidID {
		need := uint32(8)
		if id == ioAsync {
			need = 16
		}
		if size < need {
			return fmt.Errorf("io area of %d bytes", size)
		}
		area, err := s.c.mapMem(memID, offset, size)
		if err != nil {
			return err
		}
		ios = [][]byte{area[:8]}
		if id == ioAsync {
			ios = append(ios, area[8:16])
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.mixes[mixID]
	if m == nil {
		m = &mix{}
		s.mixes[mixID] = m
	}
	m.ios = ios
	return nil
}

func (s *Stream) setActivation(node uint32, fd int, memID, offset, size uint32) error {
	s.mu.Lock()
	if t := s.targets[node]; t != nil {
		unix.Close(t.fd)
		delete(s.targets, node)
	}
	s.mu.Unlock()
	if memID == invalidID {
		if fd >= 0 {
			unix.Close(fd)
		}
		return nil
	}
	act, err := s.c.mapMem(memID, offset, size)
	if err == nil && len(act) < actMinSize {
		err = fmt.Errorf("activation record of %d bytes", len(act))
	}
	if err == nil && fd < 0 {
		err = errors.New("activation without an eventfd")
	}
	if err != nil {
		if fd >= 0 {
			unix.Close(fd)
		}
		return err
	}
	s.mu.Lock()
	s.targets[node] = &target{fd: fd, act: act}
	s.mu.Unlock()
	return nil
}

// dropMix forgets a link to our port. Callers hold s.mu.
func (s *Stream) dropMix(id uint32) {
	if m := s.mixes[id]; m != nil && s.held != nil && s.held.mix == m {
		s.held = nil
	}
	delete(s.mixes, id)
}

// clearBuffers forgets every buffer, as a new format invalidates them.
// Callers hold s.mu.
func (s *Stream) clearBuffers() {
	for _, m := range s.mixes {
		m.buffers = nil
	}
	s.held = nil
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
	s.frames++
	s.last = time.Now()
	close(s.fresh)
	s.fresh = make(chan struct{})
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
