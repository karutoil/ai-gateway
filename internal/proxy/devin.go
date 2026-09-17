package proxy

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"ai-gateway/internal/antigravity"
	"ai-gateway/internal/db"
	"ai-gateway/internal/devin"
	"ai-gateway/internal/httperr"
	"ai-gateway/internal/models"
	"ai-gateway/internal/translate"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// isDevin reports whether a provider speaks the Devin Connect-proto transport.
func isDevin(p *models.Provider) bool {
	return p != nil && p.Type == models.ProviderDevin
}

// devinCostFor returns a zero-cost function: Devin bills via seat/quota,
// not per-token metered pricing.
func devinCostFor() func(prompt, completion int) float64 {
	return func(_, _ int) float64 { return 0 }
}

// devinEndpoints returns the provider base URL first (custom mirrors + tests),
// then the built-in host.
func devinEndpoints(p *models.Provider) []string {
	if base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/"); base != "" {
		if base != strings.TrimRight(devin.Host(), "/") {
			return []string{base, devin.Host()}
		}
		return []string{base}
	}
	return []string{devin.Host()}
}

// devinEffort normalizes the client's requested reasoning effort for wire
// routing ("" when the request carries none).
func devinEffort(openAIChatBody []byte) string {
	return strings.ToLower(strings.TrimSpace(translate.ExtractReasoningEffort(openAIChatBody)))
}

// proxyDevin serves one inbound request via a Devin OAuth provider.
// chatBody is OpenAI chat-shaped (callers normalize Anthropic/Responses
// first). endpoint is the client dialect: "chat.completions", "messages" or
// "responses".
func (h *Handler) proxyDevin(w http.ResponseWriter, r *http.Request, chatBody []byte, isStream bool, model, endpoint, keyPrefix string, start time.Time, p *models.Provider) {
	ctx := r.Context()
	if h.Timeouts.RequestTotal > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.Timeouts.RequestTotal)
		defer cancel()
	}

	sessionToken, _, _, err := h.ProviderStore.EnsureFreshAccess(ctx, p, h.clientOrDefault())
	if err != nil {
		httperr.Write(w, http.StatusBadGateway, "devin oauth not connected or refresh failed — reconnect in Providers", httperr.TypeProxy)
		h.logRequestExtended(keyPrefix, p.ID, model, endpoint, http.StatusBadGateway, time.Since(start).Milliseconds(), 0, 0, 0, isStream)
		return
	}

	userJWT, chatBase, err := devin.GetUserJWT(sessionToken, devinEndpoints(p)[0], h.clientOrDefault())
	if err != nil {
		httperr.Write(w, http.StatusBadGateway, "devin authentication failed — reconnect in Providers", httperr.TypeProxy)
		h.logRequestExtended(keyPrefix, p.ID, model, endpoint, http.StatusBadGateway, time.Since(start).Milliseconds(), 0, 0, 0, isStream)
		return
	}

	baseModel := devin.CollapseReasoningVariant(stripProviderPrefix(model, p))
	wire := devin.ResolveRuntime(stripProviderPrefix(model, p), devinEffort(chatBody), h.devinModelRouting(p.ID, baseModel))
	chatIn, err := devin.FromOpenAI(chatBody, wire)
	if err != nil {
		httperr.Invalid(w, "invalid request body: "+err.Error())
		return
	}

	framed := devin.FrameConnect(devin.BuildChatRequest(chatIn, sessionToken, userJWT))
	var resp *http.Response
	var lastStatus int
	var lastErrText string
	// The auth response may direct chat traffic at a per-account host: try it
	// first, then fall back to the configured endpoints.
	chatBases := devinEndpoints(p)
	if chatBase != "" && chatBase != chatBases[0] {
		chatBases = append([]string{chatBase}, chatBases...)
	}
	for _, base := range chatBases {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/exa.api_server_pb.ApiServerService/GetChatMessage", bytes.NewReader(framed))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/connect+proto")
		req.Header.Set("connect-protocol-version", "1")
		req.Header.Set("connect-content-encoding", "gzip")
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("User-Agent", "connect-go/1.18.1 (go1.26.3)")
		req.Header.Set("connect-accept-encoding", "gzip")
		resp, err = h.clientOrDefault().Do(req)
		if err != nil {
			lastErrText = err.Error()
			resp = nil
			continue
		}
		lastStatus = resp.StatusCode
		if resp.StatusCode == 200 {
			break
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
		resp.Body.Close()
		resp = nil
		lastErrText = string(b)
		if lastStatus == 429 || lastStatus >= 500 {
			continue
		}
		break
	}
	if resp == nil {
		status := lastStatus
		if status == 0 {
			status = http.StatusBadGateway
		}
		httperr.Proxy(w, status, friendlyDevinMessage(status, lastErrText))
		h.logRequestExtended(keyPrefix, p.ID, model, endpoint, status, time.Since(start).Milliseconds(), 0, 0, 0, isStream)
		return
	}
	defer resp.Body.Close()

	switch endpoint {
	case "messages", "responses":
		h.serveDevinBuffered(w, resp, model, wire, chatIn, endpoint, keyPrefix, start, p.ID, isStream)
	default:
		h.serveDevinChat(w, r, resp, model, wire, chatIn, keyPrefix, start, p.ID, isStream)
	}
}

