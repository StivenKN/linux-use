package main

import (
	"github.com/StivenKN/linux-use/internal/desktop"
	"github.com/StivenKN/linux-use/internal/desktop/kde"
	"github.com/StivenKN/linux-use/internal/desktop/wlr"
	"github.com/StivenKN/linux-use/internal/desktop/x11"
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
	func(s session.Session) (desktop.Backend, error) {
		if s.Wayland == "" {
			return nil, desktop.ErrUnsupported
		}
		return kde.Open(s.Wayland) // tells KWin apart by its globals, so XDG_CURRENT_DESKTOP needn't say KDE
	},
	func(s session.Session) (desktop.Backend, error) {
		if s.X11 == "" {
			return nil, desktop.ErrUnsupported
		}
		return x11.Open(s.X11)
	},
}
