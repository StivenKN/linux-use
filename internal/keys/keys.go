// Package keys maps human/xdotool-style key names to X keysyms and Linux
// evdev keycodes, and builds XKB keymaps for virtual keyboards.
package keys

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Keysym is an X11/XKB keysym value.
type Keysym uint32

const (
	BackSpace Keysym = 0xff08
	Tab       Keysym = 0xff09
	Return    Keysym = 0xff0d
	Escape    Keysym = 0xff1b
	Delete    Keysym = 0xffff
	ShiftL    Keysym = 0xffe1
	ShiftR    Keysym = 0xffe2
	ControlL  Keysym = 0xffe3
	ControlR  Keysym = 0xffe4
	CapsLock  Keysym = 0xffe5
	MetaL     Keysym = 0xffe7
	AltL      Keysym = 0xffe9
	AltR      Keysym = 0xffea
	SuperL    Keysym = 0xffeb
	SuperR    Keysym = 0xffec
	NumLock   Keysym = 0xff7f
)

// names holds canonical X keysym names plus common aliases, all lowercase.
var names = map[string]Keysym{
	"backspace": BackSpace, "tab": Tab, "return": Return, "enter": Return, "escape": Escape, "esc": Escape,
	"delete": Delete, "del": Delete, "insert": 0xff63, "ins": 0xff63,
	"home": 0xff50, "end": 0xff57, "left": 0xff51, "up": 0xff52, "right": 0xff53, "down": 0xff54,
	"arrowleft": 0xff51, "arrowup": 0xff52, "arrowright": 0xff53, "arrowdown": 0xff54,
	"page_up": 0xff55, "pageup": 0xff55, "pgup": 0xff55, "prior": 0xff55,
	"page_down": 0xff56, "pagedown": 0xff56, "pgdn": 0xff56, "next": 0xff56,
	"print": 0xff61, "printscreen": 0xff61, "prtsc": 0xff61, "pause": 0xff13, "break": 0xff13,
	"scroll_lock": 0xff14, "scrolllock": 0xff14, "menu": 0xff67, "contextmenu": 0xff67, "apps": 0xff67,
	"num_lock": NumLock, "numlock": NumLock, "caps_lock": CapsLock, "capslock": CapsLock,
	"space": 0x20, "spacebar": 0x20,
	"forwarddelete": Delete, "forward_delete": Delete, "padenter": 0xff8d, "numpadenter": 0xff8d,
	"equals": '=', "leftbracket": '[', "rightbracket": ']', "backtick": '`', "quote": '\'',

	"shift": ShiftL, "shift_l": ShiftL, "lshift": ShiftL, "shift_r": ShiftR, "rshift": ShiftR,
	"ctrl": ControlL, "control": ControlL, "control_l": ControlL, "ctrl_l": ControlL, "lctrl": ControlL,
	"control_r": ControlR, "ctrl_r": ControlR, "rctrl": ControlR, "lcontrol": ControlL, "rcontrol": ControlR,
	"alt": AltL, "alt_l": AltL, "lalt": AltL, "option": AltL, "opt": AltL, "alt_r": AltR, "ralt": AltR, "altgr": 0xfe03,
	"meta": SuperL, "meta_l": SuperL, "super": SuperL, "super_l": SuperL, "super_r": SuperR,
	"win": SuperL, "windows": SuperL, "cmd": SuperL, "command": SuperL, "logo": SuperL,

	"kp_enter": 0xff8d, "kp_add": 0xffab, "kp_subtract": 0xffad, "kp_multiply": 0xffaa,
	"kp_divide": 0xffaf, "kp_decimal": 0xffae, "kp_equal": 0xffbd,

	"xf86audioraisevolume": 0x1008ff13, "volumeup": 0x1008ff13, "audiovolumeup": 0x1008ff13,
	"xf86audiolowervolume": 0x1008ff11, "volumedown": 0x1008ff11, "audiovolumedown": 0x1008ff11,
	"xf86audiomute": 0x1008ff12, "volumemute": 0x1008ff12, "audiovolumemute": 0x1008ff12, "mute": 0x1008ff12,
	"xf86audioplay": 0x1008ff14, "mediaplaypause": 0x1008ff14, "playpause": 0x1008ff14,
	"xf86audiostop": 0x1008ff15, "mediastop": 0x1008ff15,
	"xf86audioprev": 0x1008ff16, "mediaprevious": 0x1008ff16, "mediatrackprevious": 0x1008ff16,
	"xf86audionext": 0x1008ff17, "medianext": 0x1008ff17, "mediatracknext": 0x1008ff17,
	"xf86monbrightnessup": 0x1008ff02, "brightnessup": 0x1008ff02,
	"xf86monbrightnessdown": 0x1008ff03, "brightnessdown": 0x1008ff03,

	// Punctuation keysym names (they equal their Latin-1 code points).
	"exclam": '!', "quotedbl": '"', "numbersign": '#', "dollar": '$', "percent": '%', "ampersand": '&',
	"apostrophe": '\'', "quoteright": '\'', "parenleft": '(', "parenright": ')', "asterisk": '*',
	"plus": '+', "comma": ',', "minus": '-', "period": '.', "slash": '/', "colon": ':', "semicolon": ';',
	"less": '<', "equal": '=', "greater": '>', "question": '?', "at": '@', "bracketleft": '[',
	"backslash": '\\', "bracketright": ']', "asciicircum": '^', "underscore": '_', "grave": '`',
	"quoteleft": '`', "braceleft": '{', "bar": '|', "braceright": '}', "asciitilde": '~',
}