// devinModelRouting loads the server-declared effort→wire-uid map stored at
// discovery for one base model. NULL (legacy rows) means "suffix convention";
// an empty map means "no routable levels — send the id verbatim".
func (h *Handler) devinModelRouting(providerID, base string) map[string]string {
	if h.DB == nil || strings.TrimSpace(base) == "" {
		return nil
	}
	var raw sql.NullString
	if err := h.DB.QueryRow(db.Q(`SELECT reasoning_routing FROM provider_models WHERE provider_id=? AND model_id=?`), providerID, base).Scan(&raw); err != nil || !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil
	}
	var routing map[string]string
	if err := json.Unmarshal([]byte(raw.String), &routing); err != nil {
		return nil
	}
	if routing == nil {
		routing = map[string]string{}
	}
	return routing
}

// logDevinTrailer records a backend trailer failure (or malformed stream) in
// the requests-tab row (502), metrics, and a structured server log with
// wire-level context for Devin support tickets. Streaming exchanges must call
// this explicitly: headers have already flowed, so without it the failure is
// invisible in request history.
func (h *Handler) logDevinTrailer(model, wire string, chatIn devin.ChatInput, endpoint, keyPrefix, providerID string, start time.Time, stream bool, trailer string) {
	h.logRequestExtended(keyPrefix, providerID, model, endpoint, http.StatusBadGateway, time.Since(start).Milliseconds(), 0, 0, 0, stream)
	toolCalls := 0
	for _, m := range chatIn.Messages {
		toolCalls += len(m.ToolCalls)
	}
	log.Warn().Str("provider", providerID).Str("model", model).Str("wire_model", wire).
		Str("endpoint", endpoint).Bool("stream", stream).
		Int("turns", len(chatIn.Messages)).Int("wire_tool_calls", toolCalls).Int("tool_defs", len(chatIn.Tools)).
		Str("trailer", trailer).Msg("devin backend trailer error")
}

// friendlyDevinTrailerError classifies in-stream trailer errors. Backend
// internal errors carry error/trace IDs for Devin support and are
// model/account-specific, so the message keeps the IDs and advises trying
// another model.
func friendlyDevinTrailerError(msg string) string {
	msg = strings.TrimSpace(msg)
	if strings.Contains(strings.ToLower(msg), "internal error") {
		return "Devin backend reported an internal error for this model/account — try another Devin model (availability varies per model and account). " + msg
	}
	return msg
}

func friendlyDevinMessage(status int, body string) string {
	msg := ScrubSecrets(strings.TrimSpace(body))
	if len(msg) > 500 {
		msg = msg[:500]
	}
	switch status {
	case 401, 403:
		return "Devin authentication failed — reconnect the provider in Providers."
	case 404:
		return "Devin model not available for this account — refresh models and try another."
	case 429:
		return "Devin rate/quota reached — wait for reset or try later. " + msg
	}
	if msg == "" {
		return "Devin upstream unavailable"
	}
	return "Devin error: " + msg
}

