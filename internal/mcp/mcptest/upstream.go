// Package mcptest provides a real, in-process MCP server (the official Go SDK
// over the Streamable HTTP transport) for the gateway's end-to-end tests.
//
// Nothing here mocks the protocol: the gateway's MCP client speaks to this
// server over HTTP exactly as it would to a production MCP server, so the
// tests exercise real initialize / tools/list / tools/call exchanges.
package mcptest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Upstream is a running test MCP server exposing echo / add / fail.
type Upstream struct {
	Server *httptest.Server
	Calls  *Calls
}

// Calls counts executions per tool.
type Calls struct {
	total   int64
	perTool map[string]int64
	lastArg map[string]map[string]any
}

func (c *Calls) Total() int64          { return c.total }
func (c *Calls) For(tool string) int64 { return c.perTool[tool] }

// Log returns the arguments of the most recent call per tool.
func (c *Calls) Log() map[string]map[string]any { return c.lastArg }

// AddTool registers a tool whose handler is invoked for every call. The
// gateway tests can count executions this way without instrumenting the code
// under test.
func New() *Upstream {
	calls := &Calls{perTool: map[string]int64{}, lastArg: map[string]map[string]any{}}
	server := newToolServer(calls)
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.Handle("/", handler)
	return &Upstream{Server: httptest.NewServer(mux), Calls: calls}
}

// newToolServer builds the MCP server with echo / add / fail wired to calls.
func newToolServer(calls *Calls) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "test-upstream", Version: "1.0.0"}, nil)

	echo := func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		calls.total++
		calls.perTool["echo"]++
		args := decodeArgs(req)
		calls.lastArg["echo"] = args
		text, _ := args["text"].(string)
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: "echo: " + text}},
		}, nil
	}
	add := func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		calls.total++
		calls.perTool["add"]++
		args := decodeArgs(req)
		calls.lastArg["add"] = args
		a, _ := args["a"].(float64)
		b, _ := args["b"].(float64)
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf("%v", a+b)}},
		}, nil
	}
	fail := func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		calls.total++
		calls.perTool["fail"]++
		return &sdk.CallToolResult{
			IsError: true,
			Content: []sdk.Content{&sdk.TextContent{Text: "upstream tool failure"}},
		}, nil
	}

	server.AddTool(&sdk.Tool{
		Name:        "echo",
		Description: "Echo the provided text back",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{"type": "string", "description": "text to echo"},
			},
			"required": []string{"text"},
		},
	}, echo)

	server.AddTool(&sdk.Tool{
		Name:        "add",
		Description: "Add two numbers",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"a": map[string]any{"type": "number"},
				"b": map[string]any{"type": "number"},
			},
			"required": []string{"a", "b"},
		},
	}, add)

	server.AddTool(&sdk.Tool{
		Name:        "fail",
		Description: "Always fails (error path)",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, fail)

	return server
}

func (u *Upstream) Close() { u.Server.Close() }

// NewSSE starts the same tool set behind the LEGACY HTTP+SSE transport, so the
// gateway's sse client path is exercised for real.
func NewSSE() *Upstream {
	base := New()
	// Rebuild the handler over the same tool definitions using the SSE
	// transport. NewSSEHandler owns its own session map, so it must be created
	// with the server that was already populated above.
	server := newToolServer(base.Calls)
	handler := sdk.NewSSEHandler(func(*http.Request) *sdk.Server { return server }, nil)
	base.Server.Close()
	base.Server = httptest.NewServer(handler)
	return base
}

// Start registers the server with the test's cleanup and returns it.
func Start(t *testing.T) *Upstream {
	t.Helper()
	u := New()
	t.Cleanup(u.Close)
	return u
}

func decodeArgs(req *sdk.CallToolRequest) map[string]any {
	out := map[string]any{}
	if req != nil && req.Params != nil && len(req.Params.Arguments) > 0 {
		_ = json.Unmarshal(req.Params.Arguments, &out)
	}
	return out
}

var _ io.Reader
