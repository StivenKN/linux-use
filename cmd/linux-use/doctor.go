package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/StivenKN/linux-use/internal/session"
)

// runDoctor reports what linux-use can drive on this machine and prints the
// exact commands to fix anything missing.
func runDoctor() error {
	ok := func(f string, a ...any) { fmt.Printf("  ✓ "+f+"\n", a...) }
	bad := func(f string, a ...any) { fmt.Printf("  ✗ "+f+"\n", a...) }
	info := func(f string, a ...any) { fmt.Printf("  · "+f+"\n", a...) }

	fmt.Printf("linux-use %s\n\nSession\n", version)
	if d := os.Getenv("WAYLAND_DISPLAY"); d != "" {
		info("WAYLAND_DISPLAY=%s (%s)", d, os.Getenv("XDG_CURRENT_DESKTOP"))
	}
	if d := os.Getenv("DISPLAY"); d != "" {
		info("DISPLAY=%s", d)
	}
	if socks := session.WaylandSockets(); len(socks) > 0 {
		info("Wayland sockets in %s: %s", session.RuntimeDir(), strings.Join(socks, ", "))
	}
	info("target: %s (set LINUX_USE_TARGET=agent|desktop|wayland-N|:N to override)", session.Target())

	s, err := session.Find(false)
	if err != nil {
		info("%v", err)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b, _, err := connectTo(s)
		if err != nil {
			bad("cannot drive %s: %v", s, err)
		} else {
			ds, err := b.Displays(ctx)
			if err == nil && len(ds) > 0 {
				start := time.Now()
				if _, err := b.Capture(ctx, ds[0]); err != nil {
					bad("%s backend: screenshot failed: %v", b.Name(), err)
				} else {
					ok("%s backend drives %s: %d display(s), screenshot in %v", b.Name(), s, len(ds), time.Since(start).Round(time.Millisecond))
				}
			} else {
				bad("%s backend: no displays: %v", b.Name(), err)
			}
			b.Close()
		}
	}

	fmt.Println("\nAgent desktop (headless desktop for servers and SSH sessions)")
	var missing []string
	for _, p := range []struct{ bin, why string }{
		{"labwc", "compositor (sway also works)"},
		{"Xwayland", "runs X11-only apps"},
		{"wayvnc", "lets you watch and take over"},
		{"waybar", "taskbar"},
		{"swaybg", "wallpaper"},
		{"foot", "terminal"},
	} {
		if _, err := exec.LookPath(p.bin); err == nil {
			ok("%s: %s", p.bin, p.why)
		} else if p.bin == "labwc" {
			if _, err := exec.LookPath("sway"); err == nil {
				ok("sway: compositor")
				continue
			}
			bad("%s: %s", p.bin, p.why)
			missing = append(missing, p.bin)
		} else {
			bad("%s: %s (optional)", p.bin, p.why)
			missing = append(missing, p.bin)
		}
	}
	if s, err := session.Running(); err == nil {
		ok("running: %s, viewer %s", s, s.VNC)
	} else {
		info("not running (it starts on first use, or run `linux-use desktop up`)")
	}

	fmt.Println("\nApps")
	browsers := 0
	for _, b := range []string{"firefox", "chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "brave-browser", "microsoft-edge"} {
		if _, err := exec.LookPath(b); err == nil {
			ok("browser: %s", b)
			browsers++
		}
	}
	if browsers == 0 {
		bad("no web browser found")
	}

	if len(missing) > 0 {
		fmt.Printf("\nInstall the agent desktop:\n  %s\n", session.InstallCommand(missing))
	}
	return nil
}
