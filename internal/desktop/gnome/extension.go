package gnome

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// The linux-use GNOME Shell extension (extension/extension.js) exports a
// D-Bus interface on GNOME Shell's own bus connection, so callers reach it
// through the org.gnome.Shell name.
const (
	ExtensionUUID = "linux-use@stivenkn.github.io"
	shellName     = "org.gnome.Shell"
	shellPath     = "/org/gnome/Shell"
	extensionsAPI = "org.gnome.Shell.Extensions"
	extPath       = "/io/github/stivenkn/LinuxUse"
	extIface      = "io.github.stivenkn.LinuxUse"

	// extVersion is the Version extension.js reports; bump both together.
	extVersion = 1
	// minShell is the first GNOME Shell with ESM extensions. testedShell is
	// the newest one tested, declared as supported even on older shells so
	// upgrades up to it keep the extension loading.
	minShell, testedShell = 45, 50
)

//go:embed extension/extension.js
var extensionJS []byte

// GNOME Shell's ExtensionState values that need explaining.
const (
	stateError     = 3
	stateOutOfDate = 4
)

// Extension is the state of the linux-use extension for this user.
type Extension struct {
	Dir     string // install directory
	Shell   int    // running GNOME Shell's major version, 0 when unknown
	Files   bool   // installed
	Current bool   // installed files are the ones this build writes
	Enabled bool   // listed in enabled-extensions, not in disabled-extensions
	NoUser  bool   // org.gnome.shell disable-user-extensions is set
	Running uint32 // Version of the extension serving windows now, 0 if none
	state   int    // GNOME Shell's ExtensionState, 0 when it hasn't scanned it since login
	err     string // GNOME Shell's load error
}

// Problem explains why the extension isn't active and what to do, or
// returns "" when it is.
func (e Extension) Problem() string {
	const install = "run `linux-use gnome-extension install`"
	const relogin = "log out and back in once: GNOME Shell only loads newly installed extensions at login"
	switch {
	case e.Shell != 0 && e.Shell < minShell:
		return fmt.Sprintf("GNOME Shell %d is too old for the extension (it needs %d or newer)", e.Shell, minShell)
	case !e.Files:
		return "not installed: " + install
	case e.NoUser:
		return "GNOME has user extensions turned off (disable-user-extensions): turn them on in the Extensions app, " +
			"or run `gsettings set org.gnome.shell disable-user-extensions false`"
	case !e.Current:
		return "installed for another linux-use or GNOME version: " + install
	case !e.Enabled:
		return "installed but disabled: " + install
	case e.Running == extVersion:
		return ""
	case e.Running != 0:
		return "GNOME Shell still runs another version of it: log out and back in once to load this one"
	case e.state == stateError:
		return "GNOME Shell failed to load it: " + e.err
	case e.state == stateOutOfDate:
		return "GNOME Shell considers it out of date: " + install + ", then " + relogin
	}
	return relogin
}

// ExtensionStatus inspects the extension's files, GNOME's settings and the
// running shell.
func ExtensionStatus(ctx context.Context) (Extension, error) {
	bus, _ := sessionBus() // without one, report what the files and settings say
	if bus != nil {
		defer bus.Close()
	}
	return status(ctx, bus)
}

func status(ctx context.Context, bus *dbus.Conn) (Extension, error) {
	dir, err := extensionDir()
	if err != nil {
		return Extension{}, err
	}
	e := Extension{Dir: dir, Shell: shellMajor(ctx, bus)}
	e.Files, e.Current = true, true
	for name, want := range extensionFiles(e.Shell) {
		have, err := os.ReadFile(filepath.Join(dir, name))
		e.Files = e.Files && err == nil
		e.Current = e.Current && bytes.Equal(have, want)
	}
	on, err := shellStrv("enabled-extensions")
	if err != nil {
		return e, err
	}
	off, err := shellStrv("disabled-extensions")
	if err != nil {
		return e, err
	}
	e.Enabled = slices.Contains(on, ExtensionUUID) && !slices.Contains(off, ExtensionUUID)
	noUser, err := gsettings("get", "org.gnome.shell", "disable-user-extensions")
	if err != nil {
		return e, err
	}
	e.NoUser = noUser == "true"
	if bus == nil {
		return e, nil
	}
	e.Running, _ = runningVersion(ctx, bus)
	var info map[string]dbus.Variant
	if bus.Object(shellName, shellPath).CallWithContext(ctx, extensionsAPI+".GetExtensionInfo", 0, ExtensionUUID).Store(&info) == nil {
		state, _ := info["state"].Value().(float64)
		e.state = int(state)
		e.err, _ = info["error"].Value().(string)
	}
	return e, nil
}

