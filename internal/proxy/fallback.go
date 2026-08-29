package proxy

import (
	"bytes"
	"net/http"
	"strings"
	"time"

	"ai-gateway/internal/httperr"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"

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
// Routing contract (product decision):
//   - Pin always wins: an X-Provider hint, or a qualified model whose first
//     segment names an existing provider/type ("openai/gpt-4o"), routes there
//     exclusively — LB rules are bypassed.
//   - Otherwise a curated lb_rules group (if configured for this model or its
//     alias name) governs: ONE member is selected per request using a rotating
//     offset (round-robin across requests). Members never chain as failovers.
//   - Otherwise the legacy resolution picks one provider (health-aware,
//     round-robin among discovered owners, deterministic otherwise).
//   - Cross-provider fallback is DISABLED by design: the returned slice always
//     holds at most one provider. Same-provider retries inside
//     proxyWithMetrics remain active per the retry policy.
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
	out := make([]*models.Provider, 0, 1)
	consider := func(p *models.Provider) bool {
		if p == nil || p.ID == "" || (pred != nil && !pred(p)) {
			return false
		}
		out = append(out, p)
		return true
	}

	// 1. Explicit provider hint = hard pin.
	if hint != "" {
		if p, err := h.ProviderStore.GetByName(hint); err == nil && orgAllows(keyOrg, p) {
			consider(p)
			return out
		}
		if p, err := h.ProviderStore.GetByID(hint); err == nil && orgAllows(keyOrg, p) {
			consider(p)
			return out
		}
	}

	// 2. Qualified-ID pin: "provider/model" where prefix matches a provider
	// name or provider TYPE. Purely namespace-ish models like
	// "openrouter/gpt-4o" don't pin unless such a provider exists.
	if idx := strings.Index(model, "/"); idx > 0 {
		prefix := strings.ToLower(strings.TrimSpace(model[:idx]))
		if prefix != "" {
			if p, err := h.ProviderStore.GetByName(prefix); err == nil && orgAllows(keyOrg, p) && consider(p) {
				return out
			}
			if p, err := h.ProviderStore.GetByType(prefix); err == nil && orgAllows(keyOrg, p) && consider(p) {
				return out
			}
		}
	}

	// 3. Curated LB rule (checked on post-alias model name first, then the
	// raw/alias spelling). One member per request, rotating start. If the
	// rotated pick's breaker is OPEN, prefer the next member with a closed
	// circuit instead of 503-ing while healthy group members exist.
	if h.LB != nil {
		for _, key := range []string{model, rawModel} {
			key = strings.ToLower(strings.TrimSpace(key))
			if key == "" {
				continue
			}
			if rule := h.LB.RuleForModel(key); rule != nil {
				if rotated := h.LB.RotateProviders(rule); len(rotated) > 0 {
					// Org-scoped keys may only route to providers their org
					// owns (or global ones).
					eligible := make([]*models.Provider, 0, len(rotated))
					for _, cand := range rotated {
						if orgAllows(keyOrg, cand) {
							eligible = append(eligible, cand)
						}
					}
					if len(eligible) == 0 {
						continue
					}
					picked := eligible[0]
					if h.Breaker != nil {
						for _, cand := range eligible {
							if h.Breaker.State(cand.ID) != "open" {
								picked = cand
								break
							}
						}
					}
					consider(picked)
					return out
				}
			}
		}
	}

	// 4. Legacy single pick: health-aware round-robin over discovered owners,
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
	return out
}

type prepareFn func(p *models.Provider, body []byte) (target, apiKey string, outBody []byte, isAnth bool, err error)

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

func (h *Handler) proxyCandidates(w http.ResponseWriter, r *http.Request, body []byte, isStream bool, model, endpoint, keyPrefix string, start time.Time, candidates []*models.Provider, prepare prepareFn) {
	if len(candidates) == 0 {
		httperr.Proxy(w, http.StatusServiceUnavailable, "no provider configured")
		return
	}
	primaryID := candidates[0].ID

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

	for idx, p := range candidates {
		if !h.breakerOrNoop().Allow(p.ID) {
			continue
		}
		target, apiKey, outBody, isAnth, err := prepare(p, body)
		if err != nil {
			continue
		}
		attempted = true

		fallbackName := ""
		if p.ID != primaryID && idx > 0 && candidates[0] != nil {
			fallbackName = p.Name
		}
		opts := proxyOpts{fallbackFrom: fallbackName, attempts: chain}

		if isStream {
			outcome := h.proxyWithMetricsOpts(w, r, target, apiKey, outBody, true, model, p.ID, keyPrefix, endpoint, start, isAnth, opts)
			lastProviderID, lastProviderName = p.ID, p.Name
			lastDetail = outcome.errSnippet
			if outcome.errSnippet == "" {
				lastDetail = outcome.errText
			}
			// Client-caused 5xx (upstream "invalid_request" semantics):
			// relay the upstream's verdict — no failover, no generic 502.
			if outcome.clientCaused {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(outboundStatus(outcome.status))
				if outcome.errSnippet != "" {
					_, _ = w.Write([]byte(outcome.errSnippet))
				}
				return
			}
			if outcome.committed || !outcome.retriable {
				return // headers already flowed (or hard client-side stop)
			}
			log.Info().Str("candidate", p.Name).Str("model", model).Int("status", outcome.status).Str("detail", truncateDetail(lastDetail)).Msg("stream candidate failed pre-commit, failing over")
			lastFailStatus = outcome.status
			chain = append(chain, providerAttempt{ProviderID: p.ID, Name: p.Name, Status: outcome.status})
			continue
		}

		bw := newBufWriter()
		outcome := h.proxyWithMetricsOpts(bw, r, target, apiKey, outBody, false, model, p.ID, keyPrefix, endpoint, start, isAnth, opts)
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
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(outboundStatus(outcome.status))
				if outcome.errSnippet != "" {
					_, _ = w.Write([]byte(outcome.errSnippet))
				}
				return
			}
			lastFailStatus = outcome.status
			if shouldFailoverFrom(outcome.status) {
				chain = append(chain, providerAttempt{ProviderID: p.ID, Name: p.Name, Status: outcome.status})
				continue
			}
			httperr.Proxy(w, outboundStatus(outcome.status), "upstream unavailable")
			return
		}
		if bw.code > 0 && bw.code < 400 {
			if p.ID != primaryID {
				bw.Header().Set("X-Fallback-Used", p.Name)
				log.Info().Str("fallback_used", p.Name).Str("from", candidates[0].Name).Str("model", model).Msg("fallback_used")
			}
			bw.flushTo(w)
			return
		}
		lastBW, lastProvider = bw, p
		if shouldFailoverFrom(bw.code) {
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
	// Nothing was even attempted → all providers were circuit-open.
	if !attempted {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":{"message":"circuit open for provider","type":"proxy_error","code":"circuit_open"}}`))
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
	h.logRequestExtendedBodies(keyPrefix, lastProviderID, model, endpoint, status, time.Since(start).Milliseconds(), 0, 0, 0, isStream, nil, nil, &logMeta{FallbackChain: marshalAttemptChain(chain)})
	httperr.Proxy(w, status, "all provider attempts failed")
}

// truncateDetail bounds upstream error text for structured logs.
func truncateDetail(s string) string {
	if len(s) > 512 {
		return s[:512] + "…"
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
