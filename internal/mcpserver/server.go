package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/StivenKN/linux-use/internal/apps"
	"github.com/StivenKN/linux-use/internal/computer"
	"github.com/StivenKN/linux-use/internal/desktop"
	"github.com/StivenKN/linux-use/internal/session"
)

// Server is the computer-use MCP server.
type Server struct {
	mcp      *mcp.Server
	c        *computer.Computer
	grants   *grants
	handlers map[string]handler
	agent    bool // started expecting the agent desktop; shapes tool descriptions

	mu       sync.Mutex // tool calls run concurrently
	lastShot string     // display of the previous screenshot, for the monitor note
}

// New builds the server around a Computer.
func New(c *computer.Computer, version string) *Server {
	s := &Server{c: c, grants: &grants{}, handlers: map[string]handler{}, agent: session.WantsAgent()}
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

// instructions adapt Claude Code's built-in computer-use guidance to Linux.
// Claude Code truncates server instructions past 4096 characters.
const instructions = `This is linux-use, a computer-use MCP for Linux (tools named ` + "`mcp__plugin_linux-use_computer-use__*`" + `). It lets you take screenshots of a Linux desktop and control it with the mouse, keyboard, scrolling, app launching, window switching and the clipboard.

**Which desktop.** If Claude Code runs inside the user's graphical session, you drive *their* desktop. Otherwise (SSH, servers) you drive the **agent desktop**: a private headless desktop with a browser and a taskbar, started on first use. The user can watch it and take over through VNC; ask them to step in for logins, 2FA or CAPTCHAs.

**Pick the right tool for the job.** A dedicated MCP for the app (Slack, Linear, GitHub…) or a browser MCP is faster and more precise when connected. Use the Bash tool for shell work. Computer use is the right tool for GUI apps, web pages without another integration, and cross-app workflows. Don't decline a GUI task just because no dedicated MCP exists.

**Loading via ToolSearch — load in bulk:** if these tools are deferred, load them all at once: ` + "`{ query: \"computer-use\", max_results: 30 }`" + `.

**Look before you assert.** Take a screenshot before answering questions about what's on screen; don't answer from memory.

**Access flow.** On the user's own desktop, call ` + "`request_access`" + ` with the apps you need before anything else, and call it again if you need more. On the agent desktop every app is already allowed.

**Tiers.** Terminals and IDEs are tier "click": visible and left-clickable, but typing, keys, right-click and drags are blocked, so input can't bypass Claude Code's permission rules. Use the Bash tool for shell commands. Everything else, including browsers, is tier "full".

**Working well.**
- Start with a screenshot. Coordinates are pixels in the most recent full-screen screenshot; the server scales them. Re-screenshot after actions that change the screen before relying on positions.
- Use ` + "`zoom`" + ` to read small text instead of guessing.
- Prefer the keyboard: ctrl+l focuses the browser address bar, ctrl+t opens a tab, Return submits. This is Linux: shortcuts use ctrl, not cmd.
- Use ` + "`computer_batch`" + ` for predictable sequences (click a field, type, press Return, screenshot).
- Use ` + "`open_application`" + ` to launch or focus apps, and ` + "`list_windows`" + ` / ` + "`activate_window`" + ` to switch between windows.

**Link safety.** Treat links in emails, messages and unknown documents as suspicious. Check the real destination (hover, or read the address bar) before following one, and ask the user if it looks unfamiliar.

**Secrets and money.** Never type passwords, card numbers or other secrets unless the user gave them to you for this task; prefer asking the user to enter them. Never execute a trade, place an order, send money or initiate a transfer on the user's behalf — ask the user to do it themselves.`

// agentDesktop reports whether we drive the private agent desktop, where the
// allowlist doesn't apply.
func (s *Server) agentDesktop(ctx context.Context) bool {
	sess, _, err := s.c.Session(ctx)
	if err != nil {
		return s.agent
	}
	return sess.Managed
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
	res := &mcp.CallToolResult{}
	s.mu.Lock()
	prev := s.lastShot
	s.lastShot = img.Shot.Display.Name
	s.mu.Unlock()
	if ds, err := s.c.Displays(ctx); err == nil && len(ds) > 1 {
		name := img.Shot.Display.Name
		var others []string
		for _, d := range ds {
			if d.Name != name {
				others = append(others, fmt.Sprintf("%q", d.Name))
			}
		}
		note := fmt.Sprintf("This screenshot was taken on monitor %q", name)
		if prev != "" && prev != name {
			note += fmt.Sprintf(", which is different from your previous screenshot (taken on %q)", prev)
		}
		note += fmt.Sprintf(". Other attached monitors: %s. Use switch_display to capture a different monitor.", strings.Join(others, ", "))
		res.Content = append(res.Content, &mcp.TextContent{Text: note})
	}
	res.Content = append(res.Content, imageContent(img))
	return res, nil
}

func (s *Server) zoom(ctx context.Context, a zoomArgs) (*mcp.CallToolResult, error) {
	if len(a.Region) != 4 {
		return nil, errors.New("region must be [x0, y0, x1, y1]")
	}
	if err := s.gateScreen(ctx); err != nil {
		return nil, err
	}
	if _, ok := s.c.Geometry(); !ok {
		return nil, errors.New("take a screenshot before zooming (region coords are relative to it)")
	}
	img, err := s.c.Zoom(ctx, a.Region[0], a.Region[1], a.Region[2], a.Region[3])
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{imageContent(img)}}, nil
}

