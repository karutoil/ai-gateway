package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ai-gateway/internal/models"
	"github.com/rs/zerolog/log"
)

// RoundResponse is the buffered result of one upstream LLM round.
type RoundResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

// Executor performs exactly one upstream LLM request with the supplied body.
// The proxy wires this to the existing handler so routing, translation,
// retries, logging and metering are all reused unchanged.
type Executor func(ctx context.Context, body []byte) RoundResponse

// Bridge connects the MCP registry to the LLM proxy surface.
type Bridge struct {
	Store       *Store
	Manager     *Manager
	MaxRounds   int
	LoopTimeout time.Duration
}

func NewBridge(store *Store, manager *Manager, maxRounds int, loopTimeout time.Duration) *Bridge {
	if maxRounds <= 0 {
		maxRounds = 25
	}
	if loopTimeout <= 0 {
		loopTimeout = 5 * time.Minute
	}
	return &Bridge{Store: store, Manager: manager, MaxRounds: maxRounds, LoopTimeout: loopTimeout}
}

// Prepare resolves MCP references for a request and returns the body with the
// references expanded into ordinary function tools. A nil plan means the
// request carried no MCP references and must be forwarded unchanged.
func (b *Bridge) Prepare(key *models.GatewayKey, body []byte, proto Protocol) ([]byte, *Plan, error) {
	refs, ok := ReferencesFromBody(body)
	if !ok {
		return body, nil, nil
	}
	if key == nil {
		return nil, nil, fmt.Errorf("mcp references require an authenticated gateway key")
	}
	access, err := b.Store.AccessForKey(key)
	if err != nil {
		return nil, nil, err
	}
	plan, tools, err := BuildPlan(access, refs)
	if err != nil {
		return nil, nil, err
	}
	expanded, err := ExpandBody(body, proto, plan, tools)
	if err != nil {
		return nil, nil, err
	}
	return expanded, plan, nil
}

