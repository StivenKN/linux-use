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
	if x != 3840 || y != 540 {
		t.Errorf("ToScreen right-middle = %d,%d", x, y)
	}
	ix, iy := s.FromScreen(1920+960, 540)
	if ix != (s.Width+1)/2 || iy != (s.Height+1)/2 {
		t.Errorf("FromScreen center = %d,%d", ix, iy)
	}
}
