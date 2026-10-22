package gnome

import (
	"context"
	"fmt"
	"image"
	"slices"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/StivenKN/linux-use/internal/desktop"
	"github.com/StivenKN/linux-use/internal/pw"
)

// capturePipeWire reads the newest frame of the monitor's ScreenCast stream.
// The input session already records every monitor, so this only links a
// PipeWire consumer to the stream the first time a monitor is captured.
func (b *Backend) capturePipeWire(ctx context.Context, d desktop.Display) (*image.RGBA, error) {
	r, err := b.remote(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(r.displays, func(o desktop.Display) bool { return o.Name == d.Name })
	if i < 0 {
		return nil, fmt.Errorf("display %q is not being recorded", d.Name)
	}
	s, err := b.capture(ctx, r, i)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	input := b.input
	b.mu.Unlock()
	img, err := s.Frame(ctx, input)
	if err != nil {
		s.Close()
		b.mu.Lock()
		r.caps[i] = nil
		b.mu.Unlock()
	}
	return img, err
}

// capture returns the PipeWire consumer of r's i-th stream, starting it if needed.
func (b *Backend) capture(ctx context.Context, r *remote, i int) (*pw.Stream, error) {
	b.mu.Lock()
	s := r.caps[i]
	b.mu.Unlock()
	if s != nil && s.Err() == nil {
		return s, nil
	}
	if s != nil {
		s.Close()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	node, err := b.node(ctx, r.streams[i])
	if err != nil {
		return nil, err
	}
	c, err := b.pipewire(ctx)
	if err != nil {
		return nil, err
	}
	if s, err = c.Capture(ctx, node); err != nil {
		return nil, err
	}
	b.mu.Lock()
	r.caps[i] = s
	b.mu.Unlock()
	return s, nil
}

// pipewire returns the daemon connection, redialing if it died.
func (b *Backend) pipewire(ctx context.Context) (*pw.Conn, error) {
	if b.pw != nil && b.pw.Err() == nil {
		return b.pw, nil
	}
	if b.pw != nil {
		b.pw.Close()
		b.pw = nil
	}
	c, err := pw.Dial(ctx)
	if err != nil {
		return nil, err
	}
	b.pw = c
	return c, nil
}

// node waits for Mutter to announce the PipeWire node of a ScreenCast stream.
func (b *Backend) node(ctx context.Context, stream dbus.ObjectPath) (uint32, error) {
	for {
		b.mu.Lock()
		id, ok := b.nodes[stream]
		sig := b.nodeSig
		b.mu.Unlock()
		if ok {
			return id, nil
		}
		select {
		case <-sig:
		case <-ctx.Done():
			return 0, fmt.Errorf("GNOME didn't start the screen cast stream: %w", ctx.Err())
		}
	}
}

// streamAdded records the node of a stream from its PipeWireStreamAdded signal.
func (b *Backend) streamAdded(sig *dbus.Signal) {
	if len(sig.Body) < 1 {
		return
	}
	id, ok := sig.Body[0].(uint32)
	if !ok {
		return
	}
	b.mu.Lock()
	b.nodes[sig.Path] = id
	close(b.nodeSig)
	b.nodeSig = make(chan struct{})
	b.mu.Unlock()
}
