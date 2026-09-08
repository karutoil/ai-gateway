package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"ai-gateway/internal/antigravity"
	"ai-gateway/internal/httperr"
	"ai-gateway/internal/models"
	"ai-gateway/internal/translate"

	"github.com/google/uuid"
)

// isAntigravity reports whether a provider speaks the Cloud Code Assist wire protocol.
func isAntigravity(p *models.Provider) bool {
	return p != nil && p.Type == models.ProviderAntigravity
}

// antigravityPublicID strips an optional "antigravity/" or provider-name prefix.
func antigravityPublicID(model string, p *models.Provider) string {
	m := model
	if i := strings.LastIndex(m, "/"); i >= 0 && i+1 < len(m) {
		prefix := strings.ToLower(strings.TrimSpace(m[:i]))
		if p != nil && (strings.EqualFold(prefix, p.Name) || prefix == "antigravity" || prefix == string(p.Type)) {
			m = m[i+1:]
		} else if prefix == "antigravity" {
			m = m[i+1:]
		}
	}
	return strings.TrimSpace(m)
}

// antigravityEffort normalizes reasoning effort to the routing vocabulary.
func antigravityEffort(openAIChatBody []byte) string {
	e := strings.ToLower(strings.TrimSpace(translate.ExtractReasoningEffort(openAIChatBody)))
	switch e {
	case "", "off", "none":
		return "off"
	case "minimal", "low", "medium", "high", "xhigh", "max":
		return e
	default:
		return "off"
	}
}

func antigravityCost(publicID string, prompt, completion int) float64 {
	for _, m := range antigravity.PublicModels {
		if m.ID == publicID {
			return float64(prompt)/1e6*m.InputCost + float64(completion)/1e6*m.OutputCost
		}
	}
	return 0
}

// proxyAntigravity serves one inbound request via an Antigravity OAuth provider.
// chatBody is OpenAI chat-shaped (callers normalize Anthropic/Responses first).
// endpoint is the client dialect: "chat.completions", "messages" or "responses".
func (h *Handler) proxyAntigravity(w http.ResponseWriter, r *http.Request, originalBody, chatBody []byte, isStream bool, model, endpoint, keyPrefix string, start time.Time, p *models.Provider) {
	publicID := antigravityPublicID(model, p)
	effort := antigravityEffort(chatBody)
	if pinned := strings.TrimSpace(os.Getenv("ANTIGRAVITY_RUNTIME_MODEL")); pinned != "" {
		_ = pinned
	}
	runtime := antigravity.ResolveRuntime(publicID, effort)
	if pinned := strings.TrimSpace(os.Getenv("ANTIGRAVITY_RUNTIME_MODEL")); pinned != "" {
		runtime = pinned
	}

	ctx := r.Context()
	if h.Timeouts.RequestTotal > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.Timeouts.RequestTotal)
		defer cancel()
	}

	access, project, _, err := h.ProviderStore.EnsureFreshAccess(ctx, p, h.Client)
	if err != nil {
		httperr.Write(w, http.StatusBadGateway, "antigravity oauth not connected or refresh failed — reconnect in Providers", httperr.TypeProxy)
		h.logRequestExtended(keyPrefix, p.ID, model, endpoint, http.StatusBadGateway, time.Since(start).Milliseconds(), 0, 0, 0, isStream)
		return
	}
	if project == "" {
		project = antigravity.DefaultProjectID("")
	}

	agBody, err := antigravity.BuildRequest(chatBody, project, runtime, effort)
	if err != nil {
		httperr.Invalid(w, "failed to build antigravity request: "+err.Error())
		return
	}

	runtimes := []string{runtime}
	if fb := antigravity.FallbackRuntime(runtime); fb != "" {
		runtimes = append(runtimes, fb)
	}
	// Provider BaseURL first (custom mirrors + tests), then built-in fallbacks.
	endpoints := []string{}
	if base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/"); base != "" {
		endpoints = append(endpoints, base)
	}
	for _, ep := range antigravity.EndpointCandidates() {
		dup := false
		for _, e := range endpoints {
			if strings.TrimRight(e, "/") == strings.TrimRight(ep, "/") {
				dup = true
				break
			}
		}
		if !dup {
			endpoints = append(endpoints, ep)
		}
	}

	var resp *http.Response
	var lastStatus int
	var lastErrText string
	usedRuntime := runtime
	usedEndpoint := ""
