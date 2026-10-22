package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/StivenKN/linux-use/internal/apps"
	"github.com/StivenKN/linux-use/internal/computer"
	"github.com/StivenKN/linux-use/internal/desktop"
)

// Server is the computer-use MCP server.
type Server struct {
	mcp      *mcp.Server
	c        *computer.Computer
	grants   *grants
	handlers map[string]handler
}

// New builds the server around a Computer.
func New(c *computer.Computer, version string) *Server {
	s := &Server{c: c, grants: &grants{}, handlers: map[string]handler{}}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "linux-use", Title: "Linux computer use", Version: version},
		&mcp.ServerOptions{Instructions: instructions})
	s.registerTools()
	return s
}

// Run serves MCP over stdio until the client disconnects.
func (s *Server) Run(ctx context.Context) error {
	defer s.c.Close()
	return s.mcp.Run(ctx, &mcp.StdioTransport{})
}

const instructions = `You can see and control a Linux desktop: take screenshots, move and click the mouse, type, press keys, scroll, drag, open apps, switch windows, and use the clipboard.

How to work:
- Start with a screenshot. Coordinates are pixels in the most recent full-screen screenshot; the server scales them to the real screen. After each meaningful action, take a screenshot to confirm it worked before moving on.
- Use zoom to read small text instead of guessing.
- Prefer keyboard shortcuts and typing over hunting for small targets: ctrl+l focuses a browser's address bar, ctrl+t opens a tab, Return submits. This is Linux, so shortcuts use ctrl, not cmd.
- Use computer_batch for predictable sequences (click a field, type, press Return, screenshot) to save round trips.
- Use open_application to launch apps by name, and list_windows / activate_window to move between them.
- If the target is the linux-use "agent desktop" (a private headless desktop, typical on servers), you are free to use any app in it. The user can watch and take over through VNC. Ask them to step in for logins, CAPTCHAs, or anything needing their credentials.
- On the user's own desktop, call request_access first for the apps you need, and only touch those apps.
- Never enter passwords, payment details or other secrets unless the user explicitly provided them for this task.`

// agentDesktop reports whether we drive the private agent desktop, where the
// allowlist doesn't apply.
func (s *Server) agentDesktop(ctx context.Context) bool {
	sess, _, err := s.c.Session(ctx)
	return err == nil && sess.Managed
}

func imageContent(img computer.Image) *mcp.ImageContent {
	return &mcp.ImageContent{Data: img.Data, MIMEType: img.MIME}
}

func (s *Server) screenshot(ctx context.Context) (*mcp.CallToolResult, error) {
	if err := s.gateScreen(ctx); err != nil {
		return nil, err
	}
	img, err := s.c.Screenshot(ctx)
	if err != nil {
		return nil, err
	}
	res := &mcp.CallToolResult{Content: []mcp.Content{imageContent(img)}}
	if ds, err := s.c.Displays(ctx); err == nil && len(ds) > 1 {
		var others []string
		for _, d := range ds {
			if d.Name != img.Shot.Display.Name {
				others = append(others, fmt.Sprintf("%q (%dx%d)", d.Name, d.Width, d.Height))
			}
		}
		res.Content = append(res.Content, &mcp.TextContent{Text: fmt.Sprintf(
			"Captured display %q (%dx%d). Other displays: %s. Use switch_display to capture another.",
			img.Shot.Display.Name, img.Shot.Display.Width, img.Shot.Display.Height, strings.Join(others, ", "))})
	}
	return res, nil
}

// gateScreen requires a grant before looking at the user's own screen.
func (s *Server) gateScreen(ctx context.Context) error {
	if s.agentDesktop(ctx) {
		return nil
	}
	s.grants.mu.Lock()
	n := len(s.grants.apps)
	s.grants.mu.Unlock()
	if n == 0 {
		return errors.New("no applications are granted yet; call request_access first")
	}
	return nil
}

