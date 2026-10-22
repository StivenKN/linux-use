package session

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/StivenKN/linux-use/internal/wl"
)

//go:embed assets
var assets embed.FS

const unit = "linux-use-desktop"

// Options configures the agent desktop.
type Options struct {
	Width, Height int    // output size, default 1280x800
	VNCListen     string // TCP address for the viewer; "" serves it on a private unix socket
	VNCPort       int    // TCP port when VNCListen is set; 0 picks 5900 or the next free one
	NoVNC         bool   // don't start the viewer
	Nested        bool   // open as a window inside the current desktop instead of headless
}

func (o *Options) defaults() {
	if o.VNCListen == "" {
		o.VNCListen = os.Getenv("LINUX_USE_VNC_LISTEN")
	}
	if o.Width == 0 || o.Height == 0 {
		o.Width, o.Height = 1280, 800
		if w, h, ok := parseSize(os.Getenv("LINUX_USE_RESOLUTION")); ok {
			o.Width, o.Height = w, h
		}
	}
}

func parseSize(s string) (int, int, bool) {
	ws, hs, ok := strings.Cut(strings.ToLower(s), "x")
	w, err1 := strconv.Atoi(ws)
	h, err2 := strconv.Atoi(hs)
	return w, h, ok && err1 == nil && err2 == nil && w >= 320 && h >= 240
}

// StateDir holds the agent desktop's config, state file and logs.
func StateDir() string { return filepath.Join(RuntimeDir(), "linux-use") }

func statePath() string { return filepath.Join(StateDir(), "desktop.json") }

// Running returns the agent desktop if its compositor is alive.
func Running() (Session, error) {
	b, err := os.ReadFile(statePath())
	if err != nil {
		return Session{}, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return Session{}, err
	}
	// The socket name alone isn't proof: after a crash, another compositor
	// (the user's own desktop) may own it. Check who is listening.
	if pid, err := peerPID(s.Wayland); err != nil || s.PID == 0 || pid != s.PID {
		return Session{}, errors.New("agent desktop is not running")
	}
	return s, nil
}

