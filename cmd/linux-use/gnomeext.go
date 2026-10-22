package main

import (
	"context"
	"fmt"
	"time"

	"github.com/StivenKN/linux-use/internal/desktop/gnome"
)

// runGnomeExtension manages the GNOME Shell extension that lets linux-use
// list and focus windows on GNOME:
//
//	linux-use gnome-extension install|uninstall|status
func runGnomeExtension(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := "status"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "install":
		e, err := gnome.InstallExtension(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("GNOME Shell extension installed in %s\n", e.Dir)
		printExtension(e)
	case "uninstall":
		if err := gnome.UninstallExtension(); err != nil {
			return err
		}
		fmt.Printf("GNOME Shell extension %s uninstalled\n", gnome.ExtensionUUID)
	case "status":
		e, err := gnome.ExtensionStatus(ctx)
		if err != nil {
			return err
		}
		printExtension(e)
	default:
		return fmt.Errorf("usage: linux-use gnome-extension install|uninstall|status")
	}
	return nil
}

// printExtension reports whether the extension is active, or what to do.
func printExtension(e gnome.Extension) {
	if p := e.Problem(); p != "" {
		fmt.Printf("  ✗ %s\n", p)
		return
	}
	fmt.Printf("  ✓ active: Claude can list and focus windows, and can't type into a terminal in front\n")
}
