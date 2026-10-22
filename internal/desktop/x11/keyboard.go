package x11

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/jezek/xgb/xproto"

	"github.com/StivenKN/linux-use/internal/keys"
)

// keyboard mirrors the server's core keyboard mapping. Keysyms the layout
// cannot produce are bound to spare keycodes (ones with no keysyms), the
// way xdotool does it. Bindings stay until Close so typing long non-ASCII
// text reuses them instead of remapping on every character.
type keyboard struct {
	min   xproto.Keycode
	per   int             // keysyms per keycode
	syms  []xproto.Keysym // per entries for each keycode from min
	stale atomic.Bool     // set on MappingNotify: reload before the next lookup

	bound map[xproto.Keycode]binding
	held  map[keys.Keysym]xproto.Keycode // pressed by Key, not yet released
}

type binding struct {
	sym  keys.Keysym
	used time.Time
}

// rebindDelay is how long a borrowed keycode keeps its keysym after its last
// press. Xlib clients resolve keysyms lazily, so changing the mapping before
// they handle the press can turn it into the new character.
const rebindDelay = 100 * time.Millisecond

func (b *Backend) loadKeymap() error {
	k := &b.kbd
	k.stale.Store(false)
	lo, hi := b.setup.MinKeycode, b.setup.MaxKeycode
	r, err := xproto.GetKeyboardMapping(b.c, lo, byte(hi-lo+1)).Reply()
	if err != nil {
		return err
	}
	if r.KeysymsPerKeycode == 0 {
		return errors.New("X server reports an empty keyboard mapping")
	}
	k.min, k.per, k.syms = lo, int(r.KeysymsPerKeycode), r.Keysyms
	for code, bd := range k.bound {
		if k.at(code)[0] != xproto.Keysym(bd.sym) {
			delete(k.bound, code) // someone remapped it, e.g. setxkbmap
		}
	}
	return nil
}

// at returns the keysyms of a keycode, one per shift level and group.
func (k *keyboard) at(code xproto.Keycode) []xproto.Keysym {
	i := int(code-k.min) * k.per
	return k.syms[i : i+k.per]
}

// lookup finds the keycode producing sym in the first group, preferring
// the unshifted level.
func (k *keyboard) lookup(sym keys.Keysym) (code xproto.Keycode, shift, ok bool) {
	n := len(k.syms) / k.per
	for level := range min(2, k.per) {
		for i := range n {
			if k.syms[i*k.per+level] == xproto.Keysym(sym) {
				return k.min + xproto.Keycode(i), level == 1, true
			}
		}
	}
	return 0, false, false
}

// spare picks a keycode to bind a new keysym to: an empty one if any is
// left, else the binding used longest ago. Held keys are never taken.
func (k *keyboard) spare() (xproto.Keycode, bool) {
	held := map[xproto.Keycode]bool{}
	for _, c := range k.held {
		held[c] = true
	}
	var best xproto.Keycode
	var oldest time.Time
	found := false
	// From the top: low keycodes like 8 are the ones toolkits mishandle.
	for i := len(k.syms)/k.per - 1; i >= 0; i-- {
		code := k.min + xproto.Keycode(i)
		bd, ours := k.bound[code]
		empty := !slices.ContainsFunc(k.at(code), func(s xproto.Keysym) bool { return s != 0 })
		if !ours && !empty || held[code] {
			continue
		}
		if !found || bd.used.Before(oldest) {
			best, oldest, found = code, bd.used, true
		}
	}
	return best, found
}

// stroke resolves sym to a keycode, binding a spare one when the layout
// lacks it. shift reports that the keysym sits on the shifted level.
func (b *Backend) stroke(sym keys.Keysym) (code xproto.Keycode, shift bool, err error) {
	k := &b.kbd
	if k.stale.Load() {
		if err := b.loadKeymap(); err != nil {
			return 0, false, err
		}
	}
	code, shift, ok := k.lookup(sym)
	if !ok {
		if code, err = b.bind(sym); err != nil {
			return 0, false, err
		}
	}
	if _, ours := k.bound[code]; ours {
		k.bound[code] = binding{sym, time.Now()}
	}
	return code, shift, nil
}

