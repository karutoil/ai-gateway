package proxy

import (
	"context"
	"strings"
	"sync"
	"time"

	"ai-gateway/internal/antigravity"
	"ai-gateway/internal/models"
)

type quotaSnapshot struct {
	quota     *antigravity.Quota
	expiresAt time.Time
}

// quotaCache avoids making a quota RPC on every request. A short TTL is
// intentional: routing should react to a nearly exhausted subscription while
// remaining independent of quota-service latency during the hot path.
type quotaCache struct {
	mu      sync.Mutex
	entries map[string]quotaSnapshot
	ttl     time.Duration
}

func (h *Handler) providerQuotaAllows(ctx context.Context, p *models.Provider, model string, thresholdPct *int) bool {
	if thresholdPct == nil || *thresholdPct <= 0 || p == nil || p.Type != models.ProviderAntigravity || h.ProviderStore == nil {
		return true
	}
	h.quotaCacheMu.Lock()
	c := h.quotaCache
	if c == nil {
		c = newQuotaCache()
		h.quotaCache = c
	}
	h.quotaCacheMu.Unlock()
	if snap, ok := c.get(p.ID); ok {
		return quotaAllows(snap.quota, model, *thresholdPct)
	}
	quotaCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	access, project, _, err := h.ProviderStore.EnsureFreshAccess(quotaCtx, p, h.clientOrDefault())
	if err != nil || access == "" {
		c.put(p.ID, nil)
		return true
	}
	client := *h.clientOrDefault()
	client.Timeout = 5 * time.Second
	q, _ := antigravity.FetchQuota(access, project, p.BaseURL, &client)
	c.put(p.ID, q)
	return quotaAllows(q, model, *thresholdPct)
}

func newQuotaCache() *quotaCache {
	return &quotaCache{entries: make(map[string]quotaSnapshot), ttl: 30 * time.Second}
}

func (c *quotaCache) get(id string) (quotaSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.entries[id]
	if !ok || time.Now().After(v.expiresAt) {
		return quotaSnapshot{}, false
	}
	return v, true
}

func (c *quotaCache) put(id string, q *antigravity.Quota) quotaSnapshot {
	v := quotaSnapshot{quota: q, expiresAt: time.Now().Add(c.ttl)}
	c.mu.Lock()
	c.entries[id] = v
	c.mu.Unlock()
	return v
}

// quotaAllows reports whether a provider's subscription may serve model at a
// consumed-quota threshold. Unknown/unavailable quota is fail-open: a quota
// endpoint outage must not turn a healthy upstream into a gateway outage.
func quotaAllows(q *antigravity.Quota, model string, thresholdPct int) bool {
	if q == nil || thresholdPct <= 0 {
		return true
	}
	model = strings.ToLower(strings.TrimSpace(model))
	for _, m := range q.Models {
		if !quotaModelMatches(m, model) {
			continue
		}
		if m.Exhausted {
			return false
		}
		used := -1.0
		if m.UsedPct != nil {
			used = *m.UsedPct
		} else if m.RemainingPct != nil {
			used = 100 - *m.RemainingPct
		}
		// A quota row without a percentage is not actionable, so keep it
		// eligible rather than guessing that it is exhausted.
		return used < 0 || used < float64(thresholdPct)
	}
	// Some upstream snapshots omit a model temporarily. Fail open so a
	// transiently incomplete quota response does not black-hole traffic.
	return true
}

func quotaModelMatches(q antigravity.ModelQuota, requested string) bool {
	for _, id := range []string{q.ModelID, q.Label} {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == requested || strings.TrimPrefix(id, "models/") == requested {
			return true
		}
	}
	return false
}