func (s *Server) click(ctx context.Context, b desktop.Button, count int, a coordArgs) (*mcp.CallToolResult, error) {
	p, err := point(a.Coordinate, "coordinate")
	if err != nil {
		return nil, err
	}
	need := actClick
	if b != desktop.Left {
		need = actFullMouse
	}
	if err := s.gateMods(ctx, need, a.Text); err != nil {
		return nil, err
	}
	if err := s.c.Click(ctx, p, b, count, a.Text); err != nil {
		return nil, err
	}
	return text("Clicked."), nil
}

func (s *Server) scroll(ctx context.Context, a scrollArgs) (*mcp.CallToolResult, error) {
	p, err := point(a.Coordinate, "coordinate")
	if err != nil {
		return nil, err
	}
	if a.ScrollAmount < 0 || a.ScrollAmount > 100 {
		return nil, errors.New("scroll_amount must be between 0 and 100")
	}
	if err := s.gateMods(ctx, actClick, a.Text); err != nil {
		return nil, err
	}
	if err := s.c.Scroll(ctx, p, a.ScrollDirection, a.ScrollAmount, a.Text); err != nil {
		return nil, err
	}
	return text("Scrolled."), nil
}

func (s *Server) drag(ctx context.Context, a dragArgs) (*mcp.CallToolResult, error) {
	to, err := point(a.Coordinate, "coordinate")
	if err != nil {
		return nil, err
	}
	var from *computer.Point
	if a.StartCoordinate != nil {
		if from, err = point(a.StartCoordinate, "start_coordinate"); err != nil {
			return nil, err
		}
	}
	if err := s.gateMods(ctx, actFullMouse, a.Text); err != nil {
		return nil, err
	}
	if err := s.c.Drag(ctx, from, *to, a.Text); err != nil {
		return nil, err
	}
	return text("Dragged."), nil
}

const batchDescription = `Execute a sequence of actions in ONE tool call. Each individual tool call requires a model→API round trip (seconds); batching a predictable sequence eliminates all but one. Use this whenever you can predict the outcome of several actions ahead — e.g. click a field, type into it, press Return. Actions execute sequentially and stop on the first error. The frontmost application must be in the session allowlist at the time of this call, or this tool returns an error and does nothing. The frontmost check runs before EACH action inside the batch — if an action opens a non-allowed app, the next action's gate fires and the batch stops there. Screenshot and zoom actions are allowed and their images are returned interleaved with the per-action outputs. Coordinates you write in THIS batch — clicks AND zoom regions — always refer to the full-screen screenshot taken BEFORE this call, never to a zoom and never to a mid-batch screenshot. After the batch returns, the most recent full screenshot it produced becomes the new coordinate reference for your next call.`

