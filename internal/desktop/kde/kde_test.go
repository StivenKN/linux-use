package kde

import (
	"bytes"
	"image/color"
	"testing"
)

func TestDecode(t *testing.T) {
	// One 2x1 row padded to a 12-byte stride: red, then blue (alpha ignored).
	for _, tc := range []struct {
		format int
		row    []byte
	}{
		{qimageARGB32Premultiplied, []byte{0, 0, 0xff, 0xff, 0xff, 0, 0, 0x80, 9, 9, 9, 9}},
		{qimageRGBA8888, []byte{0xff, 0, 0, 0xff, 0, 0, 0xff, 0x80, 9, 9, 9, 9}},
	} {
		img, err := decode(tc.row, 2, 1, 12, tc.format)
		if err != nil {
			t.Fatalf("format %d: %v", tc.format, err)
		}
		if got := img.RGBAAt(0, 0); got != (color.RGBA{0xff, 0, 0, 0xff}) {
			t.Errorf("format %d: pixel 0 = %v, want opaque red", tc.format, got)
		}
		if got := img.RGBAAt(1, 0); got != (color.RGBA{0, 0, 0xff, 0xff}) {
			t.Errorf("format %d: pixel 1 = %v, want opaque blue", tc.format, got)
		}
	}
	if _, err := decode(make([]byte, 8), 2, 1, 8, 13); err == nil {
		t.Error("unknown QImage format decoded without error")
	}
}

func TestDesktopEntryExec(t *testing.T) {
	for exe, want := range map[string]string{
		"/home/me/.cache/linux-use/0.1.0/linux-use-amd64": "Exec=/home/me/.cache/linux-use/0.1.0/linux-use-amd64\n",
		"/home/me/My Tools/linux-use":                     "Exec=\"/home/me/My Tools/linux-use\"\n",
	} {
		if e := desktopEntry(exe); !bytes.Contains(e, []byte(want)) {
			t.Errorf("entry for %q lacks %q:\n%s", exe, want, e)
		}
	}
}
