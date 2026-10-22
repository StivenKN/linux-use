package gnome

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/StivenKN/linux-use/internal/desktop"
)

var errNoExtension = errors.New("listing and focusing windows on GNOME needs the linux-use GNOME Shell extension, " +
	"which isn't active in this session: run /linux-use:setup")

// Windows lists app windows through the linux-use GNOME Shell extension.
func (b *Backend) Windows(ctx context.Context) ([]desktop.Window, error) {
	var ws []struct {
		ID                 uint64
		Title, App         string
		Focused, Minimized bool
	}
	if err := b.bus.Object(shellName, extPath).CallWithContext(ctx, extIface+".Windows", 0).Store(&ws); err != nil {
		return nil, b.extErr(ctx, err)
	}
	out := make([]desktop.Window, len(ws))
	for i, w := range ws {
		out[i] = desktop.Window{
			ID:    strconv.FormatUint(w.ID, 10),
			Title: w.Title,
			// GNOME names apps by desktop file id; Wayland app ids, which
			// the rest of linux-use expects, lack the suffix.
			AppID:     strings.TrimSuffix(w.App, ".desktop"),
			Focused:   w.Focused,
			Minimized: w.Minimized,
		}
	}
	return out, nil
}

// Activate switches to the window's workspace, unminimizes and focuses it.
func (b *Backend) Activate(ctx context.Context, id string) error {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return fmt.Errorf("no window with id %q", id)
	}
	if err := b.bus.Object(shellName, extPath).CallWithContext(ctx, extIface+".Activate", 0, n).Err; err != nil {
		return b.extErr(ctx, err)
	}
	return nil
}

// extErr explains a failed extension call: the extension isn't active, an
// older version is, or err is the answer.
func (b *Backend) extErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return err
	}
	v, verr := runningVersion(ctx, b.bus)
	switch {
	case verr != nil:
		return errNoExtension
	case v != extVersion:
		return fmt.Errorf("this session runs version %d of the linux-use GNOME Shell extension, but linux-use needs version %d: run /linux-use:setup", v, extVersion)
	}
	return err
}
