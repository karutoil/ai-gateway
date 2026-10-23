package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"ai-gateway/internal/httperr"
	"ai-gateway/internal/mcp"
	"ai-gateway/internal/middleware"
)

// bufferedResponseWriter captures a complete upstream round instead of writing
// to the client. The MCP bridge runs every non-final round through it so the
// tool calls can be inspected before anything is committed downstream.
type bufferedResponseWriter struct {
	mu      sync.Mutex
	header  http.Header
	status  int
	body    bytes.Buffer
	written bool
}

func newBufferedResponseWriter() *bufferedResponseWriter {
	return &bufferedResponseWriter{header: make(http.Header), status: http.StatusOK}
}

func (b *bufferedResponseWriter) Header() http.Header { return b.header }

func (b *bufferedResponseWriter) WriteHeader(status int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.written {
		return
	}
	b.status = status
	b.written = true
}

func (b *bufferedResponseWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.written = true
	return b.body.Write(p)
}

func (b *bufferedResponseWriter) Flush() {}

func (b *bufferedResponseWriter) result() mcp.RoundResponse {
	b.mu.Lock()
	defer b.mu.Unlock()
	hdr := b.header.Clone()
	return mcp.RoundResponse{Status: b.status, Header: hdr, Body: append([]byte(nil), b.body.Bytes()...)}
}

// forceNonStream rewrites "stream": true to false so a round can be buffered
// and inspected. The client still receives a stream: the bridge emits the
// final round as SSE with keepalive frames while the tool rounds run.
func forceNonStream(body []byte) []byte {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	if _, ok := root["stream"]; !ok {
		return body
	}
	root["stream"] = json.RawMessage(`false`)
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

// mcpRoundKey marks a request as one iteration of the gateway-side MCP tool
// loop. It is a context key on purpose: the marker must not be reachable from
// the wire (see mcpRound in fallback.go).
type mcpRoundKey struct{}

// withMCPRound tags ctx as an internal MCP tool round.
func withMCPRound(ctx context.Context) context.Context {
	return context.WithValue(ctx, mcpRoundKey{}, true)
}

// isMCPRound reports whether ctx belongs to an internal MCP tool round.
func isMCPRound(ctx context.Context) bool {
	v, _ := ctx.Value(mcpRoundKey{}).(bool)
	return v
}

// mcpPrepareOutcome is the tri-state result of resolving MCP references. It
// exists so "no references" (forward unchanged) can never be confused with
// "references were invalid, the response is already written" (stop).
type mcpPrepareOutcome int

const (
	// mcpNoReferences: the request carried no mcp tool entries.
	mcpNoReferences mcpPrepareOutcome = iota
	// mcpExpanded: the body now holds real function tools; forward it, and if
	// the plan opted into automatic execution, run the tool loop instead.
	mcpExpanded
	// mcpRejected: an error response has already been written; stop.
	mcpRejected
)

// prepareMCP resolves LiteLLM-style MCP reference blocks in the request body.
func (h *Handler) prepareMCP(w http.ResponseWriter, r *http.Request, body []byte, proto mcp.Protocol, endpoint string) ([]byte, *mcp.Plan, mcpPrepareOutcome) {
	if h.MCP == nil {
		return body, nil, mcpNoReferences
	}
	if _, ok := mcp.ReferencesFromBody(body); !ok {
		return body, nil, mcpNoReferences
	}
	key, _ := middleware.GatewayKeyFromContext(r.Context())
	expanded, plan, err := h.MCP.Prepare(key, body, proto)
	if err != nil {
		if strings.Contains(err.Error(), "mcp_no_tools_available") || strings.Contains(err.Error(), "invalid mcp server_url") || strings.Contains(err.Error(), "not registered or not authorized") {
			httperr.Invalid(w, err.Error())
			return body, nil, mcpRejected
		}
		httperr.Proxy(w, http.StatusBadGateway, "mcp reference resolution failed: "+err.Error())
		return body, nil, mcpRejected
	}
	if plan == nil {
		return body, nil, mcpNoReferences
	}
	// Advertise which tools the reference expanded to. Clients that support
	// structured metadata read the JSON body fields; every client can read
	// these headers.
	w.Header().Set("X-MCP-Tools", strconv.Itoa(len(plan.Bindings)))
	w.Header().Set("X-MCP-Servers", strings.Join(planServerSlugs(plan), ","))
	return expanded, plan, mcpExpanded
}

func planServerSlugs(plan *mcp.Plan) []string {
	out := make([]string, 0, len(plan.Servers))
	for _, srv := range plan.Servers {
		out = append(out, srv.Slug)
	}
	return out
}

// runMCPLoop executes the gateway-side tool loop and writes the final answer.
func (h *Handler) runMCPLoop(w http.ResponseWriter, r *http.Request, body []byte, plan *mcp.Plan, proto mcp.Protocol, endpoint string) {
	keyPrefix := r.Header.Get("X-Gateway-Key-Prefix")
	if k, ok := middleware.GatewayKeyFromContext(r.Context()); ok && k != nil {
		keyPrefix = k.Prefix
	}
	isStream := false
	var probe map[string]json.RawMessage
	if json.Unmarshal(body, &probe) == nil {
		if raw, ok := probe["stream"]; ok {
			var s bool
			if json.Unmarshal(raw, &s) == nil {
				isStream = s
			}
		}
	}
	exec := func(ctx context.Context, roundBody []byte) mcp.RoundResponse {
		return h.mcpRound(ctx, r, roundBody, proto, endpoint)
	}
	h.MCP.RunAuto(w, r, body, plan, proto, endpoint, keyPrefix, isStream, exec)
}

// mcpRound performs exactly one complete upstream LLM round with the supplied
// body. It re-enters the normal protocol handler with a buffered writer so
// routing, translation, retries, logging and metering are all reused
// unchanged — a tool round is billed and logged exactly like a client request.
func (h *Handler) mcpRound(ctx context.Context, r *http.Request, body []byte, proto mcp.Protocol, endpoint string) mcp.RoundResponse {
	roundBody := forceNonStream(body)
	bw := newBufferedResponseWriter()
	rr := r.Clone(withMCPRound(ctx))
	rr.Body = io.NopCloser(bytes.NewReader(roundBody))
	rr.ContentLength = int64(len(roundBody))
	rr.Header = r.Header.Clone()
	rr.Header.Set("Content-Type", "application/json")
	rr.Header.Del("Accept")
	// A round must never consult or populate the response cache: the same body
	// repeated in a loop would otherwise replay a stale pre-tool answer.
	rr.Header.Del("X-Gateway-MCP-Round")

	switch proto {
	case mcp.ProtocolAnthropic:
		h.AnthropicMessages(bw, rr)
	case mcp.ProtocolResponses:
		h.Responses(bw, rr)
	default:
		h.ChatCompletions(bw, rr)
	}
	return bw.result()
}
