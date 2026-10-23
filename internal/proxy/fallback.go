package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ai-gateway/internal/decision"
	"ai-gateway/internal/httperr"
	"ai-gateway/internal/imagegen"
	"ai-gateway/internal/lb"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/resilience"
	"ai-gateway/internal/videogen"

	"github.com/rs/zerolog/log"
)

func qualifiedModelID(providerName, modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return modelID
	}
	if strings.Contains(modelID, "/") {
		return modelID
	}
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		return modelID
	}
	return providerName + "/" + modelID
}

func shortModelID(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
		return model[i+1:]
	}
	return model
}

func stripProviderPrefix(model string, p *models.Provider) string {
	if p == nil {
		return model
	}
	for _, pre := range []string{p.Name + "/", string(p.Type) + "/"} {
		if pre == "/" {
			continue
		}
		if len(model) > len(pre) && strings.EqualFold(model[:len(pre)], pre) {
			return model[len(pre):]
		}
	}
	return model
}

type bufWriter struct {
	hdr  http.Header
	code int
	buf  bytes.Buffer
}

func newBufWriter() *bufWriter {
	return &bufWriter{hdr: make(http.Header)}
}

func (b *bufWriter) Header() http.Header {
	if b.hdr == nil {
		b.hdr = make(http.Header)
	}
	return b.hdr
}

func (b *bufWriter) WriteHeader(code int) {
	if b.code == 0 {
		b.code = code
	}
}

func (b *bufWriter) Write(p []byte) (int, error) {
	if b.code == 0 {
		b.code = http.StatusOK
	}
	return b.buf.Write(p)
}

func (b *bufWriter) flushTo(w http.ResponseWriter) {
	copyHeader(w.Header(), b.hdr)
	code := b.code
	if code == 0 {
		code = http.StatusOK
	}
	w.WriteHeader(code)
	_, _ = w.Write(b.buf.Bytes())
}

// candidateProviders selects the provider(s) eligible to serve this request.
//
// Routing contract:
//   - Pin always wins: an X-Provider hint, or a qualified model whose first
//     segment names an existing provider/type ("openai/gpt-4o"), routes there
//     exclusively — LB rules are bypassed.
//   - Otherwise a curated lb_rules group (if configured for this model or its
//     alias name) governs: the rule's strategy orders the healthy members
//     (round-robin / random / weighted pick a single serving member;
//     failover returns the whole position-ordered list so proxyCandidates
//     walks it on retriable failures).
//   - Otherwise: LegacyFallback=false (default) rejects the request — the
//     caller surfaces model_not_routed; LegacyFallback=true restores the
//     legacy resolution (health-aware ownership round-robin, name/type
//     heuristics, default provider).
//
// Same-provider retries inside proxyWithMetrics remain active per the retry
// policy regardless of strategy.
//
// requestKeyOrg extracts the authenticated gateway key's org scope ("" for
// global keys / dashboard sessions without an org).
func (h *Handler) requestKeyOrg(r *http.Request) string {
	if k, ok := middleware.GatewayKeyFromContext(r.Context()); ok && k != nil && k.OrgID != nil {
		return *k.OrgID
	}
	return ""
}

// orgAllows reports whether a key org may use a provider. Policy: global
// (unscoped) providers serve everyone; org-scoped providers serve only that
// org. Previously the proxy path ignored the key's org entirely, so any key
// could route through (and spend) another tenant's provider.
func orgAllows(keyOrg string, p *models.Provider) bool {
	if keyOrg == "" || p == nil || p.OrgID == nil || *p.OrgID == "" {
		return true
	}
	return *p.OrgID == keyOrg
}

func (h *Handler) candidateProviders(rawModel, model, hint, keyOrg string, pred func(*models.Provider) bool) []*models.Provider {
	cands, _ := h.candidateProvidersWithRule(rawModel, model, hint, keyOrg, pred)
	out := make([]*models.Provider, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Provider)
	}
	return out
}

func (h *Handler) candidateProvidersWithRuleContext(ctx context.Context, rawModel, model, hint, keyOrg string, pred func(*models.Provider) bool) ([]lb.Candidate, *lb.Rule) {
	return h.candidateProvidersWithRuleContextAndRule(ctx, rawModel, model, hint, keyOrg, pred)
}