// devinToolAccum accumulates streamed tool-argument fragments per call,
// mirroring the reference client (cumulative payloads replace, deltas append).
// swe-2 streams one logical call as a head frame (id+name+partial args)
// followed by continuation frames with no id/name and one args fragment
// each; grouping strictly by id would emit each fragment as its own call
// with invalid-JSON args ("{", "\"target_directory\": \"", ...), which
// harnesses reject as "tool not found: tool" / missing-field errors.
//
// Calls that never receive a real tool name are dropped (never surfaced as
// the literal placeholder "tool"): no such tool exists, so emitting it fails
// every harness and poisons the agent loop into retries.
type devinToolAccum struct {
	order  []string
	args   map[string]string
	names  map[string]string
	lastID string
}

func newDevinToolAccum() *devinToolAccum {
	return &devinToolAccum{args: map[string]string{}, names: map[string]string{}}
}

// Add folds one tool delta in and returns the new text fragment to emit,
// whether this is the first frame for the call, and the effective call id
// to emit under (continuation fragments map to their head's id).
func (a *devinToolAccum) Add(id, name, argsJSON string) (fragment string, start bool, effectiveID string) {
	if strings.TrimSpace(id) == "" {
		if a.lastID != "" {
			id = a.lastID
		} else {
			id = "call_" + uuid.NewString()[:8]
		}
	}
	previous, seen := a.args[id]
	if !seen {
		a.order = append(a.order, id)
		previous = ""
		start = true
		a.lastID = id
	} else {
		a.lastID = id
	}
	if isRealToolName(name) {
		a.names[id] = name
	} else if !seen && name != "" {
		// Keep a placeholder only when the head itself has no real name;
		// Name still reports "" until a real name arrives, so the
		// placeholder never leaks to harnesses.
		if _, ok := a.names[id]; !ok {
			a.names[id] = name
		}
	}
	accumulated := argsJSON
	switch {
	case !seen || previous == "":
		accumulated = argsJSON
	case strings.HasPrefix(argsJSON, previous):
		// Cumulative sender: this payload already contains everything
		// accumulated so far; the new text is the suffix.
		accumulated = argsJSON
	case isCompleteJSON(previous) && isCompleteJSON(argsJSON):
		// Cumulative revision of a closed object ({"a":1} then
		// {"a":1,"b":2}): appending would corrupt both objects into
		// invalid JSON, which downstream degrades to {} and harnesses
		// reject as "missing required property".
		accumulated = argsJSON
	default:
		accumulated = previous + argsJSON
	}
	a.args[id] = accumulated
	if strings.HasPrefix(accumulated, previous) {
		return accumulated[len(previous):], start, id
	}
	// The revision moved backwards; there is no coherent incremental text.
	return "", start, id
}

// isCompleteJSON reports whether s is a self-contained JSON object/array.
func isCompleteJSON(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	if c := t[0]; c != '{' && c != '[' {
		return false
	}
	return json.Valid([]byte(t))
}

// isRealToolName reports whether name identifies a real tool rather than a
// missing-name placeholder from the wire decoder.
func isRealToolName(name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	return name != "tool"
}

// Name returns the latest known real tool name for a call id, or "" when
// none arrived yet. The "" (never the literal "tool") signals the caller to
// hold or drop the call: emitting a fake name fails every harness.
func (a *devinToolAccum) Name(id string) string {
	if n := a.names[id]; isRealToolName(n) {
		return n
	}
	return ""
}

// FinalCalls returns one accumulated tool call per id, in first-seen order.
// Calls that never received a real name are dropped rather than emitted
// with a fake one.
func (a *devinToolAccum) FinalCalls() []antigravity.ParsedToolCall {
	var out []antigravity.ParsedToolCall
	for _, id := range a.order {
		name := a.Name(id)
		if !isRealToolName(name) {
			continue
		}
		var args map[string]any
		if s := a.args[id]; s != "" {
			_ = json.Unmarshal([]byte(s), &args)
		}
		if args == nil {
			args = map[string]any{}
		}
		out = append(out, antigravity.ParsedToolCall{ID: id, Name: name, Arguments: args})
	}
	return out
}

