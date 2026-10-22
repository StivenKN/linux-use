package computer

import (
	"testing"

	"github.com/StivenKN/linux-use/internal/desktop"
)

func TestFit(t *testing.T) {
	// Sizes Claude Code's built-in computer use produces for the same displays.
	cases := []struct{ w, h, ww, wh int }{
		{1280, 800, 1280, 800},
		{1920, 1080, 1456, 819},
		{2560, 1440, 1456, 819},
		{3840, 2160, 1456, 819},
		{3024, 1964, 1372, 891},
		{3456, 2234, 1372, 887},
		{1080, 1920, 819, 1456},
	}
	for _, c := range cases {
		if w, h := fit(c.w, c.h); w != c.ww || h != c.wh {
			t.Errorf("fit(%d,%d) = %dx%d, want %dx%d", c.w, c.h, w, h, c.ww, c.wh)
		}
	}
}

func TestShotMapping(t *testing.T) {
	// A 4K monitor at scale 2 placed right of a 1080p one.
	d := desktop.Display{X: 1920, Y: 0, Width: 1920, Height: 1080}
	s := shotFor(d)
	x, y := s.ToScreen(float64(s.Width), float64(s.Height)/2)
	if x != 3839 || y != 540 { // clamped to the last column
		t.Errorf("ToScreen right-middle = %d,%d", x, y)
	}
	ix, iy := s.FromScreen(1920+960, 540)
	if ix != (s.Width+1)/2 || iy != (s.Height+1)/2 {
		t.Errorf("FromScreen center = %d,%d", ix, iy)
	}
}

func TestHiDPIFitsPhysicalPixels(t *testing.T) {
	// A 1280x800 logical display at scale 2 is 2560x1600 physical, which
	// Claude Code sends as 1389x868.
	s := shotFor(desktop.Display{Width: 1280, Height: 800, Scale: 2})
	if s.Width != 1389 || s.Height != 868 {
		t.Errorf("got %dx%d", s.Width, s.Height)
	}
	if x, y := s.ToScreen(float64(s.Width), float64(s.Height)); x != 1279 || y != 799 {
		t.Errorf("edge maps to %d,%d; want clamped 1279,799", x, y)
	}
}

func TestModifiersRejectKeys(t *testing.T) {
	if _, err := Modifiers("ctrl+shift"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"Return", "ctrl+c", "y"} {
		if _, err := Modifiers(bad); err == nil {
			t.Errorf("Modifiers(%q) accepted a non-modifier", bad)
		}
	}
}