// candidateProvidersWithRule is candidateProviders plus the curated rule that
// governed selection (nil for pins, legacy resolution, or no-route). Each
// returned candidate carries its own rule member — and with it that member's
// model override — so rules with several options on one provider route
// correctly. Callers pass the rule to proxyCandidates for strategy context
// (e.g. logging); overrides come from the candidates, never by provider id.
func (h *Handler) candidateProvidersWithRule(rawModel, model, hint, keyOrg string, pred func(*models.Provider) bool) ([]lb.Candidate, *lb.Rule) {
	return h.candidateProvidersWithRuleContext(context.Background(), rawModel, model, hint, keyOrg, pred)
}

func (h *Handler) candidateProvidersWithRuleContextAndRule(ctx context.Context, rawModel, model, hint, keyOrg string, pred func(*models.Provider) bool) ([]lb.Candidate, *lb.Rule) {
	out := make([]lb.Candidate, 0, 1)
	consider := func(p *models.Provider) bool {
		if p == nil || p.ID == "" || (pred != nil && !pred(p)) {
			return false
		}
		out = append(out, lb.Candidate{Provider: p})
		return true
	}

	// 1. Explicit provider hint = hard pin (case-insensitive: display names
	// keep their casing, e.g. "AIHubMix", while callers send any case).
	if hint != "" {
		if p, err := h.ProviderStore.GetByNameCI(hint); err == nil && orgAllows(keyOrg, p) {
			consider(p)
			return out, nil
		}
		if p, err := h.ProviderStore.GetByID(hint); err == nil && orgAllows(keyOrg, p) {
			consider(p)
			return out, nil
		}
	}

	// 2. Qualified-ID pin: "provider/model" where prefix matches a provider
	// name or provider TYPE. Purely namespace-ish models like
	// "openrouter/gpt-4o" don't pin unless such a provider exists.
	if idx := strings.Index(model, "/"); idx > 0 {
		prefix := strings.ToLower(strings.TrimSpace(model[:idx]))
		if prefix != "" {
			if p, err := h.ProviderStore.GetByNameCI(prefix); err == nil && orgAllows(keyOrg, p) && consider(p) {
				return out, nil
			}
			if p, err := h.ProviderStore.GetByType(prefix); err == nil && orgAllows(keyOrg, p) && consider(p) {
				return out, nil
			}
		}
	}

	// 3. Curated LB rule (checked on post-alias model name first, then the
	// raw/alias spelling). The rule's strategy orders healthy members;
	// failover rules hand the whole ordering to proxyCandidates as ordered
	// fallback candidates.
	if h.LB != nil {
		for _, key := range []string{model, rawModel} {
			key = strings.ToLower(strings.TrimSpace(key))
			if key == "" {
				continue
			}
			// Org-aware resolution: a tenant's own rule wins, the global rule is
			// the fallback. Without the org argument every key resolved the one
			// global rule, so an org-scoped rule could not exist at all.
			if rule := h.LB.RuleForModelOrg(key, keyOrg); rule != nil {
				quotaEligible := make(map[string]bool, len(rule.Members))
				for _, member := range rule.Members {
					p, err := h.ProviderStore.GetByID(member.ProviderID)
					if err != nil || p == nil {
						continue
					}
					quotaModel := model
					if override := member.ModelOverride; override != "" {
						quotaModel = override
					}
					if h.providerQuotaAllows(ctx, p, quotaModel, rule.QuotaThresholdPct) {
						quotaEligible[quotaRuleMemberKey(member)] = true
					}
				}
				ordered := h.LB.SelectCandidatesFiltered(rule, func(member lb.Member) bool {
					return quotaEligible[quotaRuleMemberKey(member)]
				})
				if len(ordered) == 0 {
					// A configured rule with no quota-eligible members is still a
					// route. Do not silently fall back to legacy provider heuristics.
					return nil, rule
				}
				// Org-scoped keys may only route to providers their org
				// owns (or global ones), AND each member must satisfy the
				// endpoint's capability predicate.
				//
				// SECURITY/CORRECTNESS: the rule branch used to skip `pred`
				// entirely — pred was only applied inside consider(), which the
				// hint/pin/legacy tiers use. So a rule for a System One model
				// that listed an OpenAI provider sent the decision body to
				// /chat/completions, and a rule for an image model that listed
				// an Anthropic member posted a Gemini/OpenAI image body to
				// /v1/messages: an opaque upstream error, a wasted (and on
				// metered providers billable) round trip, and a violation of the
				// capability isolation the endpoint guards exist to enforce.
				eligible := make([]lb.Candidate, 0, len(ordered))
				for _, cand := range ordered {
					if !orgAllows(keyOrg, cand.Provider) {
						continue
					}
					if pred != nil && !pred(cand.Provider) {
						continue
					}
					eligible = append(eligible, cand)
				}
				if len(eligible) == 0 {
					continue
				}
				picked := eligible[:1]
				if rule.Strategy == lb.StrategyFailover {
					// Explicit opt-in: walk members in position order on
					// retriable failures.
					picked = eligible
				}
				out = append(out, picked...)
				return out, rule
			}
		}
	}

	// 4. No rule and no pin: reject by default, or resolve via legacy
	// heuristics when the operator opted back in.
	if !h.LegacyFallback {
		return nil, nil
	}
	// Legacy single pick: health-aware round-robin over discovered owners,
	// heuristic ownership by name/type, else default provider. Org-scoped keys
	// resolve through the org-aware resolver (global providers still shared).
	if h.ProviderStore != nil {
		var p *models.Provider
		var err error
		if keyOrg != "" {
			p, err = h.ProviderStore.ResolveWithOrg(model, "", keyOrg)
		} else {
			p, err = h.ProviderStore.Resolve(model, "")
		}
		if err == nil {
			consider(p)
		}
	}
	return out, nil
}

