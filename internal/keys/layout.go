package keys

import (
	"fmt"
	"sort"
	"strings"
)

// Stroke is how to produce a keysym on the US layout: an evdev keycode,
// optionally with Shift held.
type Stroke struct {
	Code  uint32
	Shift bool
}

// usKeys lists evdev keycode -> [base, shifted] keysyms for a US pc105 keyboard.
var usKeys = map[uint32][2]Keysym{
	1: {Escape}, 14: {BackSpace}, 15: {Tab}, 28: {Return}, 57: {' '},
	2: {'1', '!'}, 3: {'2', '@'}, 4: {'3', '#'}, 5: {'4', '$'}, 6: {'5', '%'}, 7: {'6', '^'},
	8: {'7', '&'}, 9: {'8', '*'}, 10: {'9', '('}, 11: {'0', ')'}, 12: {'-', '_'}, 13: {'=', '+'},
	16: {'q', 'Q'}, 17: {'w', 'W'}, 18: {'e', 'E'}, 19: {'r', 'R'}, 20: {'t', 'T'}, 21: {'y', 'Y'},
	22: {'u', 'U'}, 23: {'i', 'I'}, 24: {'o', 'O'}, 25: {'p', 'P'}, 26: {'[', '{'}, 27: {']', '}'},
	30: {'a', 'A'}, 31: {'s', 'S'}, 32: {'d', 'D'}, 33: {'f', 'F'}, 34: {'g', 'G'}, 35: {'h', 'H'},
	36: {'j', 'J'}, 37: {'k', 'K'}, 38: {'l', 'L'}, 39: {';', ':'}, 40: {'\'', '"'}, 41: {'`', '~'},
	43: {'\\', '|'}, 44: {'z', 'Z'}, 45: {'x', 'X'}, 46: {'c', 'C'}, 47: {'v', 'V'}, 48: {'b', 'B'},
	49: {'n', 'N'}, 50: {'m', 'M'}, 51: {',', '<'}, 52: {'.', '>'}, 53: {'/', '?'},

	29: {ControlL}, 97: {ControlR}, 42: {ShiftL}, 54: {ShiftR}, 56: {AltL}, 100: {AltR},
	125: {SuperL}, 126: {SuperR}, 58: {CapsLock}, 69: {NumLock},

	59: {0xffbe}, 60: {0xffbf}, 61: {0xffc0}, 62: {0xffc1}, 63: {0xffc2}, 64: {0xffc3},
	65: {0xffc4}, 66: {0xffc5}, 67: {0xffc6}, 68: {0xffc7}, 87: {0xffc8}, 88: {0xffc9},
	183: {0xffca}, 184: {0xffcb}, 185: {0xffcc}, 186: {0xffcd}, 187: {0xffce}, 188: {0xffcf},
	189: {0xffd0}, 190: {0xffd1}, 191: {0xffd2}, 192: {0xffd3}, 193: {0xffd4}, 194: {0xffd5},

	102: {0xff50}, 103: {0xff52}, 104: {0xff55}, 105: {0xff51}, 106: {0xff53}, 107: {0xff57},
	108: {0xff54}, 109: {0xff56}, 110: {0xff63}, 111: {Delete}, 99: {0xff61}, 119: {0xff13},
	70: {0xff14}, 127: {0xff67},

	71: {0xffb7}, 72: {0xffb8}, 73: {0xffb9}, 74: {0xffad}, 75: {0xffb4}, 76: {0xffb5},
	77: {0xffb6}, 78: {0xffab}, 79: {0xffb1}, 80: {0xffb2}, 81: {0xffb3}, 82: {0xffb0},
	83: {0xffae}, 96: {0xff8d}, 98: {0xffaf}, 55: {0xffaa}, 117: {0xffbd},

	113: {0x1008ff12}, 114: {0x1008ff11}, 115: {0x1008ff13}, 163: {0x1008ff17},
	164: {0x1008ff14}, 165: {0x1008ff16}, 166: {0x1008ff15}, 224: {0x1008ff03}, 225: {0x1008ff02},
}

var usStrokes = func() map[Keysym]Stroke {
	m := map[Keysym]Stroke{}
	for code, syms := range usKeys {
		if syms[0] != 0 {
			if prev, ok := m[syms[0]]; !ok || code < prev.Code {
				m[syms[0]] = Stroke{Code: code}
			}
		}
		if syms[1] != 0 {
			m[syms[1]] = Stroke{Code: code, Shift: true}
		}
	}
	m[0xfe20] = Stroke{Code: 15, Shift: true} // ISO_Left_Tab
	return m
}()

// US returns the stroke that produces sym on a US keyboard.
func US(sym Keysym) (Stroke, bool) {
	s, ok := usStrokes[sym]
	return s, ok
}

// ExtraCodes is the evdev keycode range used to bind keysyms the US layout
// lacks (emoji, accents, CJK) when we control the keymap.
const (
	ExtraFirst uint32 = 200
	ExtraLast  uint32 = 247
)

// Keymap renders an XKB v1 keymap: the US layout plus extra keysyms bound to
// single-level keys at the given evdev codes.
func Keymap(extra map[Keysym]uint32) string {
	type entry struct {
		code uint32
		syms [2]Keysym
	}
	var entries []entry
	for code, syms := range usKeys {
		entries = append(entries, entry{code, syms})
	}
	for sym, code := range extra {
		entries = append(entries, entry{code, [2]Keysym{sym}})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].code < entries[j].code })

	var kc, sy strings.Builder
	for _, e := range entries {
		xkb := e.code + 8
		fmt.Fprintf(&kc, "  <K%d> = %d;\n", xkb, xkb)
		if e.syms[1] != 0 {
			fmt.Fprintf(&sy, "  key <K%d> { [ 0x%x, 0x%x ] };\n", xkb, uint32(e.syms[0]), uint32(e.syms[1]))
		} else {
			fmt.Fprintf(&sy, "  key <K%d> { [ 0x%x ] };\n", xkb, uint32(e.syms[0]))
		}
	}
	mods := func(name string, codes ...uint32) {
		ks := make([]string, len(codes))
		for i, c := range codes {
			ks[i] = fmt.Sprintf("<K%d>", c+8)
		}
		fmt.Fprintf(&sy, "  modifier_map %s { %s };\n", name, strings.Join(ks, ", "))
	}
	mods("Shift", 42, 54)
	mods("Lock", 58)
	mods("Control", 29, 97)
	mods("Mod1", 56, 100)
	mods("Mod2", 69)
	mods("Mod4", 125, 126)

	return "xkb_keymap {\n" +
		"xkb_keycodes \"linux-use\" {\n  minimum = 8;\n  maximum = 255;\n" + kc.String() + "};\n" +
		"xkb_types \"linux-use\" { include \"complete\" };\n" +
		"xkb_compatibility \"linux-use\" { include \"complete\" };\n" +
		"xkb_symbols \"linux-use\" {\n" + sy.String() + "};\n" +
		"};\n"
}
