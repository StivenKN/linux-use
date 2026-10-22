// Command linux-use gives Claude Code computer use on Linux: an MCP server
// plus helpers to run and inspect a headless agent desktop.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/StivenKN/linux-use/internal/computer"
	"github.com/StivenKN/linux-use/internal/desktop"
	"github.com/StivenKN/linux-use/internal/mcpserver"
	"github.com/StivenKN/linux-use/internal/session"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "desktop":
		err = runDesktop(os.Args[2:])
	case "mcp":
		c := computer.New(connect)
		err = mcpserver.New(c, version).Run(context.Background())
	case "doctor":
		err = runDoctor()
	case "debug":
		err = runDebug(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "linux-use:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: linux-use <command>

  mcp       run the computer-use MCP server on stdio (used by the Claude Code plugin)
  desktop   manage the headless agent desktop: up | down | status
  doctor    check this machine and print what is missing
  debug     raw backend operations for manual testing
  version   print the version`)
}

func runDesktop(args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	switch args[0] {
	case "up":
		fs := flag.NewFlagSet("desktop up", flag.ExitOnError)
		size := fs.String("size", "", "output size WIDTHxHEIGHT (default 1280x800, or $LINUX_USE_RESOLUTION)")
		listen := fs.String("vnc-listen", "", "serve the VNC viewer on this TCP address (e.g. 127.0.0.1) instead of a private unix socket; it has no password")
		port := fs.Int("vnc-port", 0, "TCP port with --vnc-listen (default: first free from 5900)")
		noVNC := fs.Bool("no-vnc", false, "don't start the VNC viewer")
		nested := fs.Bool("window", false, "open the agent desktop as a window inside your current desktop")
		fs.Parse(args[1:])
		opts := session.Options{VNCListen: *listen, VNCPort: *port, NoVNC: *noVNC, Nested: *nested}
		if *size != "" {
			if _, err := fmt.Sscanf(*size, "%dx%d", &opts.Width, &opts.Height); err != nil {
				return fmt.Errorf("bad --size %q", *size)
			}
		}
		s, err := session.Up(opts)
		if err != nil {
			return err
		}
		printStatus(s)
		return nil
	case "down":
		return session.Down()
	case "status":
		s, err := session.Running()
		if err != nil {
			fmt.Println("agent desktop: not running")
			return nil
		}
		printStatus(s)
		return nil
	case "hook":
		return session.Hook()
	}
	return fmt.Errorf("unknown desktop command %q", args[0])
}

func printStatus(s session.Session) {
	fmt.Printf("agent desktop: running (%s)\n", s)
	if s.VNC == "" {
		return
	}
	host, _ := os.Hostname()
	target := s.VNC
	if sock, ok := strings.CutPrefix(s.VNC, "unix:"); ok {
		target = sock
		fmt.Printf("viewer:        VNC on unix socket %s (private to %s)\n", sock, os.Getenv("USER"))
	} else {
		fmt.Printf("viewer:        VNC on %s (no password: anyone who can reach it controls the desktop)\n", s.VNC)
	}
	fmt.Printf("watch it:      ssh -L 5900:%s %s@%s\n", target, os.Getenv("USER"), host)
	fmt.Printf("               then open vnc://localhost:5900 in a VNC viewer\n")
	if strings.HasPrefix(s.VNC, "unix:") {
		fmt.Printf("               (if your SSH server refuses socket forwarding, e.g. Tailscale SSH as another user,\n")
		fmt.Printf("               restart with `linux-use desktop up --vnc-listen 127.0.0.1` or LINUX_USE_VNC_LISTEN=127.0.0.1)\n")
	}
}

// connect finds (or starts) the session and opens a backend for it. When no
// backend can drive the user's desktop, it falls back to the agent desktop
// in a window.
func connect(ctx context.Context) (desktop.Backend, session.Session, error) {
	s, err := session.Find(true)
	if err != nil {
		return nil, s, err
	}
	b, s, err := connectTo(s)
	if errors.Is(err, desktop.ErrUnsupported) {
		if as, ferr := session.Fallback(s); ferr == nil {
			fmt.Fprintf(os.Stderr, "linux-use: %v; using the agent desktop in a window instead\n", err)
			return connectTo(as)
		}
	}
	return b, s, err
}

// connectTo opens the first backend that can drive s.
func connectTo(s session.Session) (desktop.Backend, session.Session, error) {
	var reasons []string
	for _, open := range backends {
		b, err := open(s)
		if err == nil {
			return b, s, nil
		}
		if !errors.Is(err, desktop.ErrUnsupported) {
			return nil, s, err
		}
		if err != desktop.ErrUnsupported {
			reasons = append(reasons, err.Error())
		}
	}
	return nil, s, fmt.Errorf("%w: no backend can drive %s: %s", desktop.ErrUnsupported, s, strings.Join(reasons, "; "))
}
