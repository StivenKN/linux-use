// Package session finds the graphical session to drive and manages the
// optional headless "agent desktop" for machines without one.
package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Session is a graphical session the agent can drive.
type Session struct {
	Wayland string `json:"wayland_display,omitempty"` // WAYLAND_DISPLAY value
	X11     string `json:"display,omitempty"`         // DISPLAY value
	Desktop string `json:"desktop,omitempty"`         // XDG_CURRENT_DESKTOP
	Managed bool   `json:"managed,omitempty"`         // the linux-use agent desktop
	VNC     string `json:"vnc,omitempty"`             // host:port of the viewer (managed only)
}

func (s Session) String() string {
	var parts []string
	if s.Managed {
		parts = append(parts, "agent desktop")
	}
	if s.Wayland != "" {
		parts = append(parts, "wayland "+s.Wayland)
	}
	if s.X11 != "" {
		parts = append(parts, "x11 "+s.X11)
	}
	if s.Desktop != "" {
		parts = append(parts, s.Desktop)
	}
	return strings.Join(parts, ", ")
}

// Env returns the environment for launching applications inside this session.
func (s Session) Env() []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "WAYLAND_DISPLAY", "DISPLAY", "WAYLAND_SOCKET":
			return true
		}
		return s.Managed && k == "XDG_CURRENT_DESKTOP"
	})
	env = append(env, "XDG_RUNTIME_DIR="+RuntimeDir())
	// Apps that find no session bus misbehave (Firefox can't reach its
	// running instance and reports "already running").
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		if bus := filepath.Join(RuntimeDir(), "bus"); socketExists(bus) {
			env = append(env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+bus)
		}
	}
	if s.Wayland != "" {
		env = append(env, "WAYLAND_DISPLAY="+s.Wayland, "XDG_SESSION_TYPE=wayland")
	}
	if s.X11 != "" {
		env = append(env, "DISPLAY="+s.X11)
	}
	if s.Managed {
		env = append(env,
			"XDG_CURRENT_DESKTOP=labwc:wlroots",
			"MOZ_ENABLE_WAYLAND=1",
			"ELECTRON_OZONE_PLATFORM_HINT=auto",
			"QT_QPA_PLATFORM=wayland;xcb",
			"SDL_VIDEODRIVER=wayland,x11",
		)
	}
	return env
}

// ErrNoSession means no graphical session was found and none could be started.
var ErrNoSession = errors.New("no graphical session found")

// Target selects which session to drive, from LINUX_USE_TARGET:
//
//	auto     (default) the session we were started in, else the agent desktop
//	agent    always the headless agent desktop, even inside a real desktop
//	window   the agent desktop, shown as a window inside the current desktop
//	desktop  only the session we were started in; never start the agent desktop
//	wayland-N or :N  a specific Wayland socket or X display
func Target() string {
	if t := os.Getenv("LINUX_USE_TARGET"); t != "" {
		return t
	}
	return "auto"
}

// Find resolves the session to drive. With start set, it boots the agent
// desktop when nothing else is available.
func Find(start bool) (Session, error) {
	target := Target()
	switch {
	case strings.HasPrefix(target, "wayland-") || filepath.IsAbs(target):
		if !socketAlive(target) {
			return Session{}, fmt.Errorf("Wayland socket %s is not accepting connections", target)
		}
		return Session{Wayland: target, Desktop: os.Getenv("XDG_CURRENT_DESKTOP")}, nil
	case strings.HasPrefix(target, ":"):
		return Session{X11: target, Desktop: os.Getenv("XDG_CURRENT_DESKTOP")}, nil
	case target == "agent":
		return agent(start, false)
	case target == "window":
		return agent(start, true)
	}

	if s, ok := inherited(); ok {
		return s, nil
	}
	if target == "desktop" {
		return Session{}, fmt.Errorf("%w: WAYLAND_DISPLAY and DISPLAY are unset (LINUX_USE_TARGET=desktop)", ErrNoSession)
	}
	return agent(start, false)
}

// WantsAgent reports whether Find would drive the agent desktop, without
// starting anything.
func WantsAgent() bool {
	switch Target() {
	case "agent", "window":
		return true
	case "auto":
		_, ok := inherited()
		return !ok
	}
	return false
}

// Key identifies the session's display, for per-desktop locks.
func (s Session) Key() string {
	if s.Wayland != "" {
		return filepath.Base(s.Wayland)
	}
	return "x11" + strings.ReplaceAll(s.X11, "/", "_")
}

// inherited returns the session from our own environment, as when Claude
// Code runs in a terminal on the user's desktop.
func inherited() (Session, bool) {
	desk := os.Getenv("XDG_CURRENT_DESKTOP")
	if d := os.Getenv("WAYLAND_DISPLAY"); d != "" && socketAlive(d) {
		return Session{Wayland: d, X11: os.Getenv("DISPLAY"), Desktop: desk}, true
	}
	if d := os.Getenv("DISPLAY"); d != "" {
		return Session{X11: d, Desktop: desk}, true
	}
	return Session{}, false
}

func agent(start, window bool) (Session, error) {
	if s, err := Running(); err == nil {
		return s, nil
	}
	if !start {
		return Session{}, fmt.Errorf("%w: the agent desktop is not running (start it with `linux-use desktop up`)", ErrNoSession)
	}
	return Up(Options{Nested: window})
}

// Fallback starts the agent desktop for a session no backend can drive
// (e.g. KDE): as a window inside it, so the user still sees Claude work.
func Fallback(s Session) (Session, error) {
	if Target() != "auto" || s.Managed {
		return Session{}, ErrNoSession
	}
	return agent(true, s.Wayland != "" || s.X11 != "")
}

// RuntimeDir returns XDG_RUNTIME_DIR, falling back to /run/user/$UID.
func RuntimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	d := fmt.Sprintf("/run/user/%d", os.Getuid())
	if st, err := os.Stat(d); err == nil && st.IsDir() {
		os.Setenv("XDG_RUNTIME_DIR", d)
		return d
	}
	d = filepath.Join(os.TempDir(), fmt.Sprintf("linux-use-runtime-%d", os.Getuid()))
	os.MkdirAll(d, 0o700)
	os.Setenv("XDG_RUNTIME_DIR", d)
	return d
}

func socketExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode()&os.ModeSocket != 0
}

// WaylandSockets lists Wayland sockets in the runtime dir that accept connections.
func WaylandSockets() []string {
	matches, _ := filepath.Glob(filepath.Join(RuntimeDir(), "wayland-*"))
	var out []string
	for _, m := range matches {
		name := filepath.Base(m)
		if strings.HasSuffix(name, ".lock") || !socketAlive(name) {
			continue
		}
		out = append(out, name)
	}
	return out
}
