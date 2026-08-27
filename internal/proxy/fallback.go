package proxy

import (
	"bytes"
	"net/http"
	"strings"
	"time"

	"ai-gateway/internal/httperr"
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
func (h *Handler) candidateProviders(rawModel, model, hint string, pred func(*models.Provider) bool) []*models.Provider {
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
		if p, err := h.ProviderStore.GetByName(hint); err == nil {
			consider(p)
			return out
		}
		if p, err := h.ProviderStore.GetByID(hint); err == nil {
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
			if p, err := h.ProviderStore.GetByName(prefix); err == nil && consider(p) {
				return out
			}
			if p, err := h.ProviderStore.GetByType(prefix); err == nil && consider(p) {
				return out
			}
		}
	}

	// 3. Curated LB rule (checked on post-alias model name first, then the
	// raw/alias spelling). One healthy member per request, rotating start.
	if h.LB != nil {
		for _, key := range []string{model, rawModel} {
			key = strings.ToLower(strings.TrimSpace(key))
			if key == "" {
				continue
			}
			if rule := h.LB.RuleForModel(key); rule != nil {
				if rotated := h.LB.RotateProviders(rule); len(rotated) > 0 {
					consider(rotated[0])
					return out
				}
			}
		}
	}

	// 4. Legacy single pick: health-aware round-robin over discovered owners,
	// heuristic ownership by name/type, else default provider.
	if h.ProviderStore != nil {
		if p, err := h.ProviderStore.Resolve(model, ""); err == nil {
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
		opts := proxyOpts{fallbackFrom: fallbackName}

		if isStream {
			outcome := h.proxyWithMetricsOpts(w, r, target, apiKey, outBody, true, model, p.ID, keyPrefix, endpoint, start, isAnth, opts)
			if outcome.committed || !outcome.retriable {
				return // headers already flowed (or hard client-side stop)
			}
			log.Info().Str("candidate", p.Name).Str("model", model).Int("status", outcome.status).Msg("stream candidate failed pre-commit, failing over")
			lastFailStatus = outcome.status
			continue
		}

		bw := newBufWriter()
		outcome := h.proxyWithMetricsOpts(bw, r, target, apiKey, outBody, false, model, p.ID, keyPrefix, endpoint, start, isAnth, opts)
		if !outcome.committed && bw.code == 0 {
			// Terminal pre-commit failure: nothing usable was produced.
			lastFailStatus = outcome.status
			if shouldFailoverFrom(outcome.status) {
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
	httperr.Proxy(w, status, "all provider attempts failed")
}

// outboundStatus normalizes an upstream status for client-facing responses.
func outboundStatus(s int) int {
	if s >= 400 && s <= 599 {
		return s
	}
	return http.StatusBadGateway
}
