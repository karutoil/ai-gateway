package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/cache"
	"ai-gateway/internal/catalog"
	"ai-gateway/internal/httperr"
	"ai-gateway/internal/lb"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/otel"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/resilience"
	"ai-gateway/internal/translate"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// UsageSink receives post-response usage so budgets reconcile against real
// upstream counts instead of only pre-request estimates.
type UsageSink interface {
	RecordUsage(keyPrefix string, orgID *string, tokens int, costUSD float64, ts time.Time)
}

type Handler struct {
	ProviderStore *provider.Store
	CatalogStore  *catalog.Store
	DB            *sql.DB
	Client        *http.Client
	Cache         cache.Cache
	Retry         resilience.RetryPolicy
	Breaker       resilience.CircuitBreaker
	Metrics       otel.Metrics
	RateLimiter   middleware.Limiter

	Timeouts TimeoutsConfig
	// LB, when set, routes bare model names through operator-curated
	// round-robin groups (see internal/lb). Nil = no curated routing.
	LB *lb.Store
	// CacheTTLSeconds bounds non-stream completion caching (default 10).
	CacheTTLSeconds int
	// Usage, when set, records actual per-request token/cost outcomes.
	Usage UsageSink
	// LogBodies enables captured request/response body logging (opt-in),
	// truncated to BodyLogMaxBytes and credential-scrubbed.
	LogBodies       bool
	BodyLogMaxBytes int
	// StreamUsageInject injects stream_options.include_usage for OpenAI-style
	// streams when missing (billing accuracy over maximum compatibility).
	StreamUsageInject bool
}

func New(ps *provider.Store, db *sql.DB) *Handler {
	tc := DefaultTimeouts()
	return &Handler{
		ProviderStore:   ps,
		DB:              db,
		Client:          GatewayHTTPClient(NewGatewayTransport(tc.UpstreamHeader, true)),
		Timeouts:        tc,
		CacheTTLSeconds: 10,
		BodyLogMaxBytes: 8192,
	}
}

func NewWithCatalog(ps *provider.Store, cs *catalog.Store, db *sql.DB) *Handler {
	h := New(ps, db)
	h.CatalogStore = cs
	return h
}

func (h *Handler) resolveAlias(model string) string {
	// direct alias
	if h.DB != nil {
		var target string
		err := h.DB.QueryRow(`SELECT target FROM model_aliases WHERE alias=?`, model).Scan(&target)
		if err == nil && target != "" {
			return target
		}
		// strip opencode/ prefix fallback
		if strings.HasPrefix(model, "opencode/") {
			trimmed := strings.TrimPrefix(model, "opencode/")
			// try alias of trimmed
			err = h.DB.QueryRow(`SELECT target FROM model_aliases WHERE alias=?`, trimmed).Scan(&target)
			if err == nil && target != "" {
				return target
			}
			// just return trimmed
			return trimmed
		}
		// also try meta/ prefix for ckff?
	}
	return model
}

func (h *Handler) enforceModelAllowlist(w http.ResponseWriter, r *http.Request, rawModel, resolvedModel string) bool {
	key, ok := middleware.GatewayKeyFromContext(r.Context())
	if !ok || key == nil || len(key.AllowedModels) == 0 {
		return true
	}
	if apikey.IsModelAllowed(key.AllowedModels, resolvedModel) || apikey.IsModelAllowed(key.AllowedModels, rawModel) {
		return true
	}
	http.Error(w, `{"error":{"message":"model not allowed for this key","type":"invalid_request_error"}}`, http.StatusForbidden)
	return false
}

func estimateTokens(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	// cheap heuristic: ~4 chars per token; used for TPM pre-check only.
	// Base64 payloads (inline images, audio, files) are excluded BEFORE
	// counting — a 900KB data-URL previously registered ~225K "tokens" and
	// spurious 429'd multimodal clients long before the upstream saw bytes.
	return (len(stripBase64Blobs(body)) + 3) / 4
}

// stripBase64Blobs removes embedded base64 runs (>=256 chars) from a payload.
func stripBase64Blobs(body []byte) []byte {
	const minRun = 256
	isB64 := func(c byte) bool {
		return c == '+' || c == '/' || c == '=' ||
			('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z') || ('0' <= c && c <= '9')
	}
	var out []byte
	i := 0
	for i < len(body) {
		if isB64(body[i]) {
			j := i
			for j < len(body) && isB64(body[j]) {
				j++
			}
			if j-i >= minRun {
				out = append(out, '.')
				i = j
				continue
			}
			out = append(out, body[i:j]...)
			i = j
			continue
		}
		out = append(out, body[i])
		i++
	}
	return out
}

func (h *Handler) checkTPM(w http.ResponseWriter, r *http.Request, body []byte) bool {
	if h.RateLimiter == nil {
		return true
	}
	key, ok := middleware.GatewayKeyFromContext(r.Context())
	if !ok || key == nil || key.RateLimitTPM <= 0 {
		return true
	}
	tokens := estimateTokens(body)
	if tokens == 0 {
		return true
	}
	prefix := key.Prefix
	if prefix == "" {
		prefix = r.Header.Get("X-Gateway-Key-Prefix")
	}
	if prefix == "" {
		return true
	}
	if !h.RateLimiter.AllowTokens(prefix, tokens, key.RateLimitTPM) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, `{"error":{"message":"token rate limit exceeded","type":"rate_limit_error"}}`, http.StatusTooManyRequests)
		return false
	}
	return true
}

// no-op implementations to keep Handler fields wired even when not fully used in proxyWithMetrics
type noopCache struct{}

func (n *noopCache) Get(key string) ([]byte, int, http.Header, bool)                              { return nil, 0, nil, false }
func (n *noopCache) Set(key string, body []byte, status int, headers http.Header, ttlSeconds int) {}
func (n *noopCache) Invalidate(pattern string)                                                    {}

type noopBreaker struct{}

func (n *noopBreaker) Allow(string) bool   { return true }
func (n *noopBreaker) Record(string, int)  {}
func (n *noopBreaker) State(string) string { return "closed" }

func (h *Handler) cacheOrNoop() cache.Cache {
	if h.Cache != nil {
		return h.Cache
	}
	return &noopCache{}
}
func (h *Handler) breakerOrNoop() resilience.CircuitBreaker {
	if h.Breaker != nil {
		return h.Breaker
	}
	return &noopBreaker{}
}
func (h *Handler) retryOrDefault() resilience.RetryPolicy {
	if h.Retry != nil {
		return h.Retry
	}
	return resilience.NewDefaultRetryPolicy()
}

func extractUsage(body []byte) (prompt, completion int) {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return 0, 0
	}
	if usage, ok := m["usage"].(map[string]interface{}); ok {
		if v, ok := usage["prompt_tokens"]; ok {
			prompt = toInt(v)
		} else if v, ok := usage["input_tokens"]; ok {
			prompt = toInt(v)
		} else if v, ok := usage["promptTokens"]; ok {
			prompt = toInt(v)
		}
		if v, ok := usage["completion_tokens"]; ok {
			completion = toInt(v)
		} else if v, ok := usage["output_tokens"]; ok {
			completion = toInt(v)
		} else if v, ok := usage["completionTokens"]; ok {
			completion = toInt(v)
		}
		return
	}
	if v, ok := m["usage"]; ok {
		if um, ok := v.(map[string]interface{}); ok {
			if iv, ok := um["input_tokens"]; ok {
				prompt = toInt(iv)
			}
			if ov, ok := um["output_tokens"]; ok {
				completion = toInt(ov)
			}
		}
	}
	return
}

func toInt(v interface{}) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		i, _ := x.Int64()
		return int(i)
	}
	return 0
}

func cacheKeyFor(endpoint, model string, body []byte, scope string) string {
	sum := sha256.Sum256(append([]byte(endpoint+"\n"+scope+"\n"+model+"\n"), body...))
	return "post:" + hex.EncodeToString(sum[:])
}

// cacheScope identifies the tenant (and resolved provider) a cached completion
// belongs to. Without it any key whose allowlist permitted the same model could
// be served another tenant's byte-identical cached completion.
func cacheScopeFor(r *http.Request, providerID string) string {
	prefix := "anon"
	org := ""
	if k, ok := middleware.GatewayKeyFromContext(r.Context()); ok && k != nil {
		if k.Prefix != "" {
			prefix = k.Prefix
		}
		if k.OrgID != nil {
			org = *k.OrgID
		}
	}
	return prefix + "|" + org + "|" + providerID
}

func modelsCacheKey(r *http.Request) string {
	prefix := "anon"
	allow := ""
	if k, ok := middleware.GatewayKeyFromContext(r.Context()); ok && k != nil {
		if k.Prefix != "" {
			prefix = k.Prefix
		}
		if len(k.AllowedModels) > 0 {
			allow = strings.Join(k.AllowedModels, ",")
		}
	}
	return "models:" + prefix + ":" + allow
}