// RunAuto executes the gateway-side tool loop for requests whose references
// opted into automatic execution (require_approval: "never").
func (b *Bridge) RunAuto(w http.ResponseWriter, r *http.Request, expandedBody []byte, plan *Plan, proto Protocol, endpoint, keyPrefix string, isStream bool, exec Executor) {
	ctx, cancel := context.WithTimeout(r.Context(), b.LoopTimeout)
	defer cancel()

	var summaries []CallSummary
	seenCalls := map[string]bool{}
	rounds := 0
	loopErr := ""
	approvalRequired := false

	// Streaming: commit the SSE response immediately and keep it alive while
	// the (buffered) tool rounds run. The final model round's bytes are
	// forwarded verbatim afterwards.
	var stopKeepalive func()
	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		writeSSEComment(w, "mcp: gateway-side tool execution")
		stopKeepalive = startKeepalive(w, 10*time.Second)
	}

	currentBody := expandedBody
	var last RoundResponse
	haveLast := false

	for {
		if rounds >= b.MaxRounds {
			loopErr = "mcp_max_rounds_exceeded"
			log.Warn().Str("endpoint", endpoint).Int("rounds", rounds).Msg("mcp tool loop hit the configured round cap")
			break
		}
		if ctx.Err() != nil {
			loopErr = "mcp_timeout"
			break
		}
		last = exec(ctx, currentBody)
		haveLast = true
		if last.Status < 200 || last.Status >= 300 {
			break
		}
		calls := ParseToolCalls(proto, last.Header.Get("Content-Type"), last.Body)
		if len(calls) == 0 {
			break
		}
		// Repeating an identical call with identical arguments is a stuck
		// model, not progress: without this, a provider that keeps replaying
		// the same tool call would consume the entire round budget (up to the
		// cap) re-executing a side-effecting tool. Stop after the first repeat
		// and report it, leaving the caller's raw answer intact.
		if sig := callSignature(calls); sig != "" {
			if seenCalls[sig] {
				loopErr = "mcp_repeated_tool_call"
				log.Warn().Str("endpoint", endpoint).Str("signature", sig).Msg("mcp tool loop stopped: identical tool call repeated")
				break
			}
			seenCalls[sig] = true
		}
		// Every call must be gateway-executable. A call outside the auto set
		// (or outside the authorized bindings) is handed back to the caller
		// untouched: we never silently execute something the request did not
		// authorize.
		executable := true
		for _, c := range calls {
			if _, ok := plan.Bindings[c.Name]; !ok {
				executable = false
				break
			}
			if !plan.AutoNames[c.Name] {
				executable = false
				break
			}
		}
		if !executable {
			approvalRequired = true
			break
		}
		results := make([]ToolResult, 0, len(calls))
		for _, c := range calls {
			binding := plan.Bindings[c.Name]
			res, err := b.Manager.CallToolMeta(ctx, binding.ServerID, binding.ToolName, c.Input, keyPrefix, endpoint)
			if err != nil {
				res = ToolResult{
					ServerID:   binding.ServerID,
					ServerSlug: binding.ServerSlug,
					Tool:       binding.ToolName,
					Namespaced: binding.Namespaced,
					IsError:    true,
					Text:       "tool execution failed: " + err.Error(),
				}
			}
			results = append(results, res)
			summaries = append(summaries, CallSummary{
				Server:     binding.ServerSlug,
				Tool:       binding.ToolName,
				Name:       binding.Namespaced,
				CallID:     c.ID,
				Round:      rounds + 1,
				IsError:    res.IsError,
				DurationMs: res.DurationMs,
			})
		}
		next, err := AppendToolResults(proto, currentBody, calls, results, "")
		if err != nil {
			loopErr = "mcp_loop_append_failed"
			log.Error().Err(err).Msg("mcp: failed to append tool results")
			break
		}
		currentBody = next
		rounds++
	}
	if stopKeepalive != nil {
		stopKeepalive()
	}

	// No upstream response at all (timeout before the first round).
	if !haveLast {
		msg := "mcp tool execution timed out before the model responded"
		if loopErr == "" {
			loopErr = "mcp_timeout"
		}
		if isStream {
			writeSSEError(w, proto, msg)
			return
		}
		http.Error(w, `{"error":{"message":"`+msg+`","type":"proxy_error"}}`, http.StatusGatewayTimeout)
		return
	}

	if isStream {
		// The response headers were committed before the first round (the SSE
		// keepalive stream has to start while tools run), so execution metadata
		// CANNOT ride in headers here — setting them now would be silently
		// dropped by net/http. Streaming clients get it in the terminal event
		// instead (InjectMetadata), which is the structured channel this
		// endpoint advertises.
		if last.Status < 200 || last.Status >= 300 {
			writeSSEErrorBody(w, proto, last.Status, last.Body)
			return
		}
		// Every round is buffered (a live stream cannot be inspected for tool
		// calls), so the final round arrives as JSON even though the client
		// asked for SSE. Convert it to a well-formed stream for this protocol
		// and attach the execution metadata to a real frame.
		body := last.Body
		ct := last.Header.Get("Content-Type")
		if !strings.Contains(strings.ToLower(ct), "text/event-stream") {
			body = JSONToSSE(proto, body)
		}
		body = InjectMetadata(proto, "text/event-stream", body, rounds, summaries, loopErr)
		_, _ = w.Write(body)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}

	copyResponseHeaders(w.Header(), last.Header)
	w.Header().Set("X-MCP-Rounds", fmt.Sprintf("%d", rounds))
	w.Header().Set("X-MCP-Tool-Calls", fmt.Sprintf("%d", len(summaries)))
	if approvalRequired {
		w.Header().Set("X-MCP-Approval-Required", "true")
	}
	if loopErr != "" {
		w.Header().Set("X-MCP-Error", loopErr)
	}
	body := InjectMetadata(proto, last.Header.Get("Content-Type"), last.Body, rounds, summaries, loopErr)
	w.WriteHeader(last.Status)
	_, _ = w.Write(body)
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vv := range src {
		switch strings.ToLower(k) {
		case "content-length", "transfer-encoding", "connection":
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func writeSSEComment(w http.ResponseWriter, text string) {
	_, _ = fmt.Fprintf(w, ": %s\n\n", text)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func startKeepalive(w http.ResponseWriter, interval time.Duration) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				_, _ = fmt.Fprint(w, ": mcp keepalive\n\n")
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func writeSSEError(w http.ResponseWriter, proto Protocol, msg string) {
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "proxy_error"}})
	if proto == ProtocolAnthropic {
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
	} else {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeSSEErrorBody(w http.ResponseWriter, proto Protocol, status int, body []byte) {
	if len(body) == 0 {
		writeSSEError(w, proto, fmt.Sprintf("upstream returned HTTP %d", status))
		return
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "event:") || strings.HasPrefix(trimmed, "data:") {
		_, _ = w.Write(body)
		return
	}
	if proto == ProtocolAnthropic {
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", trimmed)
	} else {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", trimmed)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// callSignature renders a stable identity for one batch of tool calls so the
// loop can detect a model that repeats itself instead of making progress.
func callSignature(calls []ToolCall) string {
	var parts []string
	for _, c := range calls {
		args := c.Arguments
		if strings.TrimSpace(args) == "" {
			if b, err := json.Marshal(c.Input); err == nil {
				args = string(b)
			}
		}
		parts = append(parts, c.Name+"("+args+")")
	}
	return strings.Join(parts, "|")
}