// bind maps sym onto a spare keycode.
func (b *Backend) bind(sym keys.Keysym) (xproto.Keycode, error) {
	k := &b.kbd
	code, ok := k.spare()
	if !ok {
		return 0, fmt.Errorf("no spare keycode to bind keysym %#x to", uint32(sym))
	}
	if bd, ours := k.bound[code]; ours {
		time.Sleep(time.Until(bd.used.Add(rebindDelay)))
	}
	// The same keysym on both levels, so a held Shift doesn't matter.
	syms := []xproto.Keysym{xproto.Keysym(sym), xproto.Keysym(sym)}
	if err := xproto.ChangeKeyboardMappingChecked(b.c, 1, code, 2, syms).Check(); err != nil {
		return 0, fmt.Errorf("bind keysym %#x: %w", uint32(sym), err)
	}
	clear(k.at(code))
	copy(k.at(code), syms)
	k.bound[code] = binding{sym: sym}
	return code, nil
}

// restoreSpares unbinds the keycodes we borrowed, once clients have had
// time to handle their last presses.
func (b *Backend) restoreSpares() {
	k := &b.kbd
	for code, bd := range k.bound {
		time.Sleep(time.Until(bd.used.Add(rebindDelay)))
		xproto.ChangeKeyboardMapping(b.c, 1, code, 1, []xproto.Keysym{0})
		delete(k.bound, code)
	}
}

// shiftCode returns the keycode of Shift_L, or 0 if the layout has none.
func (b *Backend) shiftCode() xproto.Keycode {
	code, _, _ := b.kbd.lookup(keys.ShiftL)
	return code
}

// shiftHeld reports whether the caller is holding a Shift key through Key.
func (b *Backend) shiftHeld() bool {
	for sym := range b.kbd.held {
		if keys.ModMask(sym)&1 != 0 {
			return true
		}
	}
	return false
}

// press presses code, holding Shift around the press when shift is set and
// the caller isn't holding it already. Shift only matters for the press,
// which picks the keysym; letting go right away keeps it out of later keys.
func (b *Backend) press(code xproto.Keycode, shift bool) {
	sc := b.shiftCode()
	shift = shift && sc != 0 && !b.shiftHeld()
	if shift {
		b.fake(xproto.KeyPress, byte(sc), 0, 0)
	}
	b.fake(xproto.KeyPress, byte(code), 0, 0)
	if shift {
		b.fake(xproto.KeyRelease, byte(sc), 0, 0)
	}
}

func (b *Backend) Key(ctx context.Context, sym keys.Keysym, down bool) error {
	k := &b.kbd
	if code, ok := k.held[sym]; ok && !down {
		delete(k.held, sym)
		b.fake(xproto.KeyRelease, byte(code), 0, 0)
		return b.flush()
	}
	code, shift, err := b.stroke(sym)
	if err != nil {
		return err
	}
	if down {
		b.press(code, shift)
		k.held[sym] = code
	} else {
		b.fake(xproto.KeyRelease, byte(code), 0, 0)
	}
	return b.flush()
}

// Type taps each character's key in turn. Characters the layout lacks go
// through borrowed keycodes, so any Unicode text works on any layout.
func (b *Backend) Type(ctx context.Context, text string) error {
	for _, r := range text {
		if err := ctx.Err(); err != nil {
			b.flush()
			return err
		}
		code, shift, err := b.stroke(keys.FromRune(r))
		if err != nil {
			b.flush()
			return err
		}
		b.press(code, shift)
		b.fake(xproto.KeyRelease, byte(code), 0, 0)
	}
	return b.flush()
}