tried:
	for _, rt := range runtimes {
		bodyForTry := agBody
		if rt != runtime {
			// Rebuild with the fallback runtime id (model field differs).
			if b2, err := antigravity.BuildRequest(chatBody, project, rt, effort); err == nil {
				bodyForTry = b2
			}
		}
		for _, ep := range endpoints {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(ep, "/")+"/v1internal:streamGenerateContent?alt=sse", bytes.NewReader(bodyForTry))
			if err != nil {
				continue
			}
			for k, v := range antigravity.Headers(access) {
				req.Header.Set(k, v)
			}
			resp, err = h.clientOrDefault().Do(req)
			if err != nil {
				lastErrText = err.Error()
				continue
			}
			lastStatus = resp.StatusCode
			usedRuntime = rt
			usedEndpoint = ep
			if resp.StatusCode == 200 {
				break tried
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
			resp.Body.Close()
			resp = nil
			lastErrText = string(b)
			// 401: token may have rotated — refresh once and retry same endpoint.
			if lastStatus == 401 {
				if fresh, _, _, rerr := h.refreshAntigravityNow(ctx, p); rerr == nil {
					access = fresh
					req2, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(ep, "/")+"/v1internal:streamGenerateContent?alt=sse", bytes.NewReader(bodyForTry))
					for k, v := range antigravity.Headers(access) {
						req2.Header.Set(k, v)
					}
					if resp2, err2 := h.clientOrDefault().Do(req2); err2 == nil {
						if resp2.StatusCode == 200 {
							resp = resp2
							break tried
						}
						b2, _ := io.ReadAll(io.LimitReader(resp2.Body, 32<<10))
						resp2.Body.Close()
						lastStatus = resp2.StatusCode
						lastErrText = string(b2)
					}
				}
			}
			// 404 on the primary runtime falls through to the fallback runtime.
			if lastStatus == 404 && rt == runtime && len(runtimes) > 1 {
				break
			}
			if lastStatus == 429 || lastStatus >= 500 {
				continue
			}
			break
		}
	}
	_ = usedEndpoint
	if resp == nil {
		status := lastStatus
		if status == 0 {
			status = http.StatusBadGateway
		}
		msg := friendlyAntigravityMessage(status, lastErrText)
		httperr.Proxy(w, status, msg)
		h.logRequestExtended(keyPrefix, p.ID, model, endpoint, status, time.Since(start).Milliseconds(), 0, 0, 0, isStream)
		return
	}
	defer resp.Body.Close()

	switch endpoint {
	case "messages":
		h.serveAntigravityAsAnthropic(w, r, resp, originalBody, chatBody, model, publicID, usedRuntime, keyPrefix, start, p.ID, isStream)
	case "responses":
		h.serveAntigravityAsResponses(w, r, resp, model, publicID, usedRuntime, keyPrefix, start, p.ID, isStream)
	default:
		h.serveAntigravityAsOpenAI(w, r, resp, model, publicID, usedRuntime, keyPrefix, start, p.ID, isStream)
	}
}

func (h *Handler) clientOrDefault() *http.Client {
	if h.Client != nil {
		return h.Client
	}
	return http.DefaultClient
}

func (h *Handler) refreshAntigravityNow(ctx context.Context, p *models.Provider) (string, string, string, error) {
	return h.ProviderStore.EnsureFreshAccess(ctx, p, h.clientOrDefault())
}

func friendlyAntigravityMessage(status int, body string) string {
	parsed := parseAntigravityError(body)
	// Never leak tokens.
	safeRaw := ScrubSecrets(strings.TrimSpace(body))
	truncated := safeRaw
	if len(truncated) > 500 {
		truncated = truncated[:500]
	}
	switch status {
	case 400:
		if strings.Contains(strings.ToLower(safeRaw), "thought_signature") {
			return "Antigravity rejected the tool history (missing thought_signature on a Gemini 3+ turn). The gateway normalizes unsigned tool history automatically — retry the request; if it persists, start a fresh conversation so no unsigned tool calls are replayed."
		}
		if parsed.message != "" {
			return "Antigravity rejected the request (" + parsed.message + ") — simplify the request and retry."
		}
		return "Antigravity rejected the request. " + truncated
	case 401:
		return "Antigravity authentication failed — reconnect the provider in Providers (OAuth expired or revoked)."
	case 403:
		if parsed.validationURL != "" {
			return "Antigravity account verification required — " + parsed.message + " Open this link to verify, then retry: " + parsed.validationURL
		}
		if parsed.message != "" {
			return "Antigravity denied the request for this account/project (" + parsed.message + ") — try another model or reconnect."
		}
		return "Antigravity denied the request for this account/project — try another model or reconnect. " + truncated
	case 404:
		if parsed.message != "" {
			return "Antigravity model not available for this account (" + parsed.message + ") — try gemini-3.8-flash or refresh models."
		}
		return "Antigravity model not available for this account — try gemini-3.8-flash or refresh models. " + truncated
	case 429:
		return "Antigravity rate/quota reached — wait for reset or try another model. " + truncated
	}
	if truncated == "" {
		return "Antigravity upstream unavailable"
	}
	return "Antigravity error: " + truncated
}

