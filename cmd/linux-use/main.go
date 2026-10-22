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

	"github.com/StivenKN/linux-use/internal/desktop"
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
		port := fs.Int("vnc-port", 0, "VNC viewer port (0 = first free from 5900, -1 = no viewer)")
		listen := fs.String("vnc-listen", "127.0.0.1", "VNC viewer listen address")
		nested := fs.Bool("window", false, "open the agent desktop as a window inside your current desktop")
		fs.Parse(args[1:])
		opts := session.Options{VNCPort: *port, VNCListen: *listen, Nested: *nested}
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
	if s.VNC != "" {
		host, _ := os.Hostname()
		fmt.Printf("watch it:      VNC %s\n", s.VNC)
		fmt.Printf("from another machine: ssh -L 5900:%s %s@%s, then open vnc://localhost:5900\n", s.VNC, os.Getenv("USER"), host)
	}
}

// connect finds the session and opens the first backend that can drive it.
func connect(ctx context.Context) (desktop.Backend, session.Session, error) {
	s, err := session.Find(true)
	if err != nil {
		return nil, s, err
	}
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
	return nil, s, fmt.Errorf("no backend can drive %s: %s", s, strings.Join(reasons, "; "))
}