func valueOrZero(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func quotaRuleMemberKey(m lb.Member) string {
	return strings.ToLower(strings.TrimSpace(m.ProviderID)) + "\x00" + strings.ToLower(strings.TrimSpace(m.ModelOverride))
}

// prepareFn builds one upstream attempt. `candModel` is the model id the
// gateway has RESOLVED for this candidate (the lb_rules model_override when the
// member pins one, otherwise the requested/aliased model).
//
// It is passed explicitly because the body cannot be trusted to carry it: the
// override is applied with replaceModelInBody, a JSON rewriter, so a MULTIPART
// request (POST /v1/images/edits) comes through unchanged. A prepare closure
// that re-derived the model from the body therefore sent the client's alias
// upstream and billed it as the alias while the log recorded the override — the
// exact split that made edits ignore lb_rules.model_override.
type prepareFn func(p *models.Provider, body []byte, candModel string) (target, apiKey string, outBody []byte, isAnth bool, contentType string, err error)

// shouldFailoverFrom reports whether a status justifies trying the next
// candidate provider. The old rule stopped at ANY <500 — including 429 quota
// hiccups and rotated-credential 401/403 — stranding traffic on a dead primary.
func shouldFailoverFrom(status int) bool {
	if status == 0 {
		return true // transport failure
	}
	if status == 429 || status == 401 || status == 403 || status == 408 {
		return true
	}
	return status >= 500
}

// shouldFailoverFromOpts is shouldFailoverFrom with the per-endpoint opt-in
// escape hatch: a noFailover request (billable, non-idempotent creates) never
// advances to another candidate, whatever the failure status. Failing over a
// create to a SECOND provider bills twice for one client request and, worse,
// makes the two bills independent (provider A generated an asset the client
// never saw).
func shouldFailoverFromOpts(status int, extra proxyCandidatesOpts, endpoint string) bool {
	if extra.noFailover {
		// Count the declined failover: on a billable create this is a SECOND
		// provider invoice the gateway just refused to incur.
		resilience.IncBillableRetrySkip(endpoint, status)
		return false
	}
	return shouldFailoverFrom(status)
}

// DefaultMaxProxyResponseBytes is the default cap on a buffered upstream
// RESPONSE body. It exists because the buffered success path used to call
// io.ReadAll on an unbounded body: a 64 MiB b64_json image response cost
// ~245 MB of peak RSS per in-flight request (measured), which OOMs a 1 GiB
// container at four concurrent 4K images. 64 MiB matches the request-side
// MAX_PROXY_BODY_MB default and leaves generous head-room for a 4K PNG
// (~16 MiB raw, ~22 MiB base64) plus a multi-image batch.
const DefaultMaxProxyResponseBytes int64 = 64 << 20

// proxyCandidatesOpts carries optional per-endpoint behavior for
// proxyCandidatesWithOpts (image dialect translation, provider-specific auth,
// a longer first-byte budget, exactly-once semantics for billable creates).
type proxyCandidatesOpts struct {
	// transformResponse rewrites a successful buffered upstream body (image
	// dialect normalization). The serving provider is supplied so the
	// transform can pick the right dialect; a nil out leaves the body
	// unchanged and hasCost overrides the billed cost.
	transformResponse func(p *models.Provider, body []byte, status int) (out []byte, costUSD float64, hasCost bool)
	// contentType overrides the upstream request Content-Type (multipart for
	// image-edit passthrough).
	contentType string
	// ttfb overrides the client-facing first-byte budget (image generation is
	// legitimately slower than a chat first token). Zero = "no override": the
	// handler/endpoint default applies (see resolveTTFBBudget). Use
	// ttfbDisable to express DISABLED explicitly — zero alone cannot mean
	// "disabled" because it is indistinguishable from "unset".
	ttfb time.Duration
	// ttfbDisable removes the client-facing first-byte watchdog entirely for
	// this endpoint: the gateway waits as long as the upstream needs, exactly
	// like the pre-TTFB gateway. It maps the documented
	// IMAGE_TIMEOUT_SECONDS=0 ("disabled") setting, which previously applied
	// the 300s default anyway (P1-12). Streaming keepalive frames are not
	// emitted either (there is no budget to expire), so a client relying on
	// them must not set this.
	//
	// Precedence: ttfbDisable wins over ttfb; ttfb (when > 0) wins over the
	// endpoint default; otherwise the endpoint default applies.
	ttfbDisable bool
	// maxResponseBytes caps the buffered upstream response body for this
	// endpoint. Zero = Handler.MaxResponseBytes, then
	// DefaultMaxProxyResponseBytes. Over-limit responses answer a clean 502
	// that names the limit instead of buffering without bound (P1-9).
	maxResponseBytes int64
	// noRetry disables same-provider retries. Set it — with noFailover — for
	// BILLABLE, NON-IDEMPOTENT creates: /v1/images/generations and
	// /v1/images/edits (internal/proxy/images.go). The upstream charges per
	// generated asset, so a retry after an ambiguous 5xx/timeout generates and
	// bills a second asset for one client request (P0-2). Not even 429 is
	// retried: a metered provider that rate-limits after generation has
	// already charged. See resilience.NonRetryable.
	noRetry bool
	// noFailover stops the candidate chain at the first provider. Used for the
	// same callers as noRetry: pre-fix, an image create that failed over ran on
	// TWO providers and was billed twice. Contrast /v1/videos (videos.go),
	// which never used proxyCandidates for its create path at all — the
	// correct behaviour for a non-idempotent POST.
	noFailover bool
	// noCache bypasses the exact-match response cache for this endpoint, in
	// both directions (no lookup, no store). Generated media must never be
	// replayed: a cached image is stale the moment it is served (a re-roll
	// returns yesterday's picture) and the cached replay logs cost_usd=0, so
	// the spend also disappears from budgets (P2-2). Set by the image
	// endpoints in internal/proxy/images.go.
	noCache bool
	// requestTotal overrides the overall request budget (Timeouts.RequestTotal)
	// for this endpoint. Zero = handler default. Used by the decision and image
	// endpoints, which had none and therefore relied on the unbounded default
	// (P2-14).
	requestTotal time.Duration
}

// resolveTTFBBudget resolves the client-facing first-byte budget for one
// endpoint, as a (budget, disabled) pair.
//
// The tri-state is the P1-12 fix. IMAGE_TIMEOUT_SECONDS has three documented
// values and they must be distinguishable:
//
//	unset / -1  => DefaultImageTTFB (300s) applied by the endpoint default
//	0           => DISABLED: no first-byte watchdog at all
//	N (> 0)     => N seconds
//
// A single time.Duration cannot express that, because 0 means both "unset,
// use the default" and "disabled". Hence the explicit disabled flag; a
// zero-value proxyCandidatesOpts (the chat callers) still means "no override".
//
// Resolution order:
//  1. extra.ttfbDisable                            => disabled
//  2. image endpoint && Handler.ImageTTFBDisabled  => disabled
//  3. extra.ttfb > 0                               => that budget
//  4. image endpoint && Handler.ImageTTFB > 0      => that budget
//  5. image endpoint                               => DefaultImageTTFB
//  6. otherwise                                    => h.Timeouts.TTFB
//
// Step 2 is deliberately ahead of step 3: ImageTTFBDisabled is the operator's
// explicit IMAGE_TIMEOUT_SECONDS=0 switch, so an endpoint that passes a
// caller-side default (images.go passes h.imageTTFB(), which is 300s whenever
// ImageTTFB==0) cannot resurrect a watchdog the operator turned off. That also
// means main.go only has to set ImageTTFBDisabled for the disabled case — the
// image endpoints need no change at all.
func (h *Handler) resolveTTFBBudget(endpoint string, extra proxyCandidatesOpts) (time.Duration, bool) {
	if extra.ttfbDisable {
		return 0, true
	}
	image := isImageEndpoint(endpoint)
	if image && h.ImageTTFBDisabled {
		return 0, true
	}
	if extra.ttfb > 0 {
		return extra.ttfb, false
	}
	if image {
		if h.ImageTTFB > 0 {
			return h.ImageTTFB, false
		}
		return DefaultImageTTFB, false
	}
	return h.Timeouts.TTFB, false
}

// isImageEndpoint reports whether an internal endpoint name belongs to the
// image surfaces (the only endpoints with their own first-byte default).
func isImageEndpoint(endpoint string) bool {
	switch endpoint {
	case "images.generations", "images.edits":
		return true
	}
	return false
}

// responseByteLimit resolves the effective buffered-response cap.
func (h *Handler) responseByteLimit(extra proxyCandidatesOpts) int64 {
	if extra.maxResponseBytes > 0 {
		return extra.maxResponseBytes
	}
	if h.MaxResponseBytes > 0 {
		return h.MaxResponseBytes
	}
	return DefaultMaxProxyResponseBytes
}

// effectiveRetry resolves the retry policy for one proxied call: the handler
// default, wrapped in a never-retry policy when the endpoint declares
// noRetry (billable non-idempotent creates).
func (h *Handler) effectiveRetry(extra proxyCandidatesOpts) resilience.RetryPolicy {
	r := h.retryOrDefault()
	if extra.noRetry {
		return resilience.NoRetry(r)
	}
	return r
}

// mcpRound reports whether this request is one iteration of the gateway-side
// MCP tool loop (see proxy/mcp.go). Such rounds bypass the response cache.
//
// SECURITY: the marker lives in the request CONTEXT, never in a header. It used
// to be `X-Gateway-MCP-Round: 1`, which is fully client-controlled, and acting
// on it re-enabled retries on billable image creates (double billing) and
// silently forced stream:true requests down the buffered path, where the SSE
// usage envelope is never parsed — i.e. an unmetered completion for anyone who
// set one extra header.
func mcpRound(r *http.Request) bool {
	return isMCPRound(r.Context())
}

func (h *Handler) proxyCandidates(w http.ResponseWriter, r *http.Request, body []byte, isStream bool, model, endpoint, keyPrefix string, start time.Time, candidates []lb.Candidate, rule *lb.Rule, prepare prepareFn) {
	h.proxyCandidatesWithOpts(w, r, body, isStream, model, endpoint, keyPrefix, start, candidates, rule, prepare, proxyCandidatesOpts{})
}

func (h *Handler) proxyCandidatesWithOpts(w http.ResponseWriter, r *http.Request, body []byte, isStream bool, model, endpoint, keyPrefix string, start time.Time, candidates []lb.Candidate, rule *lb.Rule, prepare prepareFn, extra proxyCandidatesOpts) {
	if len(candidates) == 0 {
		httperr.Proxy(w, http.StatusServiceUnavailable, "no provider configured")
		return
	}
	primaryID := candidates[0].Provider.ID

	// One client-facing first-byte budget per client request, shared across
	// the whole candidate chain: every upstream attempt is bounded by the
	// REMAINING budget, so a chain of slow candidates still answers before
	// an edge proxy (Cloudflare ~100s) can 524 the client. Streams that run
	// out of budget get SSE keepalive headers + frames; buffered requests
	// get an honest 504.
	ttfbBudget, ttfbDisabled := h.resolveTTFBBudget(endpoint, extra)
	var ttfb *ttfbController
	if ttfbDisabled {
		// Explicit "no watchdog" (IMAGE_TIMEOUT_SECONDS=0): every budget-gated
		// method on this controller short-circuits, so the gateway waits as
		// long as the upstream needs and never fabricates a 504 or keepalive
		// frames. See newDisabledTTFBController.
		ttfb = newDisabledTTFBController(start)
	} else {
		ttfb = newTTFBController(ttfbBudget, start)
	}
	defer ttfb.stop()
	w = &keepaliveSafeWriter{ResponseWriter: w, c: ttfb}
	// In-band terminal error once keepalive headers have flowed (a second
	// HTTP status is impossible at that point). /v1/messages clients speak
	// anthropic SSE; everything else OpenAI SSE.
	terminalError := func(status int, msg string) {
		if ttfb.headersCommitted() {
			writeSSEUpstreamError(w, endpoint == "messages", msg)
			return
		}
		httperr.Proxy(w, status, msg)
	}

	// relayClientCaused relays a client-caused upstream error verdict exactly
	// like the Completions/Responses handlers do (proxy.go): the FULL bounded
	// clientBody with the upstream status and JSON content type — not a
	// 512-char snippet inside the generic proxy_error envelope, which handed
	// /v1/chat/completions, /v1/messages and /v1/embeddings clients truncated,
	// mid-string-cut JSON for the same upstream error class. errSnippet is
	// only a fallback when the body was unavailable. Once keepalive headers
	// have flowed on a stream, a second status line is impossible: the verdict
	// goes in-band as an SSE error frame instead.
	relayClientCaused := func(outcome attemptOutcome) {
		relay := outcome.clientBody
		if relay == "" {
			relay = outcome.errSnippet
		}
		if relay == "" {
			relay = "upstream rejected the request"
		}
		if ttfb.headersCommitted() {
			terminalError(outboundStatus(outcome.status), relay)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(outboundStatus(outcome.status))
		_, _ = w.Write([]byte(relay))
	}

	var (
		lastBW         *bufWriter
		lastProvider   *models.Provider
		lastFailStatus int
		attempted      bool
		// Diagnostics for the terminal "all attempts failed" response: which
		// provider was tried last and why it failed (bounded upstream error
		// body or transport error text).
		lastProviderID   string
		lastProviderName string
		lastDetail       string
		// chain records every provider that was tried and failed over from,
		// persisted onto the final request_logs row's fallback_chain column.
		chain []providerAttempt
	)

	for idx, cand := range candidates {
		p := cand.Provider
		// Per-member model override: each rule member may send a different
		// model id upstream (e.g. a pinned date version, or a sibling model
		// on the same provider). The override travels on the candidate — a
		// rule may list one provider several times with different models.
		// Pins and legacy routes carry no member, hence no override. The
		// override also feeds usage logging via candModel below.
		candBody, candModel := body, model
		if override := cand.ModelOverrideForRequest(); override != "" && override != model {
			candBody = replaceModelInBody(body, override)
			candModel = override
		}
		target, apiKey, outBody, isAnth, candContentType, err := prepare(p, candBody, candModel)
		if err != nil {
			continue
		}
		attempted = true

		fallbackName := ""
		if p.ID != primaryID && idx > 0 && candidates[0].Provider != nil {
			fallbackName = p.Name
		}
		ct := extra.contentType
		if candContentType != "" {
			ct = candContentType
		}
		// MCP tool rounds always run buffered and must bypass the response
		// cache: replaying a cached pre-tool answer inside the loop would
		// feed the model its own previous output as if it were new.
		//
		// NOTE: `extra.noRetry` is deliberately NOT cleared here. A tool round
		// is a normal chat/messages/responses call, which never sets it; the
		// only callers that do are the billable media creates, and clearing it
		// would re-enable the exact double-billing retry that noRetry exists to
		// prevent.
		if mcpRound(r) {
			extra.noCache = true
		}
		callOpts := proxyOpts{
			fallbackFrom:     fallbackName,
			attempts:         chain,
			rule:             rule,
			ttfb:             ttfb,
			contentType:      ct,
			noRetry:          extra.noRetry,
			noCache:          extra.noCache,
			maxResponseBytes: h.responseByteLimit(extra),
			requestTotal:     extra.requestTotal,
			provider:         p,
		}
		if extra.transformResponse != nil {
			provider := p
			callOpts.transformResponse = func(b []byte, s int) ([]byte, float64, bool) {
				return extra.transformResponse(provider, b, s)
			}
		}

		if isStream {
			outcome := h.proxyWithMetricsOpts(w, r, target, apiKey, outBody, true, candModel, p.ID, keyPrefix, endpoint, start, isAnth, callOpts)
			lastProviderID, lastProviderName = p.ID, p.Name
			lastDetail = outcome.errSnippet
			if outcome.errSnippet == "" {
				lastDetail = outcome.errText
			}
			// Client-caused 5xx (upstream "invalid_request" semantics):
			// relay the upstream's verdict — no failover, no generic 502.
			if outcome.clientCaused {
				relayClientCaused(outcome)
				return
			}
			if outcome.committed || !outcome.retriable {
				return // headers already flowed (or hard client-side stop)
			}
			if extra.noFailover {
				// Billable non-idempotent create: the first candidate is the
				// only candidate. Relaying its failure is strictly cheaper
				// than a second generation on another provider.
				log.Warn().Str("candidate", p.Name).Str("model", model).Int("status", outcome.status).Msg("noFailover set; not advancing to another candidate")
				lastFailStatus = outcome.status
				break
			}
			log.Info().Str("candidate", p.Name).Str("model", model).Int("status", outcome.status).Str("detail", truncateDetail(lastDetail)).Msg("stream candidate failed pre-commit, failing over")
			lastFailStatus = outcome.status
			chain = append(chain, providerAttempt{ProviderID: p.ID, Name: p.Name, Status: outcome.status})
			continue
		}

		bw := newBufWriter()
		outcome := h.proxyWithMetricsOpts(bw, r, target, apiKey, outBody, false, candModel, p.ID, keyPrefix, endpoint, start, isAnth, callOpts)
		if !outcome.committed && bw.code == 0 {
			// Terminal pre-commit failure: nothing usable was produced.
			lastProviderID, lastProviderName = p.ID, p.Name
			lastDetail = outcome.errSnippet
			if lastDetail == "" {
				lastDetail = outcome.errText
			}
			// Client-caused 5xx (upstream "invalid_request" semantics):
			// relay the upstream's verdict instead of the retryable-looking
			// generic envelope.
			if outcome.clientCaused {
				relayClientCaused(outcome)
				return
			}
			lastFailStatus = outcome.status
			if shouldFailoverFromOpts(outcome.status, extra, endpoint) {
				chain = append(chain, providerAttempt{ProviderID: p.ID, Name: p.Name, Status: outcome.status})
				continue
			}
			terminalError(outboundStatus(outcome.status), "upstream unavailable")
			return
		}
		if bw.code > 0 && bw.code < 400 {
			if p.ID != primaryID {
				bw.Header().Set("X-Fallback-Used", p.Name)
				log.Info().Str("fallback_used", p.Name).Str("from", candidates[0].Provider.Name).Str("model", model).Msg("fallback_used")
			}
			bw.flushTo(w)
			return
		}
		lastBW, lastProvider = bw, p
		if shouldFailoverFromOpts(bw.code, extra, endpoint) {
			chain = append(chain, providerAttempt{ProviderID: p.ID, Name: p.Name, Status: bw.code})
			continue
		}
		bw.flushTo(w)
		return
	}

	// Every candidate exhausted before committing anything.
	if lastBW != nil {
		if lastProvider != nil && lastProvider.ID != primaryID {
			lastBW.Header().Set("X-Fallback-Used", lastProvider.Name)
		}
		lastBW.flushTo(w)
		return
	}
	// Nothing was even attempted: no eligible candidates, or every candidate
	// failed gateway-side request preparation (e.g. credential decryption).
	// There is no upstream status to relay.
	if !attempted {
		httperr.Proxy(w, http.StatusBadGateway, "no provider available for this model")
		return
	}
	status := http.StatusBadGateway
	if lastFailStatus >= 400 {
		status = outboundStatus(lastFailStatus)
	}
	// The client only ever saw this relayed status with zero context, and no
	// request_logs row was written — failures were invisible in the dashboard.
	// Log the upstream's own answer and persist the failed request.
	log.Error().Str("provider", lastProviderName).Str("model", model).Str("endpoint", endpoint).Int("upstream_status", lastFailStatus).Str("detail", truncateDetail(lastDetail)).Msg("all provider attempts failed")
	h.logRequestExtendedBodies(r.Context(), keyPrefix, lastProviderID, model, endpoint, status, time.Since(start).Milliseconds(), 0, 0, 0, isStream, nil, nil, &logMeta{FallbackChain: marshalAttemptChain(chain)})
	terminalError(status, "all provider attempts failed")
}

// truncateDetail bounds upstream error text for structured logs. The cut is
// rune-safe: a byte-blind split can emit invalid UTF-8 into log fields.
func truncateDetail(s string) string {
	if len(s) > 512 {
		return truncateUTF8(s, 512) + "…"
	}
	return s
}

// outboundStatus normalizes an upstream status for client-facing responses.
func outboundStatus(s int) int {
	if s >= 400 && s <= 599 {
		return s
	}
	return http.StatusBadGateway
}

// noRouteFor writes the model_not_routed error: a bare model name matched no
// routing rule, no pin matched, and legacy heuristic fallback is disabled.
// The message is actionable — qualified IDs always work without a rule.
func noRouteFor(w http.ResponseWriter, model string) {
	msg := fmt.Sprintf("model %q is not routed: create a routing rule for it, or use provider-qualified IDs like provider/%s (or an X-Provider header)", model, shortModelID(model))
	// A model with a distinctive capability that is unrouted deserves a message
	// that names the right endpoint. Without this, sending a decision or image
	// model to a text endpoint (or a chat model to /v1/systemone) answered a
	// generic "not routed", which reads like a config problem rather than a
	// wrong-endpoint call. The capability checks are cheap and purely local.
	if decision.IsDecisionModel(model) {
		msg = fmt.Sprintf("model %q is a System One / decision model (typed noul/choice/score questions, no text generation) and has no route on this endpoint; POST /v1/systemone is the decision endpoint", model)
	} else if videogen.IsVideoModel(model) {
		msg = fmt.Sprintf("model %q is a video-generation model and has no route on this endpoint; video models are served at POST /v1/videos", model)
	} else if imagegen.IsImageModel(model) {
		msg = fmt.Sprintf("model %q is an image-generation model and has no route on this endpoint; image models are served at POST /v1/images/generations", model)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": msg,
			"type":    "invalid_request_error",
			"code":    "model_not_routed",
		},
	})
}

// unroutedModel reports whether an empty candidate list means "no routing
// rule for this model" (legacy fallback disabled) as opposed to the legacy
// resolver simply failing (no providers at all). In the former case callers
// surface model_not_routed; in the latter the pre-existing no-provider 503s.
func (h *Handler) unroutedModel() bool {
	return h.LB != nil && !h.LegacyFallback
}