// peerPID returns the pid of the process serving a Wayland socket.
func peerPID(display string) (int, error) {
	c, err := net.DialTimeout("unix", wl.SocketPath(display), time.Second)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	raw, err := c.(*net.UnixConn).SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var credErr error
	raw.Control(func(fd uintptr) { cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	if credErr != nil {
		return 0, credErr
	}
	return int(cred.Pid), nil
}

// Compositor returns the compositor binary we can run, preferring labwc.
func Compositor() (string, error) {
	for _, c := range []string{"labwc", "sway"} {
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no graphical session is available and the agent desktop is not installed. Install it with:\n  %s\nthen try again (or run /linux-use:setup)", InstallCommand(DesktopPackages))
}

// Up starts the agent desktop (or returns it if already running) and waits
// until it accepts connections.
func Up(opts Options) (Session, error) {
	if s, err := Running(); err == nil {
		return s, nil
	}
	// Serialize starts: two Claude sessions using computer use for the first
	// time at once must not start two desktops.
	dir := StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Session{}, err
	}
	lockf, err := os.OpenFile(filepath.Join(dir, "up.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return Session{}, err
	}
	defer lockf.Close()
	if err := unix.Flock(int(lockf.Fd()), unix.LOCK_EX); err != nil {
		return Session{}, err
	}
	if s, err := Running(); err == nil {
		return s, nil
	}
	// Whatever is left of a previous desktop is stale now.
	Down()
	opts.defaults()
	comp, err := Compositor()
	if err != nil {
		return Session{}, err
	}
	cfg, err := writeConfig(dir, opts)
	if err != nil {
		return Session{}, err
	}
	self, err := os.Executable()
	if err != nil {
		return Session{}, err
	}

	hook := shellQuote(self) + " desktop hook"
	var argv []string
	if filepath.Base(comp) == "sway" {
		argv = []string{comp, "--unsupported-gpu", "-c", filepath.Join(cfg, "sway.conf")}
	} else {
		argv = []string{comp, "-C", cfg, "-s", hook}
	}
	env := map[string]string{
		"XDG_RUNTIME_DIR":              RuntimeDir(),
		"XDG_SESSION_TYPE":             "wayland",
		"XDG_CURRENT_DESKTOP":          "labwc:wlroots",
		"LINUX_USE_VNC":                vncSetting(opts),
		"LINUX_USE_RESOLUTION":         fmt.Sprintf("%dx%d", opts.Width, opts.Height),
		"LINUX_USE_CONFIG":             cfg,
		"PATH":                         os.Getenv("PATH"),
		"MOZ_ENABLE_WAYLAND":           "1",
		"MOZ_CRASHREPORTER_DISABLE":    "1",
		"ELECTRON_OZONE_PLATFORM_HINT": "auto",
	}
	if opts.Nested {
		env["WLR_BACKENDS"] = "wayland"
		if os.Getenv("WAYLAND_DISPLAY") == "" {
			env["WLR_BACKENDS"] = "x11"
		}
	} else {
		env["WLR_BACKENDS"] = "headless"
		env["WLR_HEADLESS_OUTPUTS"] = "1"
		env["WLR_LIBINPUT_NO_DEVICES"] = "1"
		env["WLR_RENDERER"] = "pixman"
	}
	if err := launch(argv, env, opts.Nested); err != nil {
		return Session{}, err
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if s, err := Running(); err == nil {
			return s, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return Session{}, fmt.Errorf("agent desktop did not come up within 20s; logs: %s", logHint())
}

// vncSetting picks where the viewer listens. By default it's a unix socket
// in the private runtime dir, so only this user (and their SSH tunnels) can
// reach it; the VNC server itself has no password.
func vncSetting(o Options) string {
	switch {
	case o.NoVNC:
		return "off"
	case o.VNCListen == "":
		return "unix:" + filepath.Join(StateDir(), "vnc.sock")
	}
	port := o.VNCPort
	if port == 0 {
		port = freePort(o.VNCListen, 5900)
	}
	return net.JoinHostPort(o.VNCListen, strconv.Itoa(port))
}

// launch runs the compositor outside our process tree so it outlives the MCP
// server: as a transient systemd user unit when possible, else detached.
func launch(argv []string, env map[string]string, nested bool) error {
	if !nested && systemdUser() {
		args := []string{"--user", "--unit=" + unit, "--collect", "--quiet",
			"--property=KillMode=control-group", "--property=TimeoutStopSec=5"}
		for k, v := range env {
			args = append(args, "--setenv="+k+"="+v)
		}
		args = append(args, "--")
		args = append(args, argv...)
		exec.Command("systemctl", "--user", "reset-failed", unit).Run()
		if out, err := exec.Command("systemd-run", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("start agent desktop: systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	logf, err := os.OpenFile(filepath.Join(StateDir(), "compositor.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	if bus := filepath.Join(RuntimeDir(), "bus"); os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" && socketExists(bus) {
		cmd.Env = append(cmd.Env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+bus)
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if !nested {
		cmd.Env = append(cmd.Env, "WAYLAND_DISPLAY=", "DISPLAY=")
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	os.WriteFile(pidPath(), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	return cmd.Process.Release()
}

// systemdUser reports whether a systemd user manager can run our unit.
func systemdUser() bool {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	return exec.Command("systemctl", "--user", "show-environment").Run() == nil
}

func logHint() string {
	if systemdUser() {
		return "journalctl --user -u " + unit
	}
	return filepath.Join(StateDir(), "compositor.log")
}

func pidPath() string { return filepath.Join(StateDir(), "compositor.pid") }

// Down stops the agent desktop and every app running inside it.
func Down() error {
	exec.Command("systemctl", "--user", "stop", unit).Run()
	if b, err := os.ReadFile(pidPath()); err == nil {
		if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 0 {
			syscall.Kill(pid, syscall.SIGTERM)
		}
		os.Remove(pidPath())
	}
	os.Remove(statePath())
	return nil
}

// Hook runs inside the freshly started compositor (labwc -s / sway exec). It
// sizes the output, starts the viewer, wallpaper and panel, then records the
// session so clients can find it.
func Hook() error {
	display := os.Getenv("WAYLAND_DISPLAY")
	if display == "" {
		return errors.New("desktop hook must run inside the compositor")
	}
	dir := StateDir()
	cfg := os.Getenv("LINUX_USE_CONFIG")
	if w, h, ok := parseSize(os.Getenv("LINUX_USE_RESOLUTION")); ok {
		if c, err := wl.Dial(display); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := wl.SetOutputMode(ctx, c, "", w, h); err != nil {
				fmt.Fprintln(os.Stderr, "linux-use: resize output:", err)
			}
			cancel()
			c.Close()
		}
	}

	pid, err := peerPID(display)
	if err != nil {
		return fmt.Errorf("find compositor: %w", err)
	}
	s := Session{Wayland: display, X11: os.Getenv("DISPLAY"), Desktop: os.Getenv("XDG_CURRENT_DESKTOP"), Managed: true, PID: pid}
	spawn := func(name string, args ...string) bool {
		if _, err := exec.LookPath(name); err != nil {
			return false
		}
		logf, _ := os.OpenFile(filepath.Join(dir, name+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		cmd := exec.Command(name, args...)
		cmd.Stdout, cmd.Stderr = logf, logf
		return cmd.Start() == nil
	}
	spawn("swaybg", "-c", "#2e3440")
	spawn("waybar", "-c", filepath.Join(cfg, "waybar.jsonc"), "-s", filepath.Join(cfg, "waybar.css"))
	if vnc := os.Getenv("LINUX_USE_VNC"); vnc != "off" && vnc != "" && os.Getenv("WLR_BACKENDS") == "headless" {
		args := []string{"--render-cursor", "--disable-resizing"}
		if sock, ok := strings.CutPrefix(vnc, "unix:"); ok {
			os.Remove(sock)
			args = append(args, "--unix-socket", sock)
		} else {
			host, port, _ := net.SplitHostPort(vnc)
			args = append(args, host, port)
		}
		if spawn("wayvnc", args...) {
			s.VNC = vnc
		}
	}

	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath())
}

// writeConfig materializes the compositor config into dir/config.
func writeConfig(dir string, opts Options) (string, error) {
	cfg := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		return "", err
	}
	entries, err := assets.ReadDir("assets")
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		b, _ := assets.ReadFile("assets/" + e.Name())
		if err := os.WriteFile(filepath.Join(cfg, e.Name()), b, 0o600); err != nil {
			return "", err
		}
	}
	self, _ := os.Executable()
	sway := fmt.Sprintf("output * resolution %dx%d bg #2e3440 solid_color\nfocus_follows_mouse no\nexec %s desktop hook\n",
		opts.Width, opts.Height, shellQuote(self))
	return cfg, os.WriteFile(filepath.Join(cfg, "sway.conf"), []byte(sway), 0o600)
}

func socketAlive(display string) bool {
	c, err := net.DialTimeout("unix", wl.SocketPath(display), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func freePort(host string, from int) int {
	for p := from; p < from+100; p++ {
		l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
		if err == nil {
			l.Close()
			return p
		}
	}
	return from
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
