package kde

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"

	"github.com/StivenKN/linux-use/internal/desktop"
	"github.com/StivenKN/linux-use/internal/wl"
)

// org_kde_plasma_window_management state bits.
const (
	stateActive    = 1 << 0
	stateMinimized = 1 << 1
)

// windows tracks KWin's window list through org_kde_plasma_window_management,
// the protocol Plasma's own task manager uses.
type windows struct {
	c       *wl.Conn
	mgr     uint32
	mu      sync.Mutex
	pending []string        // uuids of announced windows we have no object for yet
	m       map[int]*window // by short id, stable for the window's lifetime
	next    int
}

type window struct {
	obj          uint32 // org_kde_plasma_window object
	title, appID string
	state        uint32
	ready        bool // initial state received
}

// trackWindows binds the window management global; sync fetches the windows.
func trackWindows(c *wl.Conn) (*windows, error) {
	t := &windows{c: c, m: map[int]*window{}}
	var err error
	t.mgr, _, err = c.BindFirst("org_kde_plasma_window_management", 16, func(op uint16, e *wl.Event) {
		if op == 4 { // window_with_uuid(id, uuid)
			e.Uint()
			t.mu.Lock()
			t.pending = append(t.pending, e.Str())
			t.mu.Unlock()
		}
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// sync requests objects for newly announced windows and waits for their
// state. It must run on the caller's goroutine, never in an event handler:
// KWin rejects new object ids that arrive out of allocation order.
func (t *windows) sync(ctx context.Context) error {
	for {
		if err := t.c.Roundtrip(ctx); err != nil {
			return err
		}
		t.mu.Lock()
		uuids := t.pending
		t.pending = nil
		t.mu.Unlock()
		if len(uuids) == 0 {
			return nil
		}
		for _, uuid := range uuids {
			t.request(uuid)
		}
	}
}

func (t *windows) request(uuid string) {
	t.mu.Lock()
	t.next++
	id, w := t.next, &window{}
	t.m[id] = w
	t.mu.Unlock()

	obj := t.c.NewObject(func(op uint16, e *wl.Event) {
		t.mu.Lock()
		defer t.mu.Unlock()
		switch op {
		case 0: // title_changed
			w.title = e.Str()
		case 1: // app_id_changed
			w.appID = e.Str()
		case 2: // state_changed
			w.state = e.Uint()
		case 5: // unmapped
			delete(t.m, id)
			t.c.Request(w.obj, 7).Send() // destroy
		case 6: // initial_state
			w.ready = true
		}
	})
	t.mu.Lock()
	w.obj = obj
	t.mu.Unlock()
	t.c.Request(t.mgr, 2).Uint(obj).String(uuid).Send() // get_window_by_uuid
}

func (t *windows) list() []desktop.Window {
	t.mu.Lock()
	defer t.mu.Unlock()
	ids := make([]int, 0, len(t.m))
	for id, w := range t.m {
		if w.ready {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	out := make([]desktop.Window, 0, len(ids))
	for _, id := range ids {
		w := t.m[id]
		out = append(out, desktop.Window{
			ID: strconv.Itoa(id), Title: w.title, AppID: w.appID,
			Focused: w.state&stateActive != 0, Minimized: w.state&stateMinimized != 0,
		})
	}
	return out
}

// activate unminimizes, raises and focuses a window.
func (t *windows) activate(id string) error {
	n, _ := strconv.Atoi(id)
	t.mu.Lock()
	w, ok := t.m[n]
	var obj uint32
	if ok {
		obj = w.obj
	}
	t.mu.Unlock()
	if !ok {
		return fmt.Errorf("no window with id %q", id)
	}
	return t.c.Request(obj, 0).Uint(stateActive | stateMinimized).Uint(stateActive).Send() // set_state(flags, state)
}