// rawToolCall is one accumulated call with its byte-identical args payload.
type rawToolCall struct {
	ID       string
	Name     string
	ArgsJSON string
}

// FinalRawCalls is FinalCalls with byte-identical argument payloads (no map
// round-trip), for streaming emission. Unnamed calls are dropped.
func (a *devinToolAccum) FinalRawCalls() []rawToolCall {
	var out []rawToolCall
	for _, id := range a.order {
		name := a.Name(id)
		if !isRealToolName(name) {
			continue
		}
		out = append(out, rawToolCall{ID: id, Name: name, ArgsJSON: a.args[id]})
	}
	return out
}

// serveDevinChat relays a Devin Connect stream as OpenAI chat SSE (stream) or
// JSON (non-stream).
func (h *Handler) serveDevinChat(w http.ResponseWriter, r *http.Request, resp *http.Response, model, wire string, chatIn devin.ChatInput, keyPrefix string, start time.Time, providerID string, isStream bool) {
	if !isStream {
		deltas, trailerErr := collectDevinDeltas(resp.Body)
		if trailerErr != "" {
			httperr.Proxy(w, http.StatusBadGateway, "Devin stream error: "+friendlyDevinTrailerError(trailerErr))
			h.logDevinTrailer(model, wire, chatIn, "chat.completions", keyPrefix, providerID, start, false, trailerErr)
			return
		}
		chunks := devinDeltasToChunks(deltas)
		body, _, prompt, completion, _, _ := antigravity.NonStreamOpenAI(model, chunks)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		h.logRequestExtended(keyPrefix, providerID, model, "chat.completions", http.StatusOK, time.Since(start).Milliseconds(), prompt, completion, 0, false)
		if h.Metrics != nil {
			h.Metrics.IncRequests(providerID, model, "chat.completions", http.StatusOK)
		}
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	st := newOpenAIChunkStreamer(w, model)
	tools := newDevinToolAccum()
	finishLen := false

	var staging []byte
	buf := make([]byte, 32<<10)
	for {
		if r.Context().Err() != nil {
			break
		}
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			staging = append(staging, buf[:n]...)
		}
		frames, rest, err := devin.ParseFrames(staging)
		if err != nil {
			writeSSEUpstreamError(w, false, "Devin stream error: malformed frame")
			h.logDevinTrailer(model, wire, chatIn, "chat.completions", keyPrefix, providerID, start, true, "malformed frame")
			return
		}
		staging = rest
		for _, fr := range frames {
			if fr.Trailer {
				// Headers already flowed, so errors terminate in-band —
				// and still land in request history via logDevinTrailer.
				if msg := devin.TrailerError(fr.Payload); msg != "" {
					writeSSEUpstreamError(w, false, "Devin stream error: "+friendlyDevinTrailerError(msg))
					h.logDevinTrailer(model, wire, chatIn, "chat.completions", keyPrefix, providerID, start, true, msg)
					return
				}
				continue
			}
			deltas, derr := devin.DecodeChatResponse(fr.Payload)
			if derr != nil {
				continue
			}
			for _, d := range deltas {
				switch d.Type {
				case "text":
					if d.Text != "" {
						st.WriteText(d.Text)
					}
				case "tool":
					// Buffer, don't stream partial args: a head frame may
					// arrive before its real name (emitting the first
					// chunk with a fake name locks harnesses onto
					// "unknown tool: tool"), and cumulative revisions
					// have no coherent incremental text. Completed
					// calls flush after the upstream finishes.
					_, _, _ = tools.Add(d.ID, d.Name, d.ArgsJSON)
				case "usage":
					st.WriteUsage(d.Input, d.Output)
				case "stop":
					// StopReason MAX_TOKENS=3 alone means length; 1 is
					// INCOMPLETE (a clean stop), per the OMP schema.
					if d.Stop == 3 {
						finishLen = true
					}
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	for _, tc := range tools.FinalRawCalls() {
		args := tc.ArgsJSON
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		st.WriteToolCall(tc.ID, tc.Name, args, true)
	}
	if finishLen {
		st.SetFinish("length")
	}
	st.Close()
	h.logRequestExtended(keyPrefix, providerID, model, "chat.completions", http.StatusOK, time.Since(start).Milliseconds(), st.prompt, st.completion, 0, true)
	if h.Metrics != nil {
		h.Metrics.IncRequests(providerID, model, "chat.completions", http.StatusOK)
	}
}

// serveDevinBuffered collects a full Devin stream then renders the messages
// or responses dialect.
func (h *Handler) serveDevinBuffered(w http.ResponseWriter, resp *http.Response, model, wire string, chatIn devin.ChatInput, endpoint, keyPrefix string, start time.Time, providerID string, isStream bool) {
	deltas, trailerErr := collectDevinDeltas(resp.Body)
	if trailerErr != "" {
		// Nothing written yet, so a plain 502 (not an SSE frame) is correct
		// even when the client asked to stream.
		httperr.Proxy(w, http.StatusBadGateway, "Devin stream error: "+friendlyDevinTrailerError(trailerErr))
		h.logDevinTrailer(model, wire, chatIn, endpoint, keyPrefix, providerID, start, isStream, trailerErr)
		return
	}
	chunks := devinDeltasToChunks(deltas)
	costFn := devinCostFor()
	if endpoint == "messages" {
		h.serveChunksAsAnthropic(w, chunks, model, "messages", costFn, keyPrefix, providerID, start, isStream)
		return
	}
	h.serveChunksAsResponses(w, chunks, model, "responses", costFn, keyPrefix, providerID, start, isStream)
}

// collectDevinDeltas drains framed deltas; the first trailer error wins.
func collectDevinDeltas(body io.Reader) ([]devin.Delta, string) {
	raw, err := io.ReadAll(io.LimitReader(body, 8<<20))
	if err != nil {
		return nil, ""
	}
	frames, _, err := devin.ParseFrames(raw)
	if err != nil {
		return nil, ""
	}
	var out []devin.Delta
	for _, fr := range frames {
		if fr.Trailer {
			if msg := devin.TrailerError(fr.Payload); msg != "" {
				return out, msg
			}
			continue
		}
		deltas, derr := devin.DecodeChatResponse(fr.Payload)
		if derr != nil {
			continue
		}
		out = append(out, deltas...)
		if len(out) > 10000 {
			break
		}
	}
	return out, ""
}

// devinDeltasToChunks normalizes deltas into shared ParsedChunks, accumulating
// streamed tool arguments into final per-call objects.
func devinDeltasToChunks(deltas []devin.Delta) []antigravity.ParsedChunk {
	tools := newDevinToolAccum()
	var out []antigravity.ParsedChunk
	var cur antigravity.ParsedChunk
	commit := func() {
		if len(cur.Texts) > 0 || cur.Usage.HasUsage || cur.Finish != "" {
			out = append(out, cur)
			cur = antigravity.ParsedChunk{}
		}
	}
	for _, d := range deltas {
		switch d.Type {
		case "text":
			cur.Texts = append(cur.Texts, d.Text)
			cur.HasData = true
		case "tool":
			commit()
			_, _, _ = tools.Add(d.ID, d.Name, d.ArgsJSON)
		case "usage":
			cur.Usage = antigravity.Usage{
				Input: d.Input, Output: d.Output,
				CacheRead: d.CacheRead, CacheWrite: d.CacheWr,
				Total:    d.Input + d.Output + d.CacheRead + d.CacheWr,
				HasUsage: true,
			}
			cur.HasData = true
		case "stop":
			if d.Stop == 3 {
				cur.Finish = "MAX_TOKENS"
			} else {
				cur.Finish = "STOP"
			}
			cur.HasData = true
		}
	}
	commit()
	if calls := tools.FinalCalls(); len(calls) > 0 {
		out = append(out, antigravity.ParsedChunk{ToolCalls: calls, HasData: true, Finish: cur.Finish})
	} else if len(out) == 0 && cur.Finish != "" {
		out = append(out, cur)
	}
	return out
}
