// Package mcpserver exposes computer use over MCP with the same tool surface
// as Claude Code's built-in macOS computer-use server (names, parameters and
// coordinate conventions), adapted to Linux. Matching the official surface
// means the model's habits carry over unchanged.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/StivenKN/linux-use/internal/computer"
	"github.com/StivenKN/linux-use/internal/desktop"
)

// Schema fragments shared by several tools, worded like the official server.
const (
	coordDesc = `"(x, y): Horizontal pixel position read directly from the most recent screenshot image, measured from the left edge. The server handles all scaling."`
	coordProp = `"coordinate":{"type":"array","items":{"type":"number"},"minItems":2,"maxItems":2,"description":` + coordDesc + `}`
	modsProp  = `"text":{"type":"string","description":"Modifier keys to hold during the click (e.g. \"shift\", \"ctrl+shift\"). Supports the same syntax as the key tool."}`
	noArgs    = `{"type":"object","properties":{},"required":[]}`
	clickArgs = `{"type":"object","properties":{` + coordProp + `,` + modsProp + `},"required":["coordinate"]}`
	gateNote  = ` The frontmost application must be in the session allowlist at the time of this call, or this tool returns an error and does nothing.`
)

type coordArgs struct {
	Coordinate []float64 `json:"coordinate"`
	Text       string    `json:"text"`
}

func (a coordArgs) point() (*computer.Point, error) {
	if len(a.Coordinate) != 2 {
		return nil, errors.New("coordinate must be [x, y]")
	}
	return &computer.Point{X: a.Coordinate[0], Y: a.Coordinate[1]}, nil
}

type textArgs struct {
	Text string `json:"text"`
}

type keyArgs struct {
	Text   string `json:"text"`
	Repeat int    `json:"repeat"`
}

type holdArgs struct {
	Text     string  `json:"text"`
	Duration float64 `json:"duration"`
}

type scrollArgs struct {
	Coordinate      []float64 `json:"coordinate"`
	ScrollDirection string    `json:"scroll_direction"`
	ScrollAmount    int       `json:"scroll_amount"`
	Text            string    `json:"text"`
}

type dragArgs struct {
	Coordinate      []float64 `json:"coordinate"`
	StartCoordinate []float64 `json:"start_coordinate"`
	Text            string    `json:"text"`
}

type zoomArgs struct {
	Region []float64 `json:"region"`
}

type waitArgs struct {
	Duration float64 `json:"duration"`
}

type appArgs struct {
	App string `json:"app"`
}

type displayArgs struct {
	Display string `json:"display"`
}

type windowArgs struct {
	Window string `json:"window"`
}

type empty struct{}

var clickTools = []struct {
	name, desc string
	button     desktop.Button
	count      int
}{
	{"left_click", "Left-click at the given coordinates.", desktop.Left, 1},
	{"double_click", "Double-click at the given coordinates. Selects a word in most text editors.", desktop.Left, 2},
	{"triple_click", "Triple-click at the given coordinates. Selects a line in most text editors.", desktop.Left, 3},
	{"right_click", "Right-click at the given coordinates. Opens a context menu in most applications.", desktop.Right, 1},
	{"middle_click", "Middle-click (scroll-wheel click) at the given coordinates. On Linux this also pastes the primary selection in many apps.", desktop.Middle, 1},
}