func (s *Server) zoom(ctx context.Context, a zoomArgs) (*mcp.CallToolResult, error) {
	if len(a.Region) != 4 {
		return nil, errors.New("region must be [x0, y0, x1, y1]")
	}
	if err := s.gateScreen(ctx); err != nil {
		return nil, err
	}
	img, err := s.c.Zoom(ctx, a.Region[0], a.Region[1], a.Region[2], a.Region[3])
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{imageContent(img)}}, nil
}

func (s *Server) click(ctx context.Context, name string, b desktop.Button, count int, a coordArgs) (*mcp.CallToolResult, error) {
	p, err := a.point()
	if err != nil {
		return nil, err
	}
	if err := s.gate(ctx); err != nil {
		return nil, err
	}
	if err := s.c.Click(ctx, p, b, count, a.Text); err != nil {
		return nil, err
	}
	return text("%s at (%g, %g)", strings.ReplaceAll(name, "_", " "), p.X, p.Y), nil
}

func (s *Server) scroll(ctx context.Context, a scrollArgs) (*mcp.CallToolResult, error) {
	p, err := coordArgs{Coordinate: a.Coordinate}.point()
	if err != nil {
		return nil, err
	}
	if a.ScrollAmount < 0 || a.ScrollAmount > 100 {
		return nil, errors.New("scroll_amount must be between 0 and 100")
	}
	if err := s.gate(ctx); err != nil {
		return nil, err
	}
	if err := s.c.Scroll(ctx, p, a.ScrollDirection, a.ScrollAmount, a.Text); err != nil {
		return nil, err
	}
	return text("Scrolled %s %d at (%g, %g)", a.ScrollDirection, a.ScrollAmount, p.X, p.Y), nil
}

func (s *Server) drag(ctx context.Context, a dragArgs) (*mcp.CallToolResult, error) {
	to, err := coordArgs{Coordinate: a.Coordinate}.point()
	if err != nil {
		return nil, err
	}
	var from *computer.Point
	if a.StartCoordinate != nil {
		if from, err = (coordArgs{Coordinate: a.StartCoordinate}).point(); err != nil {
			return nil, fmt.Errorf("start_coordinate: %w", err)
		}
	}
	if err := s.gate(ctx); err != nil {
		return nil, err
	}
	if err := s.c.Drag(ctx, from, *to, a.Text); err != nil {
		return nil, err
	}
	return text("Dragged to (%g, %g)", to.X, to.Y), nil
}

const batchDescription = `Execute a sequence of actions in ONE tool call. Each individual tool call requires a model→API round trip (seconds); batching a predictable sequence eliminates all but one. Use this whenever you can predict the outcome of several actions ahead — e.g. click a field, type into it, press Return. Actions execute sequentially and stop on the first error. The frontmost application must be in the session allowlist at the time of this call, or this tool returns an error and does nothing. The frontmost check runs before EACH action inside the batch — if an action opens a non-allowed app, the next action's gate fires and the batch stops there. Screenshot and zoom actions are allowed and their images are returned interleaved with the per-action outputs. Coordinates you write in THIS batch — clicks AND zoom regions — always refer to the full-screen screenshot taken BEFORE this call, never to a zoom and never to a mid-batch screenshot. After the batch returns, the most recent full screenshot it produced becomes the new coordinate reference for your next call.`

