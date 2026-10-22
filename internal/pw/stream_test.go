package pw

import (
	"context"
	"os"
	"testing"
	"time"
)

func put32(b []byte, off int, v uint32) { le.PutUint32(b[off:], v) }
func get32(b []byte, off int) uint32    { return le.Uint32(b[off:]) }

// testBuffer is a 2x1 frame with a padded stride.
func testBuffer(pixels ...byte) buffer {
	chunk := make([]byte, 16)
	put32(chunk, 4, 12) // size
	put32(chunk, 8, 12) // stride
	return buffer{chunk: chunk, data: append(pixels, make([]byte, 12-len(pixels))...)}
}

func TestProcessHoldsNewestFrameAndSignalsDriver(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	act, driver, io := make([]byte, actMinSize), make([]byte, actMinSize), make([]byte, 8)
	put32(driver, actServerVersion, 1)
	m := &mix{ios: [][]byte{io}, buffers: []buffer{testBuffer(1, 2, 3, 0), testBuffer(4, 5, 6, 0)}}
	s := &Stream{
		act:     act,
		mixes:   map[uint32]*mix{0: m},
		targets: map[uint32]*target{9: {fd: int(w.Fd()), act: driver}},
		fresh:   make(chan struct{}),
	}

	cycle := func(buffer uint32) {
		t.Helper()
		put32(act, actStatus, actTriggered)
		put32(driver, actStatus, actNotTriggered)
		put32(driver, actState0Pending, 1)
		put32(io, 0, statusHaveData)
		put32(io, 4, buffer)
		s.process()
		if got := get32(act, actStatus); got != actFinished {
			t.Fatalf("status = %d, want finished", got)
		}
		if got := get32(driver, actStatus); got != actTriggered {
			t.Fatalf("driver status = %d, want triggered", got)
		}
		var b [8]byte
		if n, err := r.Read(b[:]); n != 8 || err != nil || b[0] != 1 {
			t.Fatalf("driver eventfd got %v %v", b, err)
		}
	}

	cycle(1)
	if s.held == nil || s.held.id != 1 || get32(io, 4) != invalidID || get32(io, 0) != statusNeedData {
		t.Fatalf("first frame: held %+v, io returns %d", s.held, int32(get32(io, 4)))
	}
	cycle(0)
	if s.held.id != 0 || get32(io, 4) != 1 {
		t.Fatalf("second frame: held %d, io returns %d, want the first buffer back", s.held.id, get32(io, 4))
	}

	put32(act, actStatus, actFinished) // a wakeup that isn't ours to run
	s.process()
	if s.frames != 2 {
		t.Errorf("frames = %d after a stray wakeup", s.frames)
	}

	s.format = videoFormat{format: videoBGRx, width: 2, height: 1}
	img, err := s.copyFrame(m.buffers[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{3, 2, 1, 255, 0, 0, 0, 255}; string(img.Pix) != string(want) {
		t.Errorf("BGRx converted to %v, want %v", img.Pix, want)
	}
}

func TestFrameSettles(t *testing.T) {
	m := &mix{buffers: []buffer{testBuffer(1, 2, 3, 0), testBuffer(4, 5, 6, 0)}}
	s := &Stream{fresh: make(chan struct{}), format: videoFormat{format: videoBGRx, width: 2, height: 1, interval: 10 * time.Millisecond}}
	s.newFrame(&frameRef{mix: m, id: 0})
	s.last = time.Now().Add(-time.Second)
	ctx := context.Background()

	start := time.Now()
	if _, err := s.Frame(ctx, time.Time{}); err != nil || time.Since(start) > 50*time.Millisecond {
		t.Errorf("idle screen took %v (%v)", time.Since(start), err)
	}

	start = time.Now()
	if _, err := s.Frame(ctx, time.Now()); err != nil || time.Since(start) < settleIdle-20*time.Millisecond {
		t.Errorf("input that changed nothing returned after %v (%v), want about %v", time.Since(start), err, settleIdle)
	}

	input := time.Now()
	go func() {
		time.Sleep(30 * time.Millisecond)
		s.mu.Lock()
		s.newFrame(&frameRef{mix: m, id: 1})
		s.mu.Unlock()
	}()
	img, err := s.Frame(ctx, input)
	if err != nil || img.Pix[0] != 6 {
		t.Errorf("didn't wait for the frame after input: %v %v", img, err)
	}
}