func (s *Server) registerTools() {
	c := s.c

	add(s, "request_access", s.requestAccessDescription(), s.requestAccessSchema(), s.requestAccess)

	add(s, "list_granted_applications",
		"List the applications currently in the session allowlist, plus the active grant flags and coordinate mode. No side effects.",
		noArgs, func(ctx context.Context, _ empty) (*mcp.CallToolResult, error) {
			return text("%s", s.grants.describe(s.agentDesktop(ctx))), nil
		})

	add(s, "screenshot",
		"Take a screenshot of the current display. The returned image is what subsequent click coordinates are relative to. On a multi-monitor setup the result names the captured monitor and lists the others (see switch_display).",
		noArgs, func(ctx context.Context, _ empty) (*mcp.CallToolResult, error) {
			return s.screenshot(ctx)
		})

	add(s, "zoom",
		"Take a higher-resolution screenshot of a specific region of the last full-screen screenshot. Use this liberally to inspect small text, button labels, or fine UI details that are hard to read in the downsampled full-screen image. IMPORTANT: Coordinates in subsequent click calls always refer to the full-screen screenshot, never the zoomed image. This tool is read-only for inspecting detail.",
		`{"type":"object","properties":{"region":{"type":"array","items":{"type":"integer"},"minItems":4,"maxItems":4,"description":"(x0, y0, x1, y1): Rectangle to zoom into, in the coordinate space of the most recent full-screen screenshot. x0,y0 = top-left, x1,y1 = bottom-right."}},"required":["region"]}`,
		func(ctx context.Context, a zoomArgs) (*mcp.CallToolResult, error) {
			return s.zoom(ctx, a)
		})

	for _, t := range clickTools {
		add(s, t.name, t.desc+gateNote, clickArgs, func(ctx context.Context, a coordArgs) (*mcp.CallToolResult, error) {
			return s.click(ctx, t.name, t.button, t.count, a)
		})
	}

	add(s, "type",
		"Type text into whatever currently has keyboard focus."+gateNote+" Newlines are supported. Any Unicode text works, independent of the keyboard layout. For keyboard shortcuts use `key` instead.",
		`{"type":"object","properties":{"text":{"type":"string","description":"Text to type."}},"required":["text"]}`,
		func(ctx context.Context, a textArgs) (*mcp.CallToolResult, error) {
			if err := s.gate(ctx); err != nil {
				return nil, err
			}
			if err := c.Type(ctx, a.Text); err != nil {
				return nil, err
			}
			return text("Typed %d characters", len([]rune(a.Text))), nil
		})

	add(s, "key",
		`Press a key or key combination (e.g. "Return", "Escape", "ctrl+a", "ctrl+shift+Tab", "super"). This is Linux: use ctrl (not cmd) for shortcuts like copy and paste; "cmd" and "super" press the Super (Windows) key. Several chords separated by spaces are pressed in sequence (e.g. "ctrl+a Delete").`+gateNote+` System-level combos (close window, switch app, lock screen, log out) require the `+"`systemKeyCombos`"+` grant — without it they return an error. All other combos work.`,
		`{"type":"object","properties":{"text":{"type":"string","description":"Modifiers joined with \"+\", e.g. \"ctrl+shift+a\". Key names follow xdotool/X keysym names (Return, Tab, BackSpace, Delete, Page_Down, F5, …) and are case-insensitive."},"repeat":{"type":"integer","minimum":1,"maximum":100,"description":"Number of times to repeat the key press. Default is 1."}},"required":["text"]}`,
		func(ctx context.Context, a keyArgs) (*mcp.CallToolResult, error) {
			if err := s.gateKeys(ctx, a.Text); err != nil {
				return nil, err
			}
			if err := c.Key(ctx, a.Text, a.Repeat); err != nil {
				return nil, err
			}
			return text("Pressed %s", a.Text), nil
		})

	add(s, "hold_key",
		"Press and hold a key or key combination for the specified duration, then release."+gateNote+" System-level combos require the `systemKeyCombos` grant.",
		`{"type":"object","properties":{"text":{"type":"string","description":"Key or chord to hold, e.g. \"space\", \"shift+Down\"."},"duration":{"type":"number","description":"Duration in seconds (0–100)."}},"required":["text","duration"]}`,
		func(ctx context.Context, a holdArgs) (*mcp.CallToolResult, error) {
			if err := s.gateKeys(ctx, a.Text); err != nil {
				return nil, err
			}
			d, err := seconds(a.Duration)
			if err != nil {
				return nil, err
			}
			if err := c.HoldKey(ctx, a.Text, d); err != nil {
				return nil, err
			}
			return text("Held %s for %gs", a.Text, a.Duration), nil
		})

	add(s, "scroll",
		"Scroll at the given coordinates."+gateNote,
		`{"type":"object","properties":{`+coordProp+`,"scroll_direction":{"type":"string","enum":["up","down","left","right"],"description":"Direction to scroll."},"scroll_amount":{"type":"integer","minimum":0,"maximum":100,"description":"Number of scroll ticks."},"text":{"type":"string","description":"Modifier keys to hold while scrolling (e.g. \"ctrl\" to zoom, \"shift\" to scroll sideways)."}},"required":["coordinate","scroll_direction","scroll_amount"]}`,
		func(ctx context.Context, a scrollArgs) (*mcp.CallToolResult, error) {
			return s.scroll(ctx, a)
		})

	add(s, "left_click_drag",
		"Press, move to target, and release."+gateNote,
		`{"type":"object","properties":{"coordinate":{"type":"array","items":{"type":"number"},"minItems":2,"maxItems":2,"description":"(x, y) end point: Horizontal pixel position read directly from the most recent screenshot image, measured from the left edge. The server handles all scaling."},"start_coordinate":{"type":"array","items":{"type":"number"},"minItems":2,"maxItems":2,"description":"(x, y) start point. If omitted, drags from the current cursor position. Horizontal pixel position read directly from the most recent screenshot image, measured from the left edge. The server handles all scaling."}},"required":["coordinate"]}`,
		func(ctx context.Context, a dragArgs) (*mcp.CallToolResult, error) {
			return s.drag(ctx, a)
		})

	add(s, "mouse_move",
		"Move the mouse cursor without clicking. Useful for triggering hover states."+gateNote,
		`{"type":"object","properties":{`+coordProp+`},"required":["coordinate"]}`,
		func(ctx context.Context, a coordArgs) (*mcp.CallToolResult, error) {
			p, err := a.point()
			if err != nil {
				return nil, err
			}
			if err := s.gate(ctx); err != nil {
				return nil, err
			}
			if err := c.Move(ctx, *p); err != nil {
				return nil, err
			}
			return text("Moved to (%g, %g)", p.X, p.Y), nil
		})

	add(s, "left_mouse_down",
		"Press the left mouse button at the current cursor position and leave it held."+gateNote+" Use mouse_move first to position the cursor. Call left_mouse_up to release. Errors if the button is already held.",
		noArgs, func(ctx context.Context, _ empty) (*mcp.CallToolResult, error) {
			if err := s.gate(ctx); err != nil {
				return nil, err
			}
			if err := c.MouseDown(ctx); err != nil {
				return nil, err
			}
			return text("Left button down"), nil
		})

	add(s, "left_mouse_up",
		"Release the left mouse button at the current cursor position."+gateNote+" Pairs with left_mouse_down. Safe to call even if the button is not currently held.",
		noArgs, func(ctx context.Context, _ empty) (*mcp.CallToolResult, error) {
			if err := c.MouseUp(ctx); err != nil {
				return nil, err
			}
			return text("Left button up"), nil
		})

	add(s, "cursor_position",
		"Get the current mouse cursor position. Returns image-pixel coordinates relative to the most recent screenshot. Wayland does not let apps read the pointer, so this is where linux-use last moved it.",
		noArgs, func(ctx context.Context, _ empty) (*mcp.CallToolResult, error) {
			x, y, err := c.CursorPosition(ctx)
			if err != nil {
				return nil, err
			}
			return text("(%d, %d)", x, y), nil
		})

	add(s, "open_application",
		"Launch an application and bring its window to the front. Accepts a display name (\"Firefox\", \"Files\"), a desktop entry id (\"org.gnome.Nautilus\"), or a command (\"foot\"). The target must already be in the session allowlist — call request_access first.",
		`{"type":"object","properties":{"app":{"type":"string","description":"Display name (e.g. \"Firefox\"), desktop entry id (e.g. \"org.gnome.Nautilus\"), or command."}},"required":["app"]}`,
		func(ctx context.Context, a appArgs) (*mcp.CallToolResult, error) {
			if err := s.gateApp(ctx, a.App); err != nil {
				return nil, err
			}
			msg, err := c.OpenApplication(ctx, a.App)
			if err != nil {
				return nil, err
			}
			return text("%s", msg), nil
		})

	add(s, "list_windows",
		"List open windows with their ids, titles and app ids, and which one is focused. Use activate_window to bring one to the front.",
		noArgs, func(ctx context.Context, _ empty) (*mcp.CallToolResult, error) {
			ws, err := c.Windows(ctx)
			if err != nil {
				return nil, err
			}
			return text("%s", formatWindows(ws)), nil
		})

	add(s, "activate_window",
		"Bring a window to the front and focus it, by id from list_windows.",
		`{"type":"object","properties":{"window":{"type":"string","description":"Window id from list_windows."}},"required":["window"]}`,
		func(ctx context.Context, a windowArgs) (*mcp.CallToolResult, error) {
			if err := s.gateWindow(ctx, a.Window); err != nil {
				return nil, err
			}
			if err := c.Activate(ctx, a.Window); err != nil {
				return nil, err
			}
			return text("Activated window %s", a.Window), nil
		})

	add(s, "switch_display",
		`Switch which monitor subsequent screenshots capture. Use this when the application you need is on a different monitor than the one shown. The screenshot tool tells you which monitor it captured and lists other attached monitors by name — pass one of those names here. After switching, call screenshot to see the new monitor. Pass "auto" to return to the first monitor.`,
		`{"type":"object","properties":{"display":{"type":"string","description":"Monitor name from the screenshot note (e.g. \"eDP-1\", \"HDMI-A-1\"), its number, or \"auto\"."}},"required":["display"]}`,
		func(ctx context.Context, a displayArgs) (*mcp.CallToolResult, error) {
			if a.Display == "auto" {
				a.Display = "1"
			}
			d, err := c.SelectDisplay(ctx, a.Display)
			if err != nil {
				return nil, err
			}
			return text("Switched to display %d (%s, %dx%d). Take a screenshot to see it.", d.ID, d.Name, d.Width, d.Height), nil
		})

	add(s, "read_clipboard",
		"Read the current clipboard contents as text. Requires the `clipboardRead` grant.",
		noArgs, func(ctx context.Context, _ empty) (*mcp.CallToolResult, error) {
			if !s.grants.allowClipboardRead(s.agentDesktop(ctx)) {
				return nil, errors.New("clipboard read not granted; call request_access with clipboardRead: true")
			}
			t, err := c.ReadClipboard(ctx)
			if err != nil {
				return nil, err
			}
			return text("%s", t), nil
		})

	add(s, "write_clipboard",
		"Write text to the clipboard. Requires the `clipboardWrite` grant.",
		`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`,
		func(ctx context.Context, a textArgs) (*mcp.CallToolResult, error) {
			if !s.grants.allowClipboardWrite(s.agentDesktop(ctx)) {
				return nil, errors.New("clipboard write not granted; call request_access with clipboardWrite: true")
			}
			if err := c.WriteClipboard(ctx, a.Text); err != nil {
				return nil, err
			}
			return text("Wrote %d characters to the clipboard", len([]rune(a.Text))), nil
		})

	add(s, "wait",
		"Wait for a specified duration.",
		`{"type":"object","properties":{"duration":{"type":"number","description":"Duration in seconds (0–100)."}},"required":["duration"]}`,
		func(ctx context.Context, a waitArgs) (*mcp.CallToolResult, error) {
			d, err := seconds(a.Duration)
			if err != nil {
				return nil, err
			}
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return text("Waited %gs", a.Duration), nil
		})

	add(s, "computer_batch", batchDescription, batchSchema, s.batch)
}

func seconds(v float64) (time.Duration, error) {
	if v < 0 || v > 100 {
		return 0, fmt.Errorf("duration must be between 0 and 100 seconds, got %g", v)
	}
	return time.Duration(v * float64(time.Second)), nil
}

func formatWindows(ws []desktop.Window) string {
	if len(ws) == 0 {
		return "No windows are open."
	}
	var b strings.Builder
	for _, w := range ws {
		fmt.Fprintf(&b, "%s\t%s\t%q", w.ID, w.AppID, w.Title)
		if w.Focused {
			b.WriteString("\t(focused)")
		}
		if w.Minimized {
			b.WriteString("\t(minimized)")
		}
		b.WriteByte('\n')
	}
	return b.String()
}