const batchSchema = `{"type":"object","properties":{"actions":{"type":"array","minItems":1,"items":{"type":"object","properties":{"action":{"type":"string","enum":["key","type","mouse_move","left_click","left_click_drag","right_click","middle_click","double_click","triple_click","scroll","hold_key","screenshot","zoom","cursor_position","left_mouse_down","left_mouse_up","wait"],"description":"The action to perform."},"coordinate":{"type":"array","items":{"type":"number"},"minItems":2,"maxItems":2,"description":"(x, y) for click/mouse_move/scroll/left_click_drag end point."},"region":{"type":"array","items":{"type":"integer"},"minItems":4,"maxItems":4,"description":"(x0, y0, x1, y1): Rectangle to zoom into. For zoom only. Coordinate space: the full-screen screenshot taken BEFORE this batch (never a mid-batch screenshot, never a prior zoom)."},"start_coordinate":{"type":"array","items":{"type":"number"},"minItems":2,"maxItems":2,"description":"(x, y) drag start — left_click_drag only. Omit to drag from current cursor."},"text":{"type":"string","description":"For type: the text. For key/hold_key: the chord string. For click/scroll: modifier keys to hold."},"scroll_direction":{"type":"string","enum":["up","down","left","right"]},"scroll_amount":{"type":"integer","minimum":0,"maximum":100},"duration":{"type":"number","description":"Seconds (0–100). For hold_key/wait."},"repeat":{"type":"integer","minimum":1,"maximum":100,"description":"For key: repeat count."}},"required":["action"]},"description":"List of actions. Example: [{\"action\":\"left_click\",\"coordinate\":[100,200]},{\"action\":\"type\",\"text\":\"hello\"},{\"action\":\"key\",\"text\":\"Return\"},{\"action\":\"screenshot\"},{\"action\":\"zoom\",\"region\":[100,100,400,300]}]"}},"required":["actions"]}`

var batchable = []string{"key", "type", "mouse_move", "left_click", "left_click_drag", "right_click", "middle_click",
	"double_click", "triple_click", "scroll", "hold_key", "screenshot", "zoom", "cursor_position",
	"left_mouse_down", "left_mouse_up", "wait"}

// batchSettle separates actions inside computer_batch.
const batchSettle = 150 * time.Millisecond

type batchArgs struct {
	Actions []json.RawMessage `json:"actions"`
}

// batch runs actions through the same handlers as individual calls, with
// coordinates pinned to the screenshot taken before the batch.
func (s *Server) batch(ctx context.Context, a batchArgs) (*mcp.CallToolResult, error) {
	if len(a.Actions) == 0 {
		return nil, errors.New("actions must not be empty")
	}
	unpin := s.c.Pin()
	defer unpin()
	res := &mcp.CallToolResult{}
	for i, raw := range a.Actions {
		var head struct {
			Action string `json:"action"`
		}
		json.Unmarshal(raw, &head)
		h, ok := s.handlers[head.Action]
		if !ok || !slices.Contains(batchable, head.Action) {
			return batchError(res, i, head.Action, fmt.Errorf("unknown action %q", head.Action)), nil
		}
		if i > 0 && head.Action != "wait" {
			// Give the UI time to react before the next action, as a person
			// would: e.g. ctrl+l focuses Firefox's address bar asynchronously.
			time.Sleep(batchSettle)
		}
		out, err := h(ctx, raw)
		if err != nil {
			return batchError(res, i, head.Action, err), nil
		}
		for _, c := range out.Content {
			if t, ok := c.(*mcp.TextContent); ok {
				res.Content = append(res.Content, &mcp.TextContent{Text: fmt.Sprintf("[%d] %s: %s", i+1, head.Action, t.Text)})
			} else {
				res.Content = append(res.Content, &mcp.TextContent{Text: fmt.Sprintf("[%d] %s:", i+1, head.Action)}, c)
			}
		}
	}
	return res, nil
}

func batchError(res *mcp.CallToolResult, i int, action string, err error) *mcp.CallToolResult {
	res.IsError = true
	res.Content = append(res.Content, &mcp.TextContent{Text: fmt.Sprintf("[%d] %s failed: %v. Stopped; later actions were not run.", i+1, action, err)})
	return res
}

// describeOS names the distro and desktop for the model.
func describeOS() string {
	parts := []string{}
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
				parts = append(parts, strings.Trim(v, `"`))
			}
		}
	}
	if d := os.Getenv("XDG_CURRENT_DESKTOP"); d != "" {
		parts = append(parts, d)
	}
	return strings.Join(parts, ", ")
}

// fileManager returns the name of the installed file manager, if any.
func fileManager() string {
	for _, id := range []string{"org.gnome.Nautilus", "org.kde.dolphin", "nemo", "thunar", "pcmanfm", "pcmanfm-qt", "caja"} {
		if a, ok := apps.Find(id); ok {
			return a.Name
		}
	}
	return "none installed"
}