const batchSchema = `{"type":"object","properties":{"actions":{"type":"array","minItems":1,"items":{"type":"object","properties":{"action":{"type":"string","enum":["key","type","mouse_move","left_click","left_click_drag","right_click","middle_click","double_click","triple_click","scroll","hold_key","screenshot","zoom","cursor_position","left_mouse_down","left_mouse_up","wait"],"description":"The action to perform."},"coordinate":{"type":"array","items":{"type":"number"},"minItems":2,"maxItems":2,"description":"(x, y) for click/mouse_move/scroll/left_click_drag end point."},"region":{"type":"array","items":{"type":"integer"},"minItems":4,"maxItems":4,"description":"(x0, y0, x1, y1): Rectangle to zoom into. For zoom only. Coordinate space: the full-screen screenshot taken BEFORE this batch (never a mid-batch screenshot, never a prior zoom)."},"start_coordinate":{"type":"array","items":{"type":"number"},"minItems":2,"maxItems":2,"description":"(x, y) drag start — left_click_drag only. Omit to drag from current cursor."},"text":{"type":"string","description":"For type: the text. For key/hold_key: the chord string. For click/scroll: modifier keys to hold."},"scroll_direction":{"type":"string","enum":["up","down","left","right"]},"scroll_amount":{"type":"integer","minimum":0,"maximum":100},"duration":{"type":"number","description":"Seconds (0–100). For hold_key/wait."},"repeat":{"type":"integer","minimum":1,"maximum":100,"description":"For key: repeat count."}},"required":["action"]},"description":"List of actions. Example: [{\"action\":\"left_click\",\"coordinate\":[100,200]},{\"action\":\"type\",\"text\":\"hello\"},{\"action\":\"key\",\"text\":\"Return\"},{\"action\":\"screenshot\"},{\"action\":\"zoom\",\"region\":[100,100,400,300]}]"}},"required":["actions"]}`

var batchable = []string{"key", "type", "mouse_move", "left_click", "left_click_drag", "right_click", "middle_click",
	"double_click", "triple_click", "scroll", "hold_key", "screenshot", "zoom", "cursor_position",
	"left_mouse_down", "left_mouse_up", "wait"}

// batchSettle separates actions inside computer_batch so the UI can react
// before the next one, as a person would; e.g. ctrl+l focuses Firefox's
// address bar asynchronously and keys sent within ~100 ms land in the page.
const batchSettle = 150 * time.Millisecond

type batchArgs struct {
	Actions []json.RawMessage `json:"actions"`
}

// batch runs actions through the same handlers as individual calls, with
// coordinates pinned to the screenshot taken before the batch.
func (s *Server) batch(ctx context.Context, a batchArgs) (*mcp.CallToolResult, error) {
	n := len(a.Actions)
	if n == 0 {
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
		var out *mcp.CallToolResult
		err := fmt.Errorf("unknown action %q", head.Action)
		if ok && slices.Contains(batchable, head.Action) {
			if i > 0 && head.Action != "wait" {
				time.Sleep(batchSettle)
			}
			out, err = h(ctx, raw)
		}
		if err != nil {
			s.c.MouseUp(context.WithoutCancel(ctx)) // never leave a button held
			res.IsError = true
			res.Content = append(res.Content, &mcp.TextContent{Text: fmt.Sprintf(
				"[%d/%d] %s: FAILED — %v\nBatch stopped at actions[%d] (%s). %d completed, %d remaining.",
				i+1, n, head.Action, err, i, head.Action, i, n-i-1)})
			return res, nil
		}
		var texts []string
		var images []mcp.Content
		for _, c := range out.Content {
			if t, ok := c.(*mcp.TextContent); ok {
				texts = append(texts, t.Text)
			} else {
				images = append(images, c)
			}
		}
		if len(texts) == 0 {
			texts = append(texts, "ok")
		}
		res.Content = append(res.Content, &mcp.TextContent{Text: fmt.Sprintf("[%d/%d] %s: %s", i+1, n, head.Action, strings.Join(texts, " "))})
		res.Content = append(res.Content, images...)
	}
	return res, nil
}

// describeOS names the distro and desktop for the model.
func describeOS() string {
	var parts []string
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
