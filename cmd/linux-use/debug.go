package main

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"strconv"
	"time"

	"github.com/StivenKN/linux-use/internal/desktop"
	"github.com/StivenKN/linux-use/internal/keys"
)

// runDebug exposes raw backend operations for manual testing:
//
//	linux-use debug displays|shot FILE|move X Y|click X Y|type TEXT|key COMBO|windows|activate ID|clip [TEXT]
func runDebug(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: linux-use debug displays|shot FILE|move X Y|click X Y|type TEXT|key COMBO|windows|activate ID|clip [TEXT]")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, sess, err := connect(ctx)
	if err != nil {
		return err
	}
	defer b.Close()
	fmt.Fprintf(os.Stderr, "backend=%s session=%s\n", b.Name(), sess)

	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	switch args[0] {
	case "displays":
		ds, err := b.Displays(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(ds)
	case "shot": // shot FILE [N]: N captures 300 ms apart, timing each
		ds, err := b.Displays(ctx)
		if err != nil {
			return err
		}
		n := 1
		if len(args) > 2 {
			n = atoi(args[2])
		}
		var img *image.RGBA
		for i := range n {
			if i > 0 {
				time.Sleep(300 * time.Millisecond)
			}
			start := time.Now()
			if img, err = b.Capture(ctx, ds[0]); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "captured %v in %v\n", img.Bounds().Size(), time.Since(start))
		}
		f, err := os.Create(args[1])
		if err != nil {
			return err
		}
		defer f.Close()
		return png.Encode(f, img)
	case "move":
		return b.Move(ctx, atoi(args[1]), atoi(args[2]))
	case "click":
		if err := b.Move(ctx, atoi(args[1]), atoi(args[2])); err != nil {
			return err
		}
		if err := b.Button(ctx, desktop.Left, true); err != nil {
			return err
		}
		return b.Button(ctx, desktop.Left, false)
	case "type":
		return b.Type(ctx, args[1])
	case "key":
		syms, err := keys.ParseCombo(args[1])
		if err != nil {
			return err
		}
		for _, s := range syms {
			if err := b.Key(ctx, s, true); err != nil {
				return err
			}
		}
		for i := len(syms) - 1; i >= 0; i-- {
			if err := b.Key(ctx, syms[i], false); err != nil {
				return err
			}
		}
		return nil
	case "windows":
		w, ok := b.(desktop.Windows)
		if !ok {
			return fmt.Errorf("backend cannot list windows")
		}
		ws, err := w.Windows(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(ws)
	case "activate":
		w, ok := b.(desktop.Windows)
		if !ok {
			return fmt.Errorf("backend cannot focus windows")
		}
		return w.Activate(ctx, args[1])
	case "clip":
		c, ok := b.(desktop.Clipboard)
		if !ok {
			return fmt.Errorf("backend has no clipboard")
		}
		if len(args) > 1 {
			if err := c.WriteClipboard(ctx, args[1]); err != nil {
				return err
			}
			time.Sleep(10 * time.Second) // keep serving the selection briefly
			return nil
		}
		s, err := c.ReadClipboard(ctx)
		fmt.Println(s)
		return err
	}
	return fmt.Errorf("unknown debug command %q", args[0])
}
