package keys

import (
	"slices"
	"testing"
)

func TestParseCombo(t *testing.T) {
	cases := map[string][]Keysym{
		"ctrl+shift+t": {ControlL, ShiftL, 't'},
		"ctrl+C":       {ControlL, 'c'},
		"ctrl++":       {ControlL, '+'},
		"super+Return": {SuperL, Return},
		"Page_Down":    {0xff56},
		"F5":           {0xffc2},
		"A":            {'A'},
		"alt+ñ":        {AltL, 0xf1},
	}
	for in, want := range cases {
		got, err := ParseCombo(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("ParseCombo(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseCombo("ctrl+nope"); err == nil {
		t.Error("expected error for unknown key")
	}
}

func TestUSStrokes(t *testing.T) {
	if s, ok := US('A'); !ok || s.Code != 30 || !s.Shift {
		t.Errorf("US('A') = %+v, %v", s, ok)
	}
	if s, ok := US(Return); !ok || s.Code != 28 || s.Shift {
		t.Errorf("US(Return) = %+v, %v", s, ok)
	}
	if _, ok := US(FromRune('日')); ok {
		t.Error("US layout should not produce CJK")
	}
}