// InstallExtension writes the extension for the running GNOME Shell and
// enables it. GNOME Shell only scans for extensions at login, so a first
// install becomes active at the next login; a shell that already knows the
// extension enables it right away. The returned status tells which.
func InstallExtension(ctx context.Context) (Extension, error) {
	bus, _ := sessionBus() // without one, install for the next login
	if bus != nil {
		defer bus.Close()
	}
	shell := shellMajor(ctx, bus)
	if shell == 0 {
		return Extension{}, errors.New("GNOME Shell not found (`gnome-shell --version` failed)")
	}
	if shell < minShell {
		return Extension{}, fmt.Errorf("GNOME Shell %d is too old for the extension (it needs %d or newer)", shell, minShell)
	}
	dir, err := extensionDir()
	if err != nil {
		return Extension{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Extension{}, err
	}
	for name, data := range extensionFiles(shell) {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return Extension{}, err
		}
	}
	// EnableExtension updates the settings itself, but only for extensions
	// the shell scanned at login.
	var known bool
	if bus != nil {
		bus.Object(shellName, shellPath).CallWithContext(ctx, extensionsAPI+".EnableExtension", 0, ExtensionUUID).Store(&known)
	}
	if !known {
		if err := setEnabled(true); err != nil {
			return Extension{}, err
		}
	} else {
		// The shell loads the module asynchronously after replying.
		for range 20 {
			if _, err := runningVersion(ctx, bus); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return status(ctx, bus)
}

// UninstallExtension removes the extension from GNOME's settings, which
// disables it in the running shell, and deletes its files.
func UninstallExtension() error {
	if err := setEnabled(false); err != nil {
		return err
	}
	dir, err := extensionDir()
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// extensionFiles renders the extension's files. metadata.json declares
// every major version from minShell up to the running shell (or
// testedShell), so new GNOME releases need no linux-use release.
func extensionFiles(shell int) map[string][]byte {
	var versions []string
	for v := minShell; v <= max(shell, testedShell); v++ {
		versions = append(versions, strconv.Itoa(v))
	}
	meta, _ := json.MarshalIndent(struct {
		UUID         string   `json:"uuid"`
		Name         string   `json:"name"`
		Description  string   `json:"description"`
		URL          string   `json:"url"`
		ShellVersion []string `json:"shell-version"`
	}{
		ExtensionUUID, "linux-use",
		"Lets linux-use (computer use for Claude Code) list and focus windows.",
		"https://github.com/StivenKN/linux-use", versions,
	}, "", "  ")
	return map[string][]byte{"metadata.json": append(meta, '\n'), "extension.js": extensionJS}
}

// extensionDir is where GNOME Shell looks for per-user extensions.
func extensionDir() (string, error) {
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		data = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(data, "gnome-shell", "extensions", ExtensionUUID), nil
}

// shellMajor returns GNOME Shell's major version: the running shell's when
// it's on the bus, else the installed one's. 0 means unknown.
func shellMajor(ctx context.Context, bus *dbus.Conn) int {
	var v string
	if bus != nil {
		var p dbus.Variant
		if bus.Object(shellName, shellPath).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0,
			extensionsAPI, "ShellVersion").Store(&p) == nil {
			v, _ = p.Value().(string)
		}
	}
	if v == "" {
		out, _ := exec.CommandContext(ctx, "gnome-shell", "--version").Output()
		v = strings.TrimPrefix(strings.TrimSpace(string(out)), "GNOME Shell ")
	}
	major, _, _ := strings.Cut(v, ".")
	n, _ := strconv.Atoi(major)
	return n
}

// runningVersion reads the Version of the extension serving the session;
// it fails when no extension is serving.
func runningVersion(ctx context.Context, bus *dbus.Conn) (uint32, error) {
	var v dbus.Variant
	err := bus.Object(shellName, extPath).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, extIface, "Version").Store(&v)
	if err != nil {
		return 0, err
	}
	n, _ := v.Value().(uint32)
	return n, nil
}

// setEnabled adds the extension to enabled-extensions or removes it, and
// always removes it from disabled-extensions, which takes precedence. The
// running shell follows both settings live.
func setEnabled(on bool) error {
	for _, k := range []struct {
		key  string
		want bool
	}{{"disabled-extensions", false}, {"enabled-extensions", on}} {
		list, err := shellStrv(k.key)
		if err != nil {
			return err
		}
		if slices.Contains(list, ExtensionUUID) == k.want {
			continue
		}
		if k.want {
			list = append(list, ExtensionUUID)
		} else {
			list = slices.DeleteFunc(list, func(s string) bool { return s == ExtensionUUID })
		}
		if _, err := gsettings("set", "org.gnome.shell", k.key, formatStrv(list)); err != nil {
			return err
		}
	}
	return nil
}

func shellStrv(key string) ([]string, error) {
	out, err := gsettings("get", "org.gnome.shell", key)
	if err != nil {
		return nil, err
	}
	return parseStrv(out)
}

func gsettings(args ...string) (string, error) {
	out, err := exec.Command("gsettings", args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			err = errors.New(strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("gsettings %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// parseStrv parses gsettings' text form of a string array, e.g.
// "['a@b', 'c@d']" or "@as []".
func parseStrv(s string) ([]string, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(strings.TrimPrefix(s, "@as")), "[")
	if !ok || !strings.HasSuffix(rest, "]") {
		return nil, fmt.Errorf("unexpected string list %q", s)
	}
	rest = strings.TrimSpace(strings.TrimSuffix(rest, "]"))
	var out []string
	for rest != "" {
		q := rest[0]
		if q != '\'' && q != '"' {
			return nil, fmt.Errorf("unexpected string list %q", s)
		}
		var b strings.Builder
		i := 1
		for ; i < len(rest) && rest[i] != q; i++ {
			if rest[i] == '\\' && i+1 < len(rest) {
				i++
			}
			b.WriteByte(rest[i])
		}
		if i == len(rest) {
			return nil, fmt.Errorf("unexpected string list %q", s)
		}
		out = append(out, b.String())
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest[i+1:]), ","))
	}
	return out, nil
}

// formatStrv renders a string array in the text form gsettings parses.
func formatStrv(list []string) string {
	quoted := make([]string, len(list))
	for i, s := range list {
		quoted[i] = "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
