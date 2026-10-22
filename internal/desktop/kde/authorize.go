package kde

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KWin hides its automation interfaces from every client except those whose
// executable (/proc/PID/exe) matches the absolute Exec= path of an installed
// application entry that lists them. ScreenShot2 checks the D-Bus caller the
// same way. authorize installs that entry for the running binary: a one-time
// step with no prompt, repeated only when the binary moves (e.g. an update).
const (
	waylandInterfaces = "org_kde_kwin_fake_input,org_kde_plasma_window_management"
	dbusInterfaces    = "org.kde.KWin.ScreenShot2"
)

// desktopEntry renders the application entry that authorizes exe.
func desktopEntry(exe string) []byte {
	if strings.ContainsAny(exe, " \t") {
		exe = `"` + exe + `"` // KWin splits Exec= like a shell command line
	}
	return fmt.Appendf(nil, `[Desktop Entry]
Type=Application
Name=linux-use
Comment=Lets Claude Code's computer use (linux-use) see the screen and send input
Exec=%s
NoDisplay=true
X-KDE-Wayland-Interfaces=%s
X-KDE-DBUS-Restricted-Interfaces=%s
`, exe, waylandInterfaces, dbusInterfaces)
}

// desktopFile is where the entry lives: $XDG_DATA_HOME/applications.
func desktopFile() (string, error) {
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		data = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(data, "applications", "linux-use.desktop"), nil
}

// authorize makes sure the entry for this executable is installed. It
// reports whether it changed anything, in which case KWin needs a moment to
// notice before a new connection gets the restricted interfaces.
func authorize() (path string, changed bool, err error) {
	exe, err := os.Executable()
	if err != nil {
		return "", false, err
	}
	if path, err = desktopFile(); err != nil {
		return "", false, err
	}
	want := desktopEntry(exe)
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, want) {
		return path, false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return path, false, err
	}
	// Write a temporary file and rename it into place: KDE's application
	// cache (KSycoca) only rebuilds when the directory's mtime changes, which
	// rewriting an existing file in place would not do.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".linux-use-*.tmp")
	if err != nil {
		return path, false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(want); err != nil {
		tmp.Close()
		return path, false, err
	}
	if err := tmp.Close(); err != nil {
		return path, false, err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return path, false, err
	}
	return path, true, os.Rename(tmp.Name(), path)
}