// antigravityErrorDetail holds the actionable fields from a Google RPC error
// envelope. The validation URL is kept separate so truncation of the raw body
// can never cut it in half.
type antigravityErrorDetail struct {
	message        string
	reason         string
	validationURL  string
	validationText string
}

// parseAntigravityError extracts message/reason/validation URL from a
// cloudcode-pa error body like:
// {"error":{"message":"Verify your account to continue.","details":[{"reason":"VALIDATION_REQUIRED","metadata":{"validation_url":"https://..."}}]}}
func parseAntigravityError(body string) antigravityErrorDetail {
	var out antigravityErrorDetail
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Details []struct {
				Reason   string            `json:"reason"`
				Metadata map[string]string `json:"metadata"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		return out
	}
	out.message = strings.TrimSpace(envelope.Error.Message)
	for _, d := range envelope.Error.Details {
		if out.reason == "" && d.Reason != "" {
			out.reason = d.Reason
		}
		if d.Metadata != nil {
			if u := strings.TrimSpace(d.Metadata["validation_url"]); u != "" && out.validationURL == "" {
				out.validationURL = u
			}
			if t := strings.TrimSpace(d.Metadata["validation_url_link_text"]); t != "" && out.validationText == "" {
				out.validationText = t
			}
			if m := strings.TrimSpace(d.Metadata["validation_error_message"]); m != "" && out.message == "" {
				out.message = m
			}
		}
	}
	if out.message == "" {
		out.message = strings.TrimSpace(body)
		if len(out.message) > 300 {
			out.message = out.message[:300]
		}
	}
	return out
}

// collectAntigravityChunks drains the upstream SSE body into parsed chunks.
func collectAntigravityChunks(resp *http.Response) ([]antigravity.ParsedChunk, error) {
	var out []antigravity.ParsedChunk
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		c := antigravity.ParseChunk(strings.TrimPrefix(line, "data:"))
		if c.HasData || c.Finish != "" {
			out = append(out, c)
		}
		if len(out) > 10000 {
			break
		}
	}
	return out, sc.Err()
}

func (h *Handler) serveAntigravityAsOpenAI(w http.ResponseWriter, r *http.Request, resp *http.Response, model, publicID, runtime, keyPrefix string, start time.Time, providerID string, isStream bool) {
	if !isStream {
		chunks, err := collectAntigravityChunks(resp)
		if err != nil {
			httperr.Proxy(w, http.StatusBadGateway, "antigravity stream read failed")
			return
		}
		body, _, prompt, completion, total, finish := antigravity.NonStreamOpenAI(model, chunks)
		cost := antigravityCost(publicID, prompt, completion)
		_ = total
		_ = finish
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		h.logRequestExtended(keyPrefix, providerID, model, "chat.completions", http.StatusOK, time.Since(start).Milliseconds(), prompt, completion, cost, false)
		if h.Metrics != nil {
			h.Metrics.IncRequests(providerID, model, "chat.completions", http.StatusOK)
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)

	id := "chatcmpl-" + uuid.NewString()[:8]
	roleSent := false
	toolIdx := map[string]int{}
	nextIdx := 0
	var prompt, completion, cacheRead, reasoningTok, total int
	finish := "stop"
	latencyFirst := int64(0)
	firstByte := true

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	flush := func(s string) {
		_, _ = w.Write([]byte(s))
		if fl != nil {
			fl.Flush()
		}
	}
	for sc.Scan() {
		if r.Context().Err() != nil {
			break
		}
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		c := antigravity.ParseChunk(strings.TrimPrefix(line, "data:"))
		if firstByte && (c.HasData) {
			latencyFirst = time.Since(start).Milliseconds()
			firstByte = false
		}
		for _, t := range c.Texts {
			d := map[string]any{"content": t}
			if !roleSent {
				d["role"] = "assistant"
				roleSent = true
			}
			flush("data: " + mustJSONString(map[string]any{"id": id, "object": "chat.completion.chunk", "created": 0, "model": model, "choices": []any{map[string]any{"index": 0, "delta": d, "finish_reason": nil}}}) + "\n\n")
		}
		for _, tc := range c.ToolCalls {
			idx, ok := toolIdx[tc.ID]
			if !ok {
				idx = nextIdx
				toolIdx[tc.ID] = idx
				nextIdx++
			}
			args, _ := json.Marshal(tc.Arguments)
			d := map[string]any{"tool_calls": []any{map[string]any{"index": idx, "id": tc.ID, "type": "function", "function": map[string]any{"name": tc.Name, "arguments": string(args)}}}}
			if !roleSent {
				d["role"] = "assistant"
				roleSent = true
			}
			flush("data: " + mustJSONString(map[string]any{"id": id, "object": "chat.completion.chunk", "created": 0, "model": model, "choices": []any{map[string]any{"index": 0, "delta": d, "finish_reason": nil}}}) + "\n\n")
		}
		if c.Usage.HasUsage {
			prompt, completion, cacheRead, reasoningTok, total = c.Usage.Input, c.Usage.Output, c.Usage.CacheRead, c.Usage.Reasoning, c.Usage.Total
		}
		if c.Finish != "" && !strings.HasPrefix(c.Finish, "error:") {
			finish = mapAntigravityFinish(c.Finish, len(toolIdx) > 0)
		}
	}
	if len(toolIdx) > 0 && finish == "stop" {
		finish = "tool_calls"
	}
	flush("data: " + mustJSONString(map[string]any{"id": id, "object": "chat.completion.chunk", "created": 0, "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}}) + "\n\n")
	flush("data: [DONE]\n\n")
	cost := antigravityCost(publicID, prompt, completion)
	_ = cacheRead
	_ = reasoningTok
	_ = total
	_ = latencyFirst
	_ = runtime
	h.logRequestExtended(keyPrefix, providerID, model, "chat.completions", http.StatusOK, time.Since(start).Milliseconds(), prompt, completion, cost, true)
	if h.Metrics != nil {
		h.Metrics.IncRequests(providerID, model, "chat.completions", http.StatusOK)
	}
}

func mapAntigravityFinish(fr string, hasTools bool) string {
	switch fr {
	case "STOP":
		if hasTools {
			return "tool_calls"
		}
		return "stop"
	case "MAX_TOKENS":
		return "length"
	default:
		if hasTools {
			return "tool_calls"
		}
		return "stop"
	}
}

func mustJSONString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// serveAntigravityAsAnthropic renders Antigravity output in Anthropic messages SSE/JSON.
func (h *Handler) serveAntigravityAsAnthropic(w http.ResponseWriter, r *http.Request, resp *http.Response, originalBody, _ []byte, model, publicID, runtime, keyPrefix string, start time.Time, providerID string, isStream bool) {
	chunks, err := collectAntigravityChunks(resp)
	if err != nil {
		httperr.Proxy(w, http.StatusBadGateway, "antigravity stream read failed")
		return
	}
	var texts []string
	var toolCalls []antigravity.ParsedToolCall
	prompt, completion := 0, 0
	finish := "end_turn"
	for _, c := range chunks {
		texts = append(texts, c.Texts...)
		toolCalls = append(toolCalls, c.ToolCalls...)
		if c.Usage.HasUsage {
			prompt, completion = c.Usage.Input, c.Usage.Output
		}
		if c.Finish == "MAX_TOKENS" {
			finish = "max_tokens"
		}
	}
	text := strings.Join(texts, "")
	if len(toolCalls) > 0 {
		finish = "tool_use"
	}
	cost := antigravityCost(publicID, prompt, completion)
	if !isStream {
		content := []any{}
		if text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		for _, tc := range toolCalls {
			content = append(content, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": tc.Arguments})
		}
		out := map[string]any{
			"id": "msg_" + uuid.NewString()[:12], "type": "message", "role": "assistant",
			"model": model, "content": content, "stop_reason": finish,
			"usage": map[string]any{"input_tokens": prompt, "output_tokens": completion},
		}
		b, _ := json.Marshal(out)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
		h.logRequestExtended(keyPrefix, providerID, model, "messages", http.StatusOK, time.Since(start).Milliseconds(), prompt, completion, cost, false)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	emit := func(event, data string) {
		_, _ = w.Write([]byte("event: " + event + "\ndata: " + data + "\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}
	msgID := "msg_" + uuid.NewString()[:12]
	emit("message_start", mustJSONString(map[string]any{"type": "message_start", "message": map[string]any{"id": msgID, "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "usage": map[string]any{"input_tokens": prompt, "output_tokens": 0}}}))
	idx := 0
	if text != "" {
		emit("content_block_start", mustJSONString(map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "text", "text": ""}}))
		emit("content_block_delta", mustJSONString(map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "text_delta", "text": text}}))
		emit("content_block_stop", mustJSONString(map[string]any{"type": "content_block_stop", "index": idx}))
		idx++
	}
	for _, tc := range toolCalls {
		emit("content_block_start", mustJSONString(map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": map[string]any{}}}))
		args, _ := json.Marshal(tc.Arguments)
		emit("content_block_delta", mustJSONString(map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(args)}}))
		emit("content_block_stop", mustJSONString(map[string]any{"type": "content_block_stop", "index": idx}))
		idx++
	}
	emit("message_delta", mustJSONString(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": finish}, "usage": map[string]any{"output_tokens": completion}}))
	emit("message_stop", mustJSONString(map[string]any{"type": "message_stop"}))
	h.logRequestExtended(keyPrefix, providerID, model, "messages", http.StatusOK, time.Since(start).Milliseconds(), prompt, completion, cost, true)
}

// serveAntigravityAsResponses renders Antigravity output in OpenAI Responses dialect.
func (h *Handler) serveAntigravityAsResponses(w http.ResponseWriter, r *http.Request, resp *http.Response, model, publicID, runtime, keyPrefix string, start time.Time, providerID string, isStream bool) {
	chunks, err := collectAntigravityChunks(resp)
	if err != nil {
		httperr.Proxy(w, http.StatusBadGateway, "antigravity stream read failed")
		return
	}
	var texts []string
	prompt, completion := 0, 0
	for _, c := range chunks {
		texts = append(texts, c.Texts...)
		if c.Usage.HasUsage {
			prompt, completion = c.Usage.Input, c.Usage.Output
		}
	}
	text := strings.Join(texts, "")
	cost := antigravityCost(publicID, prompt, completion)
	respID := "resp_" + uuid.NewString()
	if !isStream {
		out := map[string]any{
			"id": respID, "object": "response", "created_at": time.Now().Unix(), "model": model, "status": "completed",
			"output": []any{map[string]any{"type": "message", "id": "msg_" + uuid.NewString()[:8], "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}, "status": "completed"}},
			"usage":  map[string]any{"input_tokens": prompt, "output_tokens": completion, "total_tokens": prompt + completion},
		}
		b, _ := json.Marshal(out)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
		h.logRequestExtended(keyPrefix, providerID, model, "responses", http.StatusOK, time.Since(start).Milliseconds(), prompt, completion, cost, false)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	emit := func(obj map[string]any) {
		_, _ = w.Write([]byte("data: " + mustJSONString(obj) + "\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}
	itemID := "msg_" + uuid.NewString()[:8]
	emit(map[string]any{"type": "response.created", "response": map[string]any{"id": respID, "model": model, "status": "in_progress"}})
	emit(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": itemID, "type": "message", "role": "assistant", "content": []any{}}})
	emit(map[string]any{"type": "response.content_part.added", "item_id": itemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
	if text != "" {
		emit(map[string]any{"type": "response.output_text.delta", "item_id": itemID, "output_index": 0, "content_index": 0, "delta": text})
		emit(map[string]any{"type": "response.output_text.done", "item_id": itemID, "output_index": 0, "content_index": 0, "text": text})
	}
	emit(map[string]any{"type": "response.content_part.done", "item_id": itemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}}})
	emit(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"id": itemID, "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}, "status": "completed"}})
	emit(map[string]any{"type": "response.completed", "response": map[string]any{"id": respID, "model": model, "status": "completed", "usage": map[string]any{"input_tokens": prompt, "output_tokens": completion, "total_tokens": prompt + completion}}})
	h.logRequestExtended(keyPrefix, providerID, model, "responses", http.StatusOK, time.Since(start).Milliseconds(), prompt, completion, cost, true)
	_ = fmt.Sprintf
	_ = runtime
}