func retryAfterDelay(hdr http.Header, fallback time.Duration) time.Duration {
	if hdr == nil {
		return fallback
	}
	ra := hdr.Get("Retry-After")
	if ra == "" {
		return fallback
	}
	secs, err := strconv.Atoi(strings.TrimSpace(ra))
	if err != nil || secs < 0 {
		return fallback
	}
	d := time.Duration(secs) * time.Second
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

// hopByHopHeaders are response headers owned by the upstream CONNECTION and
// must never be forwarded to gateway clients (RFC 7230 §6.1).
var hopByHopHeaders = map[string]bool{
	"connection": true, "proxy-connection": true, "keep-alive": true,
	"proxy-authenticate": true, "proxy-authorization": true, "te": true,
	"trailer": true, "transfer-encoding": true, "upgrade": true,
}

func copyHeader(dst, src http.Header) {
	// Headers named in a Connection: header are hop-by-hop too.
	for _, tok := range strings.Split(src.Get("Connection"), ",") {
		if t := strings.ToLower(strings.TrimSpace(tok)); t != "" {
			hopByHopHeaders[t] = true
		}
	}
	for k, vv := range src {
		if hopByHopHeaders[strings.ToLower(k)] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func (h *Handler) serveCacheHit(w http.ResponseWriter, body []byte, status int, headers http.Header) {
	copyHeader(w.Header(), headers)
	w.Header().Set("X-Cache", "HIT")
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	if h.Metrics != nil {
		h.Metrics.IncCacheHit(true)
	}
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	w.Write(body)
}

func (h *Handler) writeJSONCached(w http.ResponseWriter, cacheKey string, ttl int, payload any) {
	b, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	w.Write(b)
	if cacheKey != "" && len(b) <= 1<<20 {
		h.cacheOrNoop().Set(cacheKey, b, http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}, ttl)
	}
}

// upstreamForwardHeaders is an ALLOWLIST of client headers proxied to
// upstreams. The old code used a deny-list for auth/gateway hops only, so
// cookies, arbitrary SDK telemetry, anthropic-version (against OpenAI targets)
// and other residue rode along when fallback switched providers mid-request.
var upstreamForwardHeaders = map[string]bool{
	"accept":          true,
	"accept-language": true,
	"user-agent":      true,
	"x-request-id":    true,
}

func (h *Handler) newUpstreamRequest(ctx context.Context, r *http.Request, targetURL, apiKey string, body []byte, isStream, isAnthropicUpstream bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, r.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vv := range r.Header {
		lk := strings.ToLower(k)
		switch lk {
		case "authorization", "host", "content-length", "cookie":
			continue
		}
		if !upstreamForwardHeaders[lk] {
			continue
		}
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	if isAnthropicUpstream {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Content-Type", "application/json")
	if isStream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	return req, nil
}

// sleepCtx sleeps for d but wakes immediately on ctx cancellation.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// proxyOpts carries per-invocation metadata through candidate fallback chains.
type proxyOpts struct {
	// fallbackFrom, when non-empty, names the primary provider so the
	// X-Fallback-Used response header can be applied BEFORE any bytes are
	// committed (streams included).
	fallbackFrom string
	// translatedResponses marks that the outbound body was a translation of an
	// OpenAI Responses request onto a chat/anthropic backend, so the returned
	// JSON must be converted back into Responses shape (non-streaming).
	translatedResponses bool
}

// attemptOutcome describes how one proxyWithMetrics invocation ended.
type attemptOutcome struct {
	// committed: response bytes were written to the client (headers sent).
	committed bool
	// status: best-known upstream HTTP status (0 = transport-level failure).
	status int
	// retriable: caller may advance to another candidate/provider.
	retriable bool
}

// proxyWithMetrics handles both anthropic and openai upstreams correctly.
//
// Retry/fallback rules (post-hardening):
//   - NOTHING reaches the client until a usable upstream response exists.
//     Streams retry/failover exactly like buffered calls as long as headers
//     have not been committed — the gateway buffers only headers (never body
//     bytes) while deciding.
//   - Mid-stream failures terminate the SSE channel with protocol-correct
//     error frames ([DONE]/anthropic error event), record honest outcomes and
//     feed the circuit breaker.
func (h *Handler) proxyWithMetrics(w http.ResponseWriter, r *http.Request, targetURL string, apiKey string, body []byte, isStream bool, model string, providerID string, keyPrefix string, endpoint string, start time.Time, isAnthropicUpstream bool) attemptOutcome {
	return h.proxyWithMetricsOpts(w, r, targetURL, apiKey, body, isStream, model, providerID, keyPrefix, endpoint, start, isAnthropicUpstream, proxyOpts{})
}

func (h *Handler) proxyWithMetricsOpts(w http.ResponseWriter, r *http.Request, targetURL, apiKey string, body []byte, isStream bool, model, providerID, keyPrefix, endpoint string, start time.Time, isAnthropicUpstream bool, opts proxyOpts) attemptOutcome {
	c := h.cacheOrNoop()
	breaker := h.breakerOrNoop()
	retry := h.retryOrDefault()

	cacheTTL := h.CacheTTLSeconds
	if cacheTTL <= 0 {
		cacheTTL = 10
	}

	cacheKey := ""
	if !isStream && len(body) <= 1<<20 {
		cacheKey = cacheKeyFor(endpoint, model, body, cacheScopeFor(r, providerID))
		if cached, status, headers, ok := c.Get(cacheKey); ok && status >= 200 && status < 400 {
			h.serveCacheHit(w, cached, status, headers)
			h.logRequestExtended(keyPrefix, providerID, model, endpoint, status, time.Since(start).Milliseconds(), 0, 0, 0, false)
			return attemptOutcome{committed: true, status: status}
		}
		if h.Metrics != nil {
			h.Metrics.IncCacheHit(false)
		}
	}

	if !breaker.Allow(providerID) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":{"message":"circuit open for provider","type":"proxy_error","code":"circuit_open"}}`))
		h.logRequestExtended(keyPrefix, providerID, model, endpoint, http.StatusServiceUnavailable, time.Since(start).Milliseconds(), 0, 0, 0, isStream)
		return attemptOutcome{committed: true, status: http.StatusServiceUnavailable}
	}

	ctx := r.Context()
	var cancelFn context.CancelFunc
	if h.Timeouts.RequestTotal > 0 {
		ctx, cancelFn = context.WithTimeout(ctx, h.Timeouts.RequestTotal)
		defer cancelFn()
	}

	applyFallbackHeader := func(hdr http.Header) {
		if opts.fallbackFrom != "" {
			hdr.Set("X-Fallback-Used", opts.fallbackFrom)
		}
	}

	// Note on breaker accounting: mid-stream transport deaths feed the breaker
	// with status 599 (classified as failure); clean completions record the
	// real header status. Pre-commit failures use their actual status.
	const midStreamFailStatus = 599

	var (
		lastStatus int
		lastErr    error
		lastHdr    http.Header
		lastBody   []byte
	)

	for attempt := 0; ; attempt++ {
		req, err := h.newUpstreamRequest(ctx, r, targetURL, apiKey, body, isStream, isAnthropicUpstream)
		if err != nil {
			httperr.Write(w, http.StatusInternalServerError, "failed to create upstream request", httperr.TypeProxy)
			return attemptOutcome{committed: true, status: http.StatusInternalServerError}
		}
		resp, err := h.Client.Do(req)
		if err != nil || (resp != nil && resp.StatusCode >= 500) || (resp != nil && resp.StatusCode == 429) {
			// Upstream unusable and NOTHING has been committed yet — both
			// streams and buffered requests can recover here.
			status := 0
			if err == nil {
				status = resp.StatusCode
				io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
				resp.Body.Close()
				breaker.Record(providerID, status)
			} else {
				breaker.Record(providerID, 0)
			}
			lastErr, lastStatus, lastHdr, lastBody = err, status, nil, nil
			if retry.ShouldRetry(attempt, retryableCode(status)) {
				sleepCtx(ctx, retryAfterDelay(nil, retry.Backoff(attempt)))
				continue
			}
			if err != nil {
				// Distinguish client-cancelled from genuinely dead upstream.
				if ctx.Err() != nil {
					return attemptOutcome{committed: false, status: status, retriable: false}
				}
				return attemptOutcome{committed: false, status: 0, retriable: true}
			}
			return attemptOutcome{committed: false, status: status, retriable: true}
		}

		lastStatus = resp.StatusCode
		lastHdr = resp.Header.Clone()
		lastErr = nil

		if isStream {
			out := h.pumpStream(w, r, req.Context(), resp, model, keyPrefix, providerID, endpoint, start, isAnthropicUpstream, applyFallbackHeader)
			if out.midStreamFailure {
				breaker.Record(providerID, midStreamFailStatus)
			} else {
				breaker.Record(providerID, lastStatus)
			}
			return attemptOutcome{committed: true, status: lastStatus}
		}

		bodyBytes, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		lastBody = bodyBytes
		if readErr != nil && len(bodyBytes) == 0 {
			breaker.Record(providerID, 0)
			lastErr, lastStatus = readErr, 0
			if retry.ShouldRetry(attempt, 0) {
				sleepCtx(ctx, retryAfterDelay(nil, retry.Backoff(attempt)))
				continue
			}
			return attemptOutcome{committed: false, status: 0, retriable: true}
		}
		if retry.ShouldRetry(attempt, resp.StatusCode) {
			breaker.Record(providerID, resp.StatusCode)
			sleepCtx(r.Context(), retryAfterDelay(resp.Header, retry.Backoff(attempt)))
			continue
		}
		breaker.Record(providerID, resp.StatusCode)
		break
	}

	// Terminal failure path: every attempt failed pre-commit.
	if lastStatus == 0 && len(lastBody) == 0 {
		log.Error().Err(lastErr).Str("target", targetURL).Msg("upstream error")
		httperr.Proxy(w, http.StatusBadGateway, "upstream unavailable")
		h.logRequestExtended(keyPrefix, providerID, model, endpoint, 502, time.Since(start).Milliseconds(), 0, 0, 0, isStream)
		return attemptOutcome{committed: true, status: http.StatusBadGateway}
	}

	{
		pt, ct := extractUsage(lastBody)
		if pt == 0 && ct == 0 {
			pt, ct = extractAnthropicUsageFromSSEChunk(lastBody)
		}
		cost := h.costForModel(model, pt, ct)
		outBody := lastBody

		needsChatShape := endpoint == "chat.completions" && isAnthropicUpstream && lastStatus == 200
		if needsChatShape {
			translated := anthropicToOpenAIChatResponse(lastBody, model)
			if translated != nil {
				pt2, ct2 := extractUsage(translated)
				if pt2 != 0 || ct2 != 0 {
					pt, ct = pt2, ct2
					cost = h.costForModel(model, pt, ct)
				}
				outBody = translated
			}
		}

		needsResponsesShape := opts.translatedResponses && endpoint == "responses" && lastStatus == 200
		if needsResponsesShape {
			if converted := chatToResponsesJSON(outBody, model); converted != nil {
				if pt2, ct2 := extractUsage(converted); pt2 != 0 || ct2 != 0 {
					pt, ct = pt2, ct2
					cost = h.costForModel(model, pt, ct)
				}
				outBody = converted
			}
		}

		// Legacy /v1/completions clients need text_completion shape
		// (choices[].text), never a raw Anthropic message body.
		if endpoint == "completions" && isAnthropicUpstream && lastStatus == 200 {
			if converted := anthropicToOpenAICompletion(outBody, model); converted != nil {
				if pt2, ct2 := extractUsage(converted); pt2 != 0 || ct2 != 0 {
					pt, ct = pt2, ct2
					cost = h.costForModel(model, pt, ct)
				}
				outBody = converted
			}
		}

		h.recordUsage(keyPrefix, r, pt+ct, cost)

		if cacheKey != "" && lastStatus >= 200 && lastStatus < 400 && len(outBody) <= 1<<20 {
			hdr := http.Header{}
			copyHeader(hdr, lastHdr)
			hdr.Set("Content-Type", "application/json")
			c.Set(cacheKey, outBody, lastStatus, hdr, cacheTTL)
		}
		copyHeader(w.Header(), lastHdr)
		applyFallbackHeader(w.Header())
		// Body-rewriting paths (anthropic→openai conversions, responses
		// translation) change payload size; a copied stale Content-Length makes
		// clients hang or truncate ("unexpected EOF"). Drop it when rewritten.
		if len(outBody) != len(lastBody) {
			w.Header().Del("Content-Length")
		}
		w.Header().Set("X-Cache", "MISS")
		if (needsChatShape || needsResponsesShape) && lastStatus == 200 {
			w.Header().Set("Content-Type", "application/json")
		}
		h.logRequestExtendedBodies(keyPrefix, providerID, model, endpoint, lastStatus, time.Since(start).Milliseconds(), pt, ct, cost, false, body, outBody)
		w.WriteHeader(lastStatus)
		w.Write(outBody)
		return attemptOutcome{committed: true, status: lastStatus}
	}
}

// retryableCode maps transport failures to a synthetic retry-polling code.
func retryableCode(status int) int {
	if status == 0 {
		return 503
	}
	return status
}

// injectStreamUsage adds stream_options.include_usage to an OpenAI-style
// streaming body when absent and configured. Failures fall back silently.
func injectStreamUsage(body []byte) ([]byte, bool) {
	var m map[string]interface{}
	if json.Unmarshal(body, &m) != nil {
		return body, false
	}
	if m["stream"] != true {
		return body, false
	}
	so, ok := m["stream_options"].(map[string]interface{})
	switch {
	case !ok:
		m["stream_options"] = map[string]interface{}{"include_usage": true}
	default:
		if v, exists := so["include_usage"]; exists {
			if b, isBool := v.(bool); isBool && b {
				return body, false
			}
		}
		so["include_usage"] = true
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

// streamPumpResult carries what pumpStream harvested.
type streamPumpResult struct {
	bodySnapshot     []byte // sampled stream bytes (bounded) for debug bodies
	promptTokens     int
	completionTokens int
	midStreamFailure bool
	clientGone       bool
}

// pumpStream relays an SSE body to the client with:
//   - idle-chunk watchdog (no more hard ceiling killing long generations),
//   - framing-aware SSE parsing across TCP chunk boundaries,
//   - protocol-correct termination on ANY abnormal exit,
//   - honest downstream accounting (usage-so-far, real outcome status).
func (h *Handler) pumpStream(w http.ResponseWriter, r *http.Request, upstreamCtx context.Context, resp *http.Response, model, keyPrefix, providerID, endpoint string, start time.Time, isAnthropicUpstream bool, applyFallbackHeader func(http.Header)) streamPumpResult {
	res := streamPumpResult{}

	commit := func() {
		copyHeader(w.Header(), resp.Header)
		applyFallbackHeader(w.Header())
		w.Header().Set("X-Cache", "MISS")
		w.WriteHeader(resp.StatusCode)
	}
	// First flusher probe AFTER commit: statusRecorder + chi wrappers all implement Flush.
	flusher, _ := w.(http.Flusher)

	idle := h.Timeouts.StreamIdle
	timer := time.NewTimer(idle)
	if idle <= 0 {
		timer.Stop() // disabled watchdog
	}
	defer timer.Stop()

	type chunkMsg struct {
		data [8192]byte
		n    int
		err  error
	}
	chunks := make(chan chunkMsg) // sized copies avoid aliasing
	go func() {
		for {
			var cm chunkMsg
			n, err := resp.Body.Read(cm.data[:])
			cm.n, cm.err = n, err
			select {
			case chunks <- cm:
			case <-upstreamCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var pending []byte // partial SSE event carry-over between reads
	var promptTok, completeTok int
	sample := &bytes.Buffer{}
	sampleCap := h.BodyLogMaxBytes
	if sampleCap > 32<<10 {
		sampleCap = 32 << 10
	}

	harvest := func(frame []byte) {
		events := parseSSEEvents(frame)
		for _, ev := range events {
			if sample.Cap() > 0 && sample.Len() < sampleCap {
				snapshot := make([]byte, 0, len(ev.data)+len(ev.name)+16)
				if ev.name != "" {
					snapshot = append(snapshot, []byte(ev.name+": ")...)
				}
				snapshot = append(snapshot, ev.data...)
				snapshot = append(snapshot, '\n')
				if sample.Len()+len(snapshot) > sampleCap {
					snapshot = snapshot[:sample.Cap()-sample.Len()]
				}
				sample.Write(snapshot)
			}
			if bytes.Equal(bytes.TrimSpace(ev.data), []byte("[DONE]")) {
				continue
			}
			pt, ct := extractUsage(ev.data)
			if pt > 0 {
				promptTok = pt
			}
			if ct > 0 {
				completeTok = ct
			}
			// Anthropic puts input tokens on message_start and output deltas on
			// message_delta; harvest wherever present.
			aPt, aCt := harvestAnthropicTokens(ev.data)
			if aPt > 0 {
				promptTok = aPt
			}
			if aCt > 0 {
				completeTok = aCt
			}
		}
	}

	// Terminators and reframing must speak the CLIENT's dialect, not the
	// upstream's: chat.completions/completions clients receive OpenAI frames
	// even when the upstream is an anthropic translation target.
	clientDialectAnthropic := isAnthropicUpstream && endpoint == "messages"
	// Reframe anthropic-dialect SSE into legacy OpenAI completion chunks for
	// /v1/completions callers routed onto an anthropic upstream (comp-D2).
	var reframe *anthToOpenAICompletionStream
	if isAnthropicUpstream && endpoint == "completions" {
		reframe = newAnthToOpenAICompletionStream(model)
	}

	fail := func(reason string, clientGone bool) streamPumpResult {
		res.midStreamFailure = true
		res.clientGone = clientGone
		res.promptTokens, res.completionTokens = promptTok, completeTok
		res.bodySnapshot = sample.Bytes()
		cost := h.costForModel(model, promptTok, completeTok)
		logStatus := http.StatusBadGateway
		if clientGone {
			logStatus = 499 // nginx convention: client closed request
		}
		if !clientGone {
			writeStreamTerminator(w, flusher, clientDialectAnthropic, reason)
		}
		log.Error().Str("model", model).Str("provider", providerID).Str("reason", reason).Bool("client_gone", clientGone).Msg("mid-stream failure terminated")
		h.recordUsage(keyPrefix, r, promptTok+completeTok, cost)
		h.logRequestExtendedBodies(keyPrefix, providerID, model, endpoint, logStatus, time.Since(start).Milliseconds(), promptTok, completeTok, cost, true, nil, sample.Bytes())
		return res
	}

	first := true
	finishClean := func() streamPumpResult {
		// Parse any trailing partial event buffered without a final newline.
		if len(pending) > 0 {
			harvest(pending)
			pending = nil
		}
		res.promptTokens, res.completionTokens = promptTok, completeTok
		res.bodySnapshot = sample.Bytes()
		cost := h.costForModel(model, promptTok, completeTok)
		if reframe != nil {
			if tail := reframe.finish(); len(tail) > 0 {
				w.Write(tail)
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		h.recordUsage(keyPrefix, r, promptTok+completeTok, cost)
		h.logRequestExtendedBodies(keyPrefix, providerID, model, endpoint, resp.StatusCode, time.Since(start).Milliseconds(), promptTok, completeTok, cost, true, nil, sample.Bytes())
		return res
	}

	for {
		select {
		case <-r.Context().Done():
			return fail("gateway client disconnected", true)

		case <-timer.C:
			resp.Body.Close() // unblocks the pump goroutine
			drainChan(chunks)
			if idle > 0 {
				return fail("upstream idle timeout: no data received within "+idle.String(), false)
			}
			// Watchdog disabled → timer.C can only be the stopped zero-timer,
			// which never fires; this branch is unreachable but keeps the
			// compiler honest.
			continue

		case cm := <-chunks:
			if idle > 0 && !first || cm.n > 0 {
				timer.Reset(idle)
			}

			// Process payload FIRST: Go's Read may deliver n>0 together with
			// err==io.EOF on the same call.
			if cm.n > 0 {
				buf := cm.data[:cm.n]
				if first {
					// Pre-commit sanity: never present an HTML error splash (or any
					// non-SSE payload) as a "successful" event stream to SSE clients.
					ct := resp.Header.Get("Content-Type")
					trimmed := bytes.TrimLeft(buf, " \t\r\n")
					sseish := strings.HasPrefix(ct, "text/event-stream") ||
						bytes.HasPrefix(trimmed, []byte("data:")) ||
						bytes.HasPrefix(trimmed, []byte("event:")) ||
						bytes.HasPrefix(trimmed, []byte(":"))
					if !sseish {
						resp.Body.Close()
						drainChan(chunks)
						res.bodySnapshot = append(res.bodySnapshot[:0], buf...)
						log.Error().Str("model", model).Str("provider", providerID).
							Str("content_type", ct).Msg("upstream returned non-SSE payload for streaming request")
						h.logRequestExtendedBodies(keyPrefix, providerID, model, endpoint,
							http.StatusBadGateway, time.Since(start).Milliseconds(), 0, 0, 0, true, nil, buf)
						res.midStreamFailure = true
						// Nothing committed: answer honestly instead of an
						// implicit empty 200.
						httperr.Proxy(w, http.StatusBadGateway, "upstream returned a non-stream payload for a streaming request")
						return res
					}
					first = false
					commit()
				}
				if reframe != nil {
					// Rewrite the upstream dialect into client chunks instead of
					// relaying raw anthropic frames.
					if out := reframe.consume(buf); len(out) > 0 {
						w.Write(out)
						if flusher != nil {
							flusher.Flush()
						}
					}
					pending = append(pending, buf...) // raw kept for harvest/sample
				} else {
					w.Write(buf)
					if flusher != nil {
						flusher.Flush()
					}
					pending = append(pending, buf...)
				}
				if idx := bytes.LastIndexByte(pending, '\n'); idx >= 0 {
					frame := pending[:idx+1]
					harvest(frame)
					rest := append([]byte(nil), pending[idx+1:]...)
					pending = rest
				}
				if len(pending) > 1<<20 { // runaway non-SSE payload guard
					harvest(pending)
					pending = pending[:0]
				}
			}

			if cm.err != nil {
				if isCleanEOF(cm.err) {
					if first {
						// Zero bytes AND EOF before any data: upstream accepted
						// the connection then produced nothing usable.
						resp.Body.Close()
						drainChan(chunks)
						return fail("upstream produced no stream data", false)
					}
					resp.Body.Close()
					return finishClean()
				}
				resp.Body.Close()
				drainChan(chunks)
				return fail("upstream stream error: "+cm.err.Error(), false)
			}
		}
	}
}

func isCleanEOF(err error) bool {
	// ONLY a true io.EOF means the upstream finished its stream.
	// io.ErrUnexpectedEOF indicates cut framing; treating it as clean
	// terminated streams WITHOUT a protocol terminator and logged fake
	// successes (anth-D2).
	return err == io.EOF
}

// drainChan empties a channel without blocking so its producer goroutine can exit.
func drainChan[T any](ch <-chan T) {
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

func isUnexpectedEOF(err error) bool {
	return err != nil && (err == io.ErrUnexpectedEOF || strings.Contains(err.Error(), "unexpected EOF"))
}

// harvestAnthropicTokens extracts anthropic usage fields wherever they appear
// inside a JSON event payload (message_start carries input_tokens,
// message_delta/output_tokens arrive cumulatively).
func harvestAnthropicTokens(data []byte) (prompt, completion int) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return 0, 0
	}
	var m map[string]interface{}
	if json.Unmarshal(trimmed, &m) != nil {
		return 0, 0
	}
	var find func(node interface{}, depth int)
	find = func(node interface{}, depth int) {
		if depth > 4 {
			return
		}
		obj, ok := node.(map[string]interface{})
		if !ok {
			return
		}
		if u, ok := obj["usage"].(map[string]interface{}); ok {
			if v, ok := u["input_tokens"]; ok && toInt(v) > prompt {
				prompt = toInt(v)
			}
			if v, ok := u["output_tokens"]; ok && toInt(v) > completion {
				completion = toInt(v)
			}
		}
		for _, child := range obj {
			find(child, depth+1)
		}
	}
	find(m, 0)
	return
}

// chatToResponsesJSON converts a translated chat.completion payload back into
// OpenAI Responses-API shape so /v1/responses clients always receive the
// protocol they asked for (streaming variants are refused earlier — wrong-
// shaped events are worse than a clear error).
func chatToResponsesJSON(body []byte, model string) []byte {
	var chat map[string]interface{}
	if json.Unmarshal(body, &chat) != nil {
		return nil
	}
	choices, ok := chat["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return nil
	}
	first, _ := choices[0].(map[string]interface{})
	msg, _ := first["message"].(map[string]interface{})
	text, _ := msg["content"].(string)
	usage, _ := chat["usage"].(map[string]interface{})
	inTok, outTok := 0, 0
	if usage != nil {
		if v, ok := usage["prompt_tokens"]; ok {
			inTok = toInt(v)
		}
		if v, ok := usage["completion_tokens"]; ok {
			outTok = toInt(v)
		}
	}
	id, _ := chat["id"].(string)
	if id == "" {
		id = "resp-" + uuid.NewString()
	}
	finish, _ := first["finish_reason"].(string)
	status := "completed"
	if finish != "" && finish != "stop" && finish != "tool_calls" {
		status = "incomplete"
	}
	respObj := map[string]interface{}{
		"id":         id,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     status,
		"model":      model,
		"output": []map[string]interface{}{
			{
				"type":   "message",
				"id":     "msg_" + uuid.NewString()[:8],
				"role":   "assistant",
				"status": "completed",
				"content": []map[string]interface{}{
					{"type": "output_text", "text": text, "annotations": []interface{}{}},
				},
			},
		},
		"output_text": text,
		"usage": map[string]interface{}{
			"input_tokens":  inTok,
			"output_tokens": outTok,
			"total_tokens":  inTok + outTok,
		},
	}
	out, err := json.Marshal(respObj)
	if err != nil {
		return nil
	}
	return out
}

// recordUsage forwards real usage to the budget ledger sink when wired.
func (h *Handler) recordUsage(keyPrefix string, r *http.Request, tokens int, costUSD float64) {
	if h.Usage == nil || (tokens == 0 && costUSD == 0) {
		return
	}
	var orgPtr *string
	if k, ok := middleware.GatewayKeyFromContext(r.Context()); ok && k != nil && k.OrgID != nil {
		orgPtr = k.OrgID
	}
	h.Usage.RecordUsage(keyPrefix, orgPtr, tokens, costUSD, time.Now().UTC())
}

// logRequestExtended is preserved for legacy callers/tests.
func (h *Handler) logRequestExtended(keyPrefix, providerID, model, endpoint string, status int, latencyMs int64, promptTokens, completionTokens int, costUSD float64, isStream bool) {
	h.logRequestExtendedBodies(keyPrefix, providerID, model, endpoint, status, latencyMs, promptTokens, completionTokens, costUSD, isStream, nil, nil)
}

// logRequestExtendedBodies persists a completed exchange. When handler body
// logging is enabled (opt-in), captured payloads are truncated and scrubbed of
// credential material before insertion.
func (h *Handler) logRequestExtendedBodies(keyPrefix, providerID, model, endpoint string, status int, latencyMs int64, promptTokens, completionTokens int, costUSD float64, isStream bool, requestBody, responseBody []byte) {
	if h.Metrics != nil {
		h.Metrics.IncRequests(providerID, model, endpoint, status)
		h.Metrics.ObserveLatency(providerID, endpoint, time.Duration(latencyMs)*time.Millisecond)
	}
	if h.DB == nil {
		return
	}
	id := uuid.NewString()
	total := promptTokens + completionTokens

	reqBodyStr := ""
	respBodyStr := ""
	if h.LogBodies {
		cap_ := h.BodyLogMaxBytes
		if cap_ <= 0 {
			cap_ = 8192
		}
		if len(requestBody) > 0 {
			b := requestBody
			if len(b) > cap_ {
				b = b[:cap_]
			}
			reqBodyStr = ScrubSecrets(string(b))
		}
		if len(responseBody) > 0 {
			b := responseBody
			if len(b) > cap_ {
				b = b[:cap_]
			}
			respBodyStr = ScrubSecrets(string(b))
		}
	}
	h.DB.Exec(`INSERT INTO request_logs(id,key_prefix,provider_id,model,endpoint,status,latency_ms,created_at,prompt_tokens,completion_tokens,total_tokens,cost_usd,is_stream,request_body,response_body) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, keyPrefix, providerID, model, endpoint, status, latencyMs, time.Now().UTC(), promptTokens, completionTokens, total, costUSD, isStream, nullIfEmpty(reqBodyStr), nullIfEmpty(respBodyStr))
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func extractAnthropicUsageFromSSEChunk(chunk []byte) (prompt, completion int) {
	// look for "input_tokens": X and "output_tokens": Y in chunk
	// naive json extract
	var tmp map[string]interface{}
	// try to find json objects in chunk
	// split by \n and parse each
	lines := bytes.Split(chunk, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimPrefix(line, []byte("data:"))
			line = bytes.TrimSpace(line)
		}
		if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) {
			continue
		}
		if !bytes.HasPrefix(line, []byte("{")) {
			continue
		}
		var m map[string]interface{}
		if json.Unmarshal(line, &m) == nil {
			if usage, ok := m["usage"].(map[string]interface{}); ok {
				if v, ok := usage["input_tokens"]; ok {
					prompt = toInt(v)
				}
				if v, ok := usage["output_tokens"]; ok {
					completion = toInt(v)
				}
				if prompt != 0 || completion != 0 {
					return
				}
			}
			// also check top-level for message_delta usage
			if v, ok := m["usage"]; ok {
				if um, ok := v.(map[string]interface{}); ok {
					if iv, ok := um["input_tokens"]; ok {
						prompt = toInt(iv)
					}
					if ov, ok := um["output_tokens"]; ok {
						completion = toInt(ov)
					}
				}
			}
		}
		// also try to unmarshal chunk as whole map
		_ = tmp
	}
	return
}

func anthropicToOpenAIChatResponse(body []byte, model string) []byte {
	var anth map[string]interface{}
	if err := json.Unmarshal(body, &anth); err != nil {
		return nil
	}
	// check if it's anthropic message (has type message and content array)
	if t, ok := anth["type"].(string); !ok || t != "message" {
		return nil
	}
	contentArr, ok := anth["content"].([]interface{})
	if !ok {
		return nil
	}
	var text string
	for _, c := range contentArr {
		if cm, ok := c.(map[string]interface{}); ok {
			if cm["type"] == "text" {
				if txt, ok := cm["text"].(string); ok {
					text += txt
				}
			} else if cm["type"] == "thinking" {
				// skip thinking
			}
		}
	}
	usage, _ := anth["usage"].(map[string]interface{})
	promptTokens := 0
	completionTokens := 0
	if usage != nil {
		if v, ok := usage["input_tokens"]; ok {
			promptTokens = toInt(v)
		}
		if v, ok := usage["output_tokens"]; ok {
			completionTokens = toInt(v)
		}
	}
	id, _ := anth["id"].(string)
	if id == "" {
		id = "chatcmpl-" + uuid.NewString()[:8]
	}
	finish := mapStopReason(anth["stop_reason"])
	openAIResp := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": text,
				},
				"finish_reason": finish,
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}
	out, _ := json.Marshal(openAIResp)
	return out
}

// mapStopReason translates an Anthropic stop_reason value into the OpenAI
// finish_reason vocabulary. max_tokens → "length"; end_turn/stop_sequence and
// anything unrecognized → "stop".
func mapStopReason(v interface{}) string {
	switch s, _ := v.(string); s {
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}

// anthropicToOpenAICompletion converts a native Anthropic message body into
// the LEGACY OpenAI text-completion shape consumed by /v1/completions clients
// (choices[].text, object "text_completion") — the chat-completions converter
// above emits choices[].message which those clients cannot read.
func anthropicToOpenAICompletion(body []byte, model string) []byte {
	var anth map[string]interface{}
	if err := json.Unmarshal(body, &anth); err != nil {
		return nil
	}
	if t, ok := anth["type"].(string); !ok || t != "message" {
		return nil
	}
	contentArr, ok := anth["content"].([]interface{})
	if !ok {
		return nil
	}
	var text string
	for _, c := range contentArr {
		if cm, ok := c.(map[string]interface{}); ok && cm["type"] == "text" {
			if txt, ok := cm["text"].(string); ok {
				text += txt
			}
		}
	}
	promptTokens, completionTokens := 0, 0
	if usage, _ := anth["usage"].(map[string]interface{}); usage != nil {
		if v, ok := usage["input_tokens"]; ok {
			promptTokens = toInt(v)
		}
		if v, ok := usage["output_tokens"]; ok {
			completionTokens = toInt(v)
		}
	}
	id, _ := anth["id"].(string)
	if id == "" {
		id = "cmpl-" + uuid.NewString()[:8]
	}
	resp := map[string]interface{}{
		"id":      id,
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"text":          text,
				"finish_reason": mapStopReason(anth["stop_reason"]),
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}
	out, _ := json.Marshal(resp)
	return out
}

func (h *Handler) costForModel(modelID string, prompt, completion int) float64 {
	if h.CatalogStore == nil || (prompt == 0 && completion == 0) {
		return 0
	}
	m, err := h.CatalogStore.Get(modelID)
	if err != nil {
		m, err = h.CatalogStore.GetByShortID(modelID)
		if err != nil {
			return 0
		}
	}
	return catalog.CostFor(m, prompt, completion)
}

func (h *Handler) getReasoningConfig(providerID, modelID string) (reasoning bool, rType string, levels []string, limits map[string]int) {
	limits = map[string]int{}
	if h.DB == nil {
		return false, "", nil, limits
	}
	var r bool
	var rt, rl, rol sql.NullString
	err := h.DB.QueryRow(`SELECT reasoning, reasoning_type, reasoning_levels, reasoning_output_limits FROM provider_models WHERE provider_id=? AND model_id=?`, providerID, modelID).Scan(&r, &rt, &rl, &rol)
	if err != nil {
		// fallback to catalog
		if h.CatalogStore != nil {
			if cm, err := h.CatalogStore.Get(modelID); err == nil {
				r = cm.Reasoning
				rt.String, rt.Valid = cm.ReasoningType, cm.ReasoningType != ""
				rl.String, rl.Valid = cm.ReasoningLevels, cm.ReasoningLevels != ""
				rol.String, rol.Valid = cm.ReasoningOutputLimits, cm.ReasoningOutputLimits != ""
			} else if cm, err := h.CatalogStore.GetByShortID(modelID); err == nil {
				r = cm.Reasoning
				rt.String, rt.Valid = cm.ReasoningType, cm.ReasoningType != ""
				rl.String, rl.Valid = cm.ReasoningLevels, cm.ReasoningLevels != ""
				rol.String, rol.Valid = cm.ReasoningOutputLimits, cm.ReasoningOutputLimits != ""
			}
		}
		if !rt.Valid && !rl.Valid {
			return r, "", nil, limits
		}
	}
	reasoning = r
	if rt.Valid {
		rType = rt.String
	}
	if rl.Valid && rl.String != "" {
		json.Unmarshal([]byte(rl.String), &levels)
	}
	if rol.Valid && rol.String != "" {
		json.Unmarshal([]byte(rol.String), &limits)
	}
	return
}

func (h *Handler) validateReasoning(providerID, modelID string, body []byte) error {
	effort := translate.ExtractReasoningEffort(body)
	if effort == "" {
		return nil
	}
	effort = strings.ToLower(strings.TrimSpace(effort))
	reasoning, rType, levels, limits := h.getReasoningConfig(providerID, modelID)
	if !reasoning {
		return fmt.Errorf("model %s does not support reasoning (effort %s requested)", modelID, effort)
	}
	if rType == "none" {
		return fmt.Errorf("model %s reasoning disabled", modelID)
	}
	if len(levels) > 0 {
		allowed := map[string]bool{}
		for _, lv := range levels {
			allowed[strings.ToLower(lv)] = true
		}
		if !allowed[effort] {
			return fmt.Errorf("reasoning effort '%s' not supported for model %s; allowed: %v", effort, modelID, levels)
		}
		// per-level output limit
		if limit, ok := limits[effort]; ok && limit > 0 {
			// extract max_tokens / max_output_tokens
			var tmp struct {
				MaxTokens       *int `json:"max_tokens"`
				MaxOutputTokens *int `json:"max_output_tokens"`
				MaxTokensAlt    *int `json:"max_tokens_alt"`
			}
			json.Unmarshal(body, &tmp)
			maxOut := 0
			if tmp.MaxTokens != nil {
				maxOut = *tmp.MaxTokens
			} else if tmp.MaxOutputTokens != nil {
				maxOut = *tmp.MaxOutputTokens
			}
			if maxOut > 0 && maxOut > limit {
				return fmt.Errorf("max_tokens %d exceeds output limit %d for reasoning effort '%s' on model %s", maxOut, limit, effort, modelID)
			}
		}
	}
	return nil
}

func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rawModel := translate.ExtractModel(body)
	model := h.resolveAlias(rawModel)
	if !h.enforceModelAllowlist(w, r, rawModel, model) {
		return
	}
	if !h.checkTPM(w, r, body) {
		return
	}
	isStream := translate.IsStreaming(body)
	if model != rawModel {
		body = replaceModelInBody(body, model)
	}
	providerHint := r.Header.Get("X-Provider")
	candidates := h.candidateProviders(rawModel, model, providerHint, func(p *models.Provider) bool {
		return p.Type != models.ProviderAnthropic
	})
	if len(candidates) == 0 {
		if p, err := h.ProviderStore.Resolve(model, providerHint); err == nil && p != nil && p.Type == models.ProviderAnthropic {
			httperr.Invalid(w, "model '"+model+"' is an anthropic model; use POST /v1/messages instead of /v1/chat/completions")
			return
		}
		httperr.Proxy(w, http.StatusServiceUnavailable, "no provider configured")
		return
	}
	if err := h.validateReasoning(candidates[0].ID, model, body); err != nil {
		httperr.Invalid(w, err.Error())
		return
	}
	start := time.Now()
	keyPrefix := r.Header.Get("X-Gateway-Key-Prefix")
	h.proxyCandidates(w, r, body, isStream, model, "chat.completions", keyPrefix, start, candidates, func(p *models.Provider, body []byte) (string, string, []byte, bool, error) {
		apiKey, err := h.ProviderStore.DecryptKey(p)
		if err != nil {
			return "", "", nil, false, err
		}
		upstream := stripProviderPrefix(model, p)
		out := body
		if upstream != translate.ExtractModel(body) {
			out = replaceModelInBody(body, upstream)
		}
		// Billing accuracy: ask the upstream for a final usage frame when the
		// client didn't (OpenAI-compat endpoints only; guarded per-dialect).
		if isStream && h.StreamUsageInject {
			if b2, changed := injectStreamUsage(out); changed {
				out = b2
			}
		}
		target := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
		return target, apiKey, out, false, nil
	})
}

// Completions handles POST /v1/completions
func (h *Handler) Completions(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rawModel := translate.ExtractModel(body)
	model := h.resolveAlias(rawModel)
	if !h.enforceModelAllowlist(w, r, rawModel, model) {
		return
	}
	if !h.checkTPM(w, r, body) {
		return
	}
	if model != rawModel {
		body = replaceModelInBody(body, model)
	}
	isStream := translate.IsStreaming(body)
	providerHint := r.Header.Get("X-Provider")
	candidates := h.candidateProviders(rawModel, model, providerHint, nil)
	if len(candidates) == 0 {
		http.Error(w, `{"error":{"message":"no provider configured"}}`, http.StatusServiceUnavailable)
		return
	}
	p := candidates[0]
	if err := h.validateReasoning(p.ID, model, body); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"invalid_request_error"}}`, err.Error()), http.StatusBadRequest)
		return
	}
	if p.Type == models.ProviderAnthropic {
		http.Error(w, `{"error":{"message":"model '`+model+`' is an anthropic model; use POST /v1/messages instead of /v1/completions","type":"invalid_request_error"}}`, http.StatusBadRequest)
		return
	}
	apiKey, _ := h.ProviderStore.DecryptKey(p)
	target := strings.TrimRight(p.BaseURL, "/") + "/completions"
	isAnthropic := p.Type == models.ProviderAnthropic || strings.Contains(strings.ToLower(p.BaseURL), "anthropic") || strings.Contains(strings.ToLower(p.Name), "claude") || strings.Contains(strings.ToLower(model), "claude") || strings.Contains(strings.ToLower(model), "muse-spark")
	if isAnthropic {
		var comp map[string]any
		if json.Unmarshal(body, &comp) == nil {
			var chatBody map[string]any
			if prompt, hasPrompt := comp["prompt"]; hasPrompt && prompt != nil {
				var promptStr string
				switch v := prompt.(type) {
				case string:
					promptStr = v
				case []interface{}:
					if len(v) > 0 {
						if s, ok := v[0].(string); ok {
							promptStr = s
						}
					}
				default:
					b, _ := json.Marshal(prompt)
					if len(b) > 2 && b[0] == '"' {
						var s string
						json.Unmarshal(b, &s)
						promptStr = s
					} else {
						promptStr = string(b)
					}
				}
				if promptStr == "" || promptStr == "null" {
					promptStr = "hi"
				}
				chatBody = map[string]any{
					"model":    comp["model"],
					"messages": []map[string]any{{"role": "user", "content": promptStr}},
				}
				if v, ok := comp["max_tokens"]; ok {
					chatBody["max_tokens"] = v
				} else if v, ok := comp["max_output_tokens"]; ok {
					chatBody["max_tokens"] = v
				}
				if v, ok := comp["stream"]; ok {
					chatBody["stream"] = v
				}
			} else if _, hasMsgs := comp["messages"]; hasMsgs {
				if b, err := json.Marshal(comp); err == nil {
					var check map[string]any
					json.Unmarshal(b, &check)
					if check["messages"] == nil {
						chatBody = map[string]any{
							"model":    comp["model"],
							"messages": []map[string]any{{"role": "user", "content": "hi"}},
						}
						if v, ok := comp["max_tokens"]; ok {
							chatBody["max_tokens"] = v
						}
					} else {
						chatBody = comp
						if msgsArr, ok := chatBody["messages"].([]interface{}); ok {
							for i, m := range msgsArr {
								if mm, ok := m.(map[string]interface{}); ok {
									if mm["content"] == nil {
										mm["content"] = "hi"
										msgsArr[i] = mm
									}
								}
							}
							chatBody["messages"] = msgsArr
						}
					}
					if b2, err := json.Marshal(chatBody); err == nil {
						body = b2
					}
					chatBody = nil
				}
			}
			if chatBody != nil {
				if b, err := json.Marshal(chatBody); err == nil {
					body = b
				}
			}
		}
		translated, _, err := translate.OpenAIToAnthropic(body)
		if err == nil {
			body = translated
			target = strings.TrimRight(p.BaseURL, "/") + "/v1/messages"
			if strings.Contains(p.BaseURL, "ckff.dev") {
				target = "https://ckff.dev/v1/messages"
			}
		}
	} else if strings.HasSuffix(p.BaseURL, "/v1") {
	}
	start := time.Now()
	h.proxyWithMetrics(w, r, target, apiKey, body, isStream, model, p.ID, r.Header.Get("X-Gateway-Key-Prefix"), "completions", start, isAnthropic)
}

func (h *Handler) Embeddings(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rawModel := translate.ExtractModel(body)
	model := h.resolveAlias(rawModel)
	if !h.enforceModelAllowlist(w, r, rawModel, model) {
		return
	}
	if !h.checkTPM(w, r, body) {
		return
	}
	if model != rawModel {
		body = replaceModelInBody(body, model)
	}
	providerHint := r.Header.Get("X-Provider")
	candidates := h.candidateProviders(rawModel, model, providerHint, func(p *models.Provider) bool {
		return p.Type != models.ProviderAnthropic
	})
	if len(candidates) == 0 {
		httperr.Proxy(w, http.StatusServiceUnavailable, "no provider configured")
		return
	}
	start := time.Now()
	h.proxyCandidates(w, r, body, false, model, "embeddings", r.Header.Get("X-Gateway-Key-Prefix"), start, candidates, func(p *models.Provider, body []byte) (string, string, []byte, bool, error) {
		apiKey, err := h.ProviderStore.DecryptKey(p)
		if err != nil {
			return "", "", nil, false, err
		}
		upstream := stripProviderPrefix(model, p)
		out := body
		if upstream != translate.ExtractModel(body) {
			out = replaceModelInBody(body, upstream)
		}
		target := strings.TrimRight(p.BaseURL, "/") + "/embeddings"
		return target, apiKey, out, false, nil
	})
}

// Models handles GET /v1/models - now returns provider_models (discovered per provider) enriched, not full catalog
func (h *Handler) Models(w http.ResponseWriter, r *http.Request) {
	ck := modelsCacheKey(r)
	if cached, status, headers, ok := h.cacheOrNoop().Get(ck); ok {
		h.serveCacheHit(w, cached, status, headers)
		return
	}
	// allowlist filter for Models — if key is restricted, only return allowed models
	var allowed []string
	if k, ok := middleware.GatewayKeyFromContext(r.Context()); ok && k != nil {
		allowed = k.AllowedModels
	}
	filterAllowed := func(list []map[string]interface{}) []map[string]interface{} {
		if len(allowed) == 0 {
			return list
		}
		var out []map[string]interface{}
		for _, m := range list {
			id, _ := m["id"].(string)
			short, _ := m["model_id"].(string)
			tgt, _ := m["alias_target"].(string)
			if apikey.IsModelAllowed(allowed, id) || apikey.IsModelAllowed(allowed, short) || (tgt != "" && apikey.IsModelAllowed(allowed, tgt)) {
				out = append(out, m)
			}
		}
		return out
	}
	providers, err := h.ProviderStore.List()
	// Try provider_models first - this is the dynamic per-provider list user wants
	if h.DB != nil {
		rows, err := h.DB.Query(`SELECT pm.model_id, pm.display_name, pm.owned_by, pm.context_window, pm.max_output, pm.input_cost, pm.output_cost, pm.reasoning, pm.tool_call, pm.attachment, p.name, pm.reasoning_type, pm.reasoning_levels, pm.reasoning_output_limits FROM provider_models pm JOIN providers p ON p.id = pm.provider_id ORDER BY p.name, pm.model_id LIMIT 500`)
		if err == nil {
			defer rows.Close()
			var pmModels []map[string]interface{}
			for rows.Next() {
				var modelID, displayName, ownedBy, providerName string
				var ctx, maxOut sql.NullInt64
				var inCost, outCost sql.NullFloat64
				var reasoning, toolCall, attachment sql.NullBool
				var rType, rLevels, rLimits sql.NullString
				if err := rows.Scan(&modelID, &displayName, &ownedBy, &ctx, &maxOut, &inCost, &outCost, &reasoning, &toolCall, &attachment, &providerName, &rType, &rLevels, &rLimits); err == nil {
					qid := qualifiedModelID(providerName, modelID)
					m := map[string]interface{}{
						"id":       qid,
						"model_id": modelID,
						"object":   "model",
						"owned_by": providerName,
					}
					if displayName != "" {
						m["display_name"] = displayName
					}
					if ctx.Valid {
						m["context_window"] = ctx.Int64
					}
					if maxOut.Valid {
						m["max_output"] = maxOut.Int64
					}
					if inCost.Valid {
						m["input_cost"] = inCost.Float64
					}
					if outCost.Valid {
						m["output_cost"] = outCost.Float64
					}
					if reasoning.Valid {
						m["reasoning"] = reasoning.Bool
					}
					if toolCall.Valid {
						m["tool_call"] = toolCall.Bool
					}
					if attachment.Valid {
						m["attachment"] = attachment.Bool
					}
					if rType.Valid {
						m["reasoning_type"] = rType.String
					}
					if rLevels.Valid {
						m["reasoning_levels"] = rLevels.String
					}
					if rLimits.Valid {
						m["reasoning_output_limits"] = rLimits.String
					}
					pmModels = append(pmModels, m)
				}
			}
			if len(pmModels) > 0 {
				// add aliases
				seen := map[string]bool{}
				for _, m := range pmModels {
					seen[m["id"].(string)] = true
				}
				if h.DB != nil {
					rows2, _ := h.DB.Query(`SELECT alias, target FROM model_aliases`)
					if rows2 != nil {
						defer rows2.Close()
						for rows2.Next() {
							var alias, target string
							rows2.Scan(&alias, &target)
							if !seen[alias] {
								pmModels = append(pmModels, map[string]interface{}{"id": alias, "object": "model", "owned_by": "gateway-alias", "alias_target": target})
							}
						}
					}
				}
				h.writeJSONCached(w, ck, 30, map[string]interface{}{"object": "list", "data": filterAllowed(pmModels)})
				return
			}
		}
	}
	// Fallback: if no provider_models yet, try old behavior (provider live + catalog) but don't expose full catalog if no providers
	var catalogModels []map[string]interface{}
	if h.CatalogStore != nil && len(providers) > 0 {
		list, _ := h.CatalogStore.List("", "", false, 200, 0)
		for _, cm := range list {
			catalogModels = append(catalogModels, map[string]interface{}{
				"id":             qualifiedModelID(cm.Provider, cm.ID),
				"model_id":       shortModelID(cm.ID),
				"object":         "model",
				"owned_by":       cm.Provider,
				"display_name":   cm.Name,
				"context_window": cm.ContextWindow,
				"max_output":     cm.MaxOutput,
				"input_cost":     cm.InputCost,
				"output_cost":    cm.OutputCost,
				"reasoning":      cm.Reasoning,
				"tool_call":      cm.ToolCall,
				"attachment":     cm.Attachment,
			})
		}
	}
	if err != nil || len(providers) == 0 {
		if len(catalogModels) > 0 {
			h.writeJSONCached(w, ck, 30, map[string]interface{}{"object": "list", "data": filterAllowed(catalogModels)})
			return
		}
		h.writeJSONCached(w, ck, 30, map[string]interface{}{"object": "list", "data": []interface{}{}})
		return
	}
	type modelResp struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	seen := map[string]bool{}
	var all []interface{}
	for _, cm := range catalogModels {
		id := cm["id"].(string)
		seen[id] = true
		all = append(all, cm)
	}
	for _, prov := range providers {
		apiKey, _ := h.ProviderStore.DecryptKey(&prov)
		target := strings.TrimRight(prov.BaseURL, "/") + "/models"
		if prov.Type == models.ProviderAnthropic {
			continue
		}
		req, _ := http.NewRequestWithContext(r.Context(), "GET", target, nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := h.Client.Do(req)
		if err != nil || resp.StatusCode != 200 {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var mr modelResp
		if json.Unmarshal(body, &mr) == nil {
			for _, m := range mr.Data {
				qid := qualifiedModelID(prov.Name, m.ID)
				if seen[qid] || seen[m.ID] {
					continue
				}
				seen[qid] = true
				seen[m.ID] = true
				owned := m.OwnedBy
				if owned == "" {
					owned = prov.Name
				}
				entry := map[string]interface{}{
					"id": qid, "model_id": shortModelID(m.ID), "object": m.Object, "owned_by": owned,
				}
				if h.CatalogStore != nil {
					if cm, err := h.CatalogStore.Get(m.ID); err == nil {
						entry["context_window"] = cm.ContextWindow
						entry["max_output"] = cm.MaxOutput
						entry["input_cost"] = cm.InputCost
						entry["output_cost"] = cm.OutputCost
						entry["reasoning"] = cm.Reasoning
					} else if cm, err := h.CatalogStore.GetByShortID(m.ID); err == nil {
						entry["context_window"] = cm.ContextWindow
						entry["max_output"] = cm.MaxOutput
						entry["input_cost"] = cm.InputCost
						entry["output_cost"] = cm.OutputCost
						entry["reasoning"] = cm.Reasoning
					}
				}
				all = append(all, entry)
			}
		}
	}
	if h.DB != nil {
		rows, _ := h.DB.Query(`SELECT alias, target FROM model_aliases`)
		if rows != nil {
			defer rows.Close()
			for rows.Next() {
				var alias, target string
				rows.Scan(&alias, &target)
				if !seen[alias] {
					seen[alias] = true
					all = append(all, map[string]interface{}{"id": alias, "object": "model", "owned_by": "gateway-alias", "alias_target": target})
				}
			}
		}
	}
	if len(all) == 0 {
		all = append(all, map[string]string{"id": "gpt-4o", "object": "model", "owned_by": "gateway"})
	}
	// apply allowlist to final aggregated list as well
	var filteredAll []interface{}
	if len(allowed) == 0 {
		filteredAll = all
	} else {
		for _, it := range all {
			var id string
			switch v := it.(type) {
			case map[string]interface{}:
				if s, ok := v["id"].(string); ok {
					id = s
				}
				// also allow if alias_target matches allowlist
				if id != "" && !apikey.IsModelAllowed(allowed, id) {
					if tgt, ok := v["alias_target"].(string); ok && apikey.IsModelAllowed(allowed, tgt) {
						id = tgt
					} else {
						continue
					}
				}
			case map[string]string:
				id = v["id"]
				if !apikey.IsModelAllowed(allowed, id) {
					continue
				}
			default:
				continue
			}
			if id != "" && !apikey.IsModelAllowed(allowed, id) {
				continue
			}
			filteredAll = append(filteredAll, it)
		}
		if len(filteredAll) == 0 && len(allowed) > 0 {
			// if nothing matches, return empty rather than leaking
			filteredAll = []interface{}{}
		}
	}
	h.writeJSONCached(w, ck, 30, map[string]interface{}{"object": "list", "data": filteredAll})
}

func (h *Handler) AnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rawModel := translate.ExtractModel(body)
	model := h.resolveAlias(rawModel)
	if !h.enforceModelAllowlist(w, r, rawModel, model) {
		return
	}
	if !h.checkTPM(w, r, body) {
		return
	}
	if model != rawModel {
		body = replaceModelInBody(body, model)
	}
	isStream := translate.IsStreaming(body)
	providerHint := r.Header.Get("X-Provider")
	candidates := h.candidateProviders(rawModel, model, providerHint, func(p *models.Provider) bool {
		return p.Type == models.ProviderAnthropic
	})
	if len(candidates) == 0 {
		if p, err := h.ProviderStore.Resolve(model, providerHint); err == nil && p != nil && p.Type != models.ProviderAnthropic {
			httperr.Invalid(w, "model '"+model+"' is not an anthropic model; use POST /v1/chat/completions, /v1/completions or /v1/responses")
			return
		}
		httperr.Write(w, http.StatusServiceUnavailable, "no provider configured", httperr.TypeNotFound)
		return
	}
	if err := h.validateReasoning(candidates[0].ID, model, body); err != nil {
		httperr.Invalid(w, err.Error())
		return
	}
	start := time.Now()
	h.proxyCandidates(w, r, body, isStream, model, "messages", r.Header.Get("X-Gateway-Key-Prefix"), start, candidates, func(p *models.Provider, body []byte) (string, string, []byte, bool, error) {
		apiKey, err := h.ProviderStore.DecryptKey(p)
		if err != nil {
			return "", "", nil, false, err
		}
		upstream := stripProviderPrefix(model, p)
		out := body
		if upstream != translate.ExtractModel(body) {
			out = replaceModelInBody(body, upstream)
		}
		target := strings.TrimRight(p.BaseURL, "/") + "/v1/messages"
		if strings.HasSuffix(p.BaseURL, "/v1/messages") {
			target = p.BaseURL
		} else if strings.Contains(p.BaseURL, "anthropic.com") && !strings.Contains(target, "/v1/messages") {
			target = strings.TrimRight(p.BaseURL, "/") + "/v1/messages"
		}
		return target, apiKey, out, true, nil
	})
}

// Responses handles POST /v1/responses (OpenAI Responses API)
func (h *Handler) Responses(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rawModel := translate.ExtractModel(body)
	model := h.resolveAlias(rawModel)
	if !h.enforceModelAllowlist(w, r, rawModel, model) {
		return
	}
	if !h.checkTPM(w, r, body) {
		return
	}
	if model != rawModel {
		body = replaceModelInBody(body, model)
	}
	isStream := translate.IsStreaming(body)
	providerHint := r.Header.Get("X-Provider")
	candidates := h.candidateProviders(rawModel, model, providerHint, nil)
	if len(candidates) == 0 {
		http.Error(w, `{"error":{"message":"no provider configured"}}`, http.StatusServiceUnavailable)
		return
	}
	p := candidates[0]
	if err := h.validateReasoning(p.ID, model, body); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"invalid_request_error"}}`, err.Error()), http.StatusBadRequest)
		return
	}
	apiKey, _ := h.ProviderStore.DecryptKey(p)
	start := time.Now()
	keyPrefix := r.Header.Get("X-Gateway-Key-Prefix")
	// For anthropic providers, /v1/responses does not exist — skip native and translate directly
	if p.Type != models.ProviderAnthropic {
		target := strings.TrimRight(p.BaseURL, "/") + "/responses"
		if strings.HasSuffix(p.BaseURL, "/v1") {
			target = p.BaseURL + "/responses"
		}
		req, _ := http.NewRequestWithContext(r.Context(), "POST", target, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		if isStream {
			// Ask for a real stream so SSE-native providers open one instead
			// of silently downgrading to a buffered JSON answer.
			req.Header.Set("Accept", "text/event-stream")
		}
		resp, err := h.Client.Do(req)
		// Only an explicit 200 is a usable native reply. Location-less 3xx
		// bodies arrive without transport error and previously passed through as
		// pseudo-success, bypassing translation. Redirects WITH Location are
		// followed by the http.Client before this point.
		if err == nil && resp.StatusCode == http.StatusOK {
			ct := resp.Header.Get("Content-Type")
			if isStream && strings.HasPrefix(ct, "text/event-stream") {
				// TRUE native pass-through streaming: relay chunk-by-chunk,
				// flush per read. A false return means nothing was committed
				// (dead-on-arrival upstream) — fall through to translated.
				if out := h.streamNativeResponses(w, r, resp, model, keyPrefix, p.ID, start); out.committed {
					return
				}
				log.Info().Str("model", model).Str("provider", p.ID).Msg("native responses attempt unusable before commit; falling through to translated path")
			} else {
				bodyBytes, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				nativeSSE := resp.StatusCode == 200 &&
					(strings.HasPrefix(ct, "text/event-stream") || bytes.Contains(bodyBytes, []byte("\ndata: ")) || bytes.Contains(bodyBytes, []byte("data: ")))
				jsonOK := json.Valid(bodyBytes)
				noHTML := !bytes.Contains(bytes.ToLower(bodyBytes), []byte("<!doctype")) && !bytes.Contains(bytes.ToLower(bodyBytes), []byte("<html"))
				if (jsonOK && noHTML) || nativeSSE {
					copyHeader(w.Header(), resp.Header)
					w.WriteHeader(resp.StatusCode)
					w.Write(bodyBytes)
					pt, ct2 := extractUsage(bodyBytes)
					cost := h.costForModel(model, pt, ct2)
					h.logRequestExtended(keyPrefix, p.ID, model, "responses", resp.StatusCode, time.Since(start).Milliseconds(), pt, ct2, cost, false)
					return
				}
			}
		} else if resp != nil {
			resp.Body.Close()
		}
	}
	translated, _, err := translate.ResponsesToChat(body)
	if err != nil {
		http.Error(w, `{"error":{"message":"failed to translate responses to chat"}}`, http.StatusBadRequest)
		return
	}
	if isStream {
		// Translated-path streaming: force stream:true on the outbound chat
		// (or anthropic messages) call and re-emit inbound deltas as the
		// OpenAI Responses SSE protocol. LB guarantees a single candidate.
		targetChat := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
		upstreamBody := translated
		isAnthropicUpstream := false
		if p.Type == models.ProviderAnthropic {
			translated2, _, convErr := translate.OpenAIToAnthropic(translated)
			if convErr != nil || len(translated2) == 0 {
				http.Error(w, `{"error":{"message":"failed to translate responses to anthropic"}}`, http.StatusBadRequest)
				return
			}
			upstreamBody = translated2
			targetChat = strings.TrimRight(p.BaseURL, "/") + "/v1/messages"
			isAnthropicUpstream = true
		}
		h.streamTranslatedResponses(w, r, targetChat, apiKey, upstreamBody, model, keyPrefix, p.ID, start, isAnthropicUpstream)
		return
	}
	h.proxyWithMetricsOpts(w, r, strings.TrimRight(p.BaseURL, "/")+"/chat/completions", apiKey, translated, false, model, p.ID, keyPrefix, "responses", start, false, proxyOpts{translatedResponses: true})
}

func replaceModelInBody(body []byte, newModel string) []byte {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["model"] = newModel
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
