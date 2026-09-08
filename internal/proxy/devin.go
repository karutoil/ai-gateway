package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"ai-gateway/internal/antigravity"
	"ai-gateway/internal/devin"
	"ai-gateway/internal/httperr"
	"ai-gateway/internal/models"
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

	userJWT, err := devin.GetUserJWT(sessionToken, devinEndpoints(p)[0], h.clientOrDefault())
	if err != nil {
		httperr.Write(w, http.StatusBadGateway, "devin authentication failed — reconnect in Providers", httperr.TypeProxy)
		h.logRequestExtended(keyPrefix, p.ID, model, endpoint, http.StatusBadGateway, time.Since(start).Milliseconds(), 0, 0, 0, isStream)
		return
	}

	chatIn, err := devin.FromOpenAI(chatBody, model)
	if err != nil {
		httperr.Invalid(w, "invalid request body: "+err.Error())
		return
	}

	framed := devin.FrameConnect(devin.BuildChatRequest(chatIn, sessionToken, userJWT))
	var resp *http.Response
	var lastStatus int
	var lastErrText string
	for _, base := range devinEndpoints(p) {
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
		h.serveDevinBuffered(w, resp, model, endpoint, keyPrefix, start, p.ID, isStream)
	default:
		h.serveDevinChat(w, r, resp, model, keyPrefix, start, p.ID, isStream)
	}
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
type devinToolAccum struct {
	order []string
	args  map[string]string
	names map[string]string
}

func newDevinToolAccum() *devinToolAccum {
	return &devinToolAccum{args: map[string]string{}, names: map[string]string{}}
}

// Add folds one tool delta in and returns the new text fragment to emit plus
// whether this is the first frame for the call.
func (a *devinToolAccum) Add(id, name, argsJSON string) (fragment string, start bool) {
	previous, seen := a.args[id]
	if !seen {
		a.order = append(a.order, id)
		previous = ""
		start = true
	}
	if name != "" {
		a.names[id] = name
	}
	accumulated := argsJSON
	if !strings.HasPrefix(argsJSON, previous) {
		accumulated = previous + argsJSON
	}
	a.args[id] = accumulated
	return accumulated[len(previous):], start
}

// Name returns the latest known tool name for a call id.
func (a *devinToolAccum) Name(id string) string {
	if n := a.names[id]; n != "" {
		return n
	}
	return "tool"
}

// FinalCalls returns one accumulated tool call per id, in first-seen order.
func (a *devinToolAccum) FinalCalls() []antigravity.ParsedToolCall {
	var out []antigravity.ParsedToolCall
	for _, id := range a.order {
		var args map[string]any
		if s := a.args[id]; s != "" {
			_ = json.Unmarshal([]byte(s), &args)
		}
		if args == nil {
			args = map[string]any{}
		}
		out = append(out, antigravity.ParsedToolCall{ID: id, Name: a.Name(id), Arguments: args})
	}
	return out
}

// serveDevinChat relays a Devin Connect stream as OpenAI chat SSE (stream) or
// JSON (non-stream).
func (h *Handler) serveDevinChat(w http.ResponseWriter, r *http.Request, resp *http.Response, model, keyPrefix string, start time.Time, providerID string, isStream bool) {
	if !isStream {
		deltas, trailerErr := collectDevinDeltas(resp.Body)
		if trailerErr != "" {
			httperr.Proxy(w, http.StatusBadGateway, "Devin stream error: "+trailerErr)
			h.logRequestExtended(keyPrefix, providerID, model, "chat.completions", http.StatusBadGateway, time.Since(start).Milliseconds(), 0, 0, 0, false)
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
			return
		}
		staging = rest
		for _, fr := range frames {
			if fr.Trailer {
				// Headers already flowed, so errors terminate in-band.
				if msg := devin.TrailerError(fr.Payload); msg != "" {
					writeSSEUpstreamError(w, false, "Devin stream error: "+msg)
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
					fragment, start := tools.Add(d.ID, d.Name, d.ArgsJSON)
					st.WriteToolCall(d.ID, tools.Name(d.ID), fragment, start)
				case "usage":
					st.WriteUsage(d.Input, d.Output)
				case "stop":
					if d.Stop == 1 || d.Stop == 3 {
						finishLen = true
					}
				}
			}
		}
		if readErr != nil {
			break
		}
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
func (h *Handler) serveDevinBuffered(w http.ResponseWriter, resp *http.Response, model, endpoint, keyPrefix string, start time.Time, providerID string, isStream bool) {
	deltas, trailerErr := collectDevinDeltas(resp.Body)
	if trailerErr != "" {
		httperr.Proxy(w, http.StatusBadGateway, "Devin stream error: "+trailerErr)
		h.logRequestExtended(keyPrefix, providerID, model, endpoint, http.StatusBadGateway, time.Since(start).Milliseconds(), 0, 0, 0, isStream)
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
			tools.Add(d.ID, d.Name, d.ArgsJSON)
		case "usage":
			cur.Usage = antigravity.Usage{
				Input: d.Input, Output: d.Output,
				CacheRead: d.CacheRead, CacheWrite: d.CacheWr,
				Total:    d.Input + d.Output + d.CacheRead + d.CacheWr,
				HasUsage: true,
			}
			cur.HasData = true
		case "stop":
			if d.Stop == 1 || d.Stop == 3 {
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
