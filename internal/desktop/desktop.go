// Package desktop defines the backend contract every Linux graphical stack
// (wlroots Wayland, GNOME/KDE Wayland, X11) implements.
package desktop

import (
	"context"
	"errors"
	"image"

	"github.com/StivenKN/linux-use/internal/keys"
)

// Display is one monitor in the session's global logical coordinate space.
type Display struct {
	ID     int    // 1-based, ordered left to right
	Name   string // connector or output name, e.g. "HEADLESS-1", "eDP-1"
	X, Y   int    // logical position
	Width  int    // logical size
	Height int
	Scale  float64 // physical pixels per logical pixel, filled in after capture
}

// Contains reports whether the logical point lies on this display.
func (d Display) Contains(x, y int) bool {
	return x >= d.X && y >= d.Y && x < d.X+d.Width && y < d.Y+d.Height
}

// Window is a top-level application window.
type Window struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	AppID     string `json:"app_id"`
	Focused   bool   `json:"focused,omitempty"`
	Minimized bool   `json:"minimized,omitempty"`
}

// Button is a Linux evdev button code.
type Button uint32

const (
	Left    Button = 0x110
	Right   Button = 0x111
	Middle  Button = 0x112
	Back    Button = 0x113
	Forward Button = 0x114
)

// Backend drives one graphical session. Implementations need not be safe
// for concurrent use; callers serialize.
type Backend interface {
	// Name identifies the backend, e.g. "wlroots" or "x11".
	Name() string
	Displays(ctx context.Context) ([]Display, error)
	// Capture returns the display's pixels at physical resolution.
	Capture(ctx context.Context, d Display) (*image.RGBA, error)
	// Move warps the pointer to a global logical coordinate.
	Move(ctx context.Context, x, y int) error
	Button(ctx context.Context, b Button, down bool) error
	// Scroll sends wheel clicks; positive dy scrolls down, positive dx right.
	Scroll(ctx context.Context, dx, dy int) error
	Key(ctx context.Context, sym keys.Keysym, down bool) error
	// Type enters text, independent of the user's keyboard layout where possible.
	Type(ctx context.Context, text string) error
	Close() error
}

// Windows is implemented by backends that can list and focus windows.
type Windows interface {
	Windows(ctx context.Context) ([]Window, error)
	Activate(ctx context.Context, id string) error
}

// Clipboard is implemented by backends with clipboard access.
type Clipboard interface {
	ReadClipboard(ctx context.Context) (string, error)
	WriteClipboard(ctx context.Context, text string) error
}

// ErrUnsupported reports that a backend cannot drive the session it was
// pointed at, so the caller should try the next one.
var ErrUnsupported = errors.New("session not supported by this backend")
