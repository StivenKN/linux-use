package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// handler runs one tool from raw JSON arguments. computer_batch reuses the
// same handlers so batched actions behave exactly like individual calls.
type handler func(ctx context.Context, raw json.RawMessage) (*mcp.CallToolResult, error)

// add registers a tool whose arguments decode into A. Schemas are written
// by hand (see tools.go) so names and wording match the official server.
func add[A any](s *Server, name, description, schema string, h func(context.Context, A) (*mcp.CallToolResult, error)) {
	run := func(ctx context.Context, raw json.RawMessage) (*mcp.CallToolResult, error) {
		var args A
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, fmt.Errorf("invalid arguments for %s: %w", name, err)
			}
		}
		return h(ctx, args)
	}
	s.handlers[name] = run
	s.mcp.AddTool(&mcp.Tool{
		Name:        name,
		Description: description,
		InputSchema: json.RawMessage(schema),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := run(ctx, req.Params.Arguments)
		if err != nil {
			return fail(err), nil
		}
		return res, nil
	})
}

func text(format string, a ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, a...)}}}
}

func fail(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}