func init() {
	for i := 1; i <= 35; i++ {
		names[fmt.Sprintf("f%d", i)] = Keysym(0xffbe + i - 1)
	}
	for i := 0; i <= 9; i++ {
		names[fmt.Sprintf("kp_%d", i)] = Keysym(0xffb0 + i)
	}
}

// FromRune returns the keysym that types r.
func FromRune(r rune) Keysym {
	switch r {
	case '\n', '\r':
		return Return
	case '\t':
		return Tab
	case '\b':
		return BackSpace
	}
	if (r >= 0x20 && r <= 0x7e) || (r >= 0xa0 && r <= 0xff) {
		return Keysym(r)
	}
	return Keysym(0x01000000 | uint32(r))
}

// Lookup resolves one key name: an X keysym name ("Return", "Page_Up"), a
// friendly alias ("enter", "ctrl", "pgdn"), or a single character ("a", "%").
func Lookup(name string) (Keysym, bool) {
	if utf8.RuneCountInString(name) == 1 {
		r, _ := utf8.DecodeRuneInString(name)
		return FromRune(r), true
	}
	if s, ok := names[strings.ToLower(name)]; ok {
		return s, true
	}
	// Unicode keysym names like "U20AC".
	var cp uint32
	if len(name) > 1 && (name[0] == 'U' || name[0] == 'u') {
		if _, err := fmt.Sscanf(name[1:], "%x", &cp); err == nil && cp > 0 {
			return FromRune(rune(cp)), true
		}
	}
	return 0, false
}

// ParseCombo parses a chord like "ctrl+shift+t" or "super+Return" into
// keysyms, modifiers first. A literal "+" key may be written as "plus" or as
// a trailing "+" ("ctrl++").
func ParseCombo(combo string) ([]Keysym, error) {
	combo = strings.TrimSpace(combo)
	if combo == "" {
		return nil, fmt.Errorf("empty key")
	}
	var parts []string
	if combo == "+" {
		parts = []string{"+"}
	} else {
		parts = strings.Split(combo, "+")
		if strings.HasSuffix(combo, "++") {
			parts = append(parts[:len(parts)-2], "+")
		}
	}
	syms := make([]Keysym, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		// In chords "ctrl+C" means ctrl+c, not ctrl+shift+c.
		if len(parts) > 1 && len(p) == 1 && p[0] >= 'A' && p[0] <= 'Z' {
			p = strings.ToLower(p)
		}
		s, ok := Lookup(p)
		if !ok {
			return nil, fmt.Errorf("unknown key %q in %q", p, combo)
		}
		syms = append(syms, s)
	}
	return syms, nil
}

// spacedPlus matches "+" with surrounding spaces, as in "ctrl + c".
var spacedPlus = regexp.MustCompile(`\s*\+\s*`)

// ParseChords parses one or more space-separated chords ("ctrl+a Delete").
// Spaces around "+" are ignored, so "ctrl + alt + Delete" is one chord.
// Every caller (gates and actions) uses this, so what is checked is exactly
// what gets pressed.
func ParseChords(s string) ([][]Keysym, error) {
	s = spacedPlus.ReplaceAllString(strings.TrimSpace(s), "+")
	var chords [][]Keysym
	for _, f := range strings.Fields(s) {
		syms, err := ParseCombo(f)
		if err != nil {
			return nil, err
		}
		chords = append(chords, syms)
	}
	if len(chords) == 0 {
		return nil, fmt.Errorf("no key given")
	}
	return chords, nil
}

// ModMask returns the XKB real-modifier bit a modifier keysym drives
// (Shift=1, Lock=2, Control=4, Mod1/Alt=8, Mod2/NumLock=16, Mod4/Super=64),
// or 0 for ordinary keys.
func ModMask(s Keysym) uint32 {
	switch s {
	case ShiftL, ShiftR:
		return 1 << 0
	case CapsLock:
		return 1 << 1
	case ControlL, ControlR:
		return 1 << 2
	case AltL, AltR, MetaL:
		return 1 << 3
	case NumLock:
		return 1 << 4
	case SuperL, SuperR:
		return 1 << 6
	}
	return 0
}
