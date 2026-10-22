package main

import (
	"github.com/StivenKN/linux-use/internal/desktop"
	"github.com/StivenKN/linux-use/internal/desktop/gnome"
	"github.com/StivenKN/linux-use/internal/desktop/wlr"
	"github.com/StivenKN/linux-use/internal/session"
)

// backends are tried in order; the first that can drive the session wins.
// An opener returns an error wrapping desktop.ErrUnsupported to pass.
var backends = []func(session.Session) (desktop.Backend, error){
	func(s session.Session) (desktop.Backend, error) {
		if s.Wayland == "" {
			return nil, desktop.ErrUnsupported
		}
		return wlr.Open(s.Wayland)
	},
	// GNOME lacks the wlroots protocols, so wlr passes on it.
	func(s session.Session) (desktop.Backend, error) {
		if s.Wayland == "" || s.Managed {
			return nil, desktop.ErrUnsupported
		}
		return gnome.Open(s.Wayland)
	},
}
