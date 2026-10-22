package mcpserver

import (
	"testing"

	"github.com/StivenKN/linux-use/internal/keys"
)

func TestTierOf(t *testing.T) {
	click := []string{"foot", "org.gnome.Terminal", "org.gnome.Console", "org.kde.konsole", "Alacritty",
		"com.mitchellh.ghostty", "org.wezfurlong.wezterm", "code", "code-url-handler", "jetbrains-idea", "dev.zed.Zed", "XTerm"}
	full := []string{"firefox_firefox", "org.mozilla.firefox", "google-chrome", "org.gnome.Nautilus", "libreoffice-writer", "gimp"}
	for _, id := range click {
		if tierOf(id) != tierClick {
			t.Errorf("tierOf(%q) = %s, want click", id, tierOf(id))
		}
	}
	for _, id := range full {
		if tierOf(id) != tierFull {
			t.Errorf("tierOf(%q) = %s, want full", id, tierOf(id))
		}
	}
}

func TestIsSystemCombo(t *testing.T) {
	cases := map[string]bool{
		"alt+F4": true, "alt+tab": true, "super": true, "super+l": true, "ctrl+alt+Delete": true,
		"ctrl+alt+F3": true, "ctrl+q": true, "ctrl+Q": true, "Print": true,
		"ctrl+c": false, "ctrl+shift+t": false, "alt+f": false, "Return": false, "super+Return": false,
	}
	for combo, want := range cases {
		syms, err := keys.ParseCombo(combo)
		if err != nil {
			t.Fatal(err)
		}
		if got := isSystemCombo(syms); got != want {
			t.Errorf("isSystemCombo(%q) = %v, want %v", combo, got, want)
		}
	}
}
