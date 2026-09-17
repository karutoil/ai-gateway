package antigravity

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// ModelQuota is one model's remaining subscription quota.
type ModelQuota struct {
	ModelID           string   `json:"model_id"`
	Label             string   `json:"label"`
	RemainingFraction *float64 `json:"remaining_fraction,omitempty"` // 0-1 as reported
	RemainingPct      *float64 `json:"remaining_pct,omitempty"`      // 0-100
	UsedPct           *float64 `json:"used_pct,omitempty"`           // 0-100
	Exhausted         bool     `json:"exhausted"`
	ResetTime         string   `json:"reset_time,omitempty"` // RFC3339 from upstream
	ResetsInMs        *int64   `json:"resets_in_ms,omitempty"`
}

// PromptCredits is the monthly prompt-credit pool from loadCodeAssist.
type PromptCredits struct {
	Available    float64 `json:"available"`
	Monthly      float64 `json:"monthly"`
	UsedPct      float64 `json:"used_pct"`
	RemainingPct float64 `json:"remaining_pct"`
}

// Quota is the parsed Antigravity subscription usage snapshot.
type Quota struct {
	PlanType      string         `json:"plan_type,omitempty"`
	PromptCredits *PromptCredits `json:"prompt_credits,omitempty"`
	Models        []ModelQuota   `json:"models"`
}

// FetchQuota loads plan + per-model remaining quota for an access token.
// project may be empty (resolved to the default seed); baseURL, when set,
// is tried first (custom mirrors) before the built-in endpoint candidates.
func FetchQuota(access, project string, baseURL string, client *http.Client) (*Quota, error) {
	if project == "" {
		project = DefaultProjectID("")
	}
	endpoints := []string{}
	if base := strings.TrimRight(strings.TrimSpace(baseURL), "/"); base != "" {
		endpoints = append(endpoints, base)
	}
	for _, ep := range EndpointCandidates() {
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
	var codeAssist map[string]any
	for _, ep := range endpoints {
		data, _, err := doJSON(client, ep, "/v1internal:loadCodeAssist", access, map[string]any{
			"metadata": map[string]any{"ideType": "ANTIGRAVITY"},
		})
		if err != nil || data == nil {
			continue
		}
		codeAssist = data
		break
	}
	var modelsResp map[string]any
	for _, ep := range endpoints {
		data, _, err := doJSON(client, ep, "/v1internal:fetchAvailableModels", access, map[string]any{
			"project": project,
		})
		if err != nil || data == nil {
			continue
		}
		if m, ok := data["models"].(map[string]any); ok && len(m) > 0 {
			modelsResp = data
			break
		}
		// Keep empty-but-valid responses as fallback; a 200 with no models
		// map still counts as reachable (permissions may hide models).
		if modelsResp == nil {
			modelsResp = data
		}
	}
	if codeAssist == nil && modelsResp == nil {
		return nil, errQuotaUnreachable
	}
	return parseQuota(codeAssist, modelsResp), nil
}

var errQuotaUnreachable = &quotaError{"antigravity quota unavailable"}

type quotaError struct{ msg string }

func (e *quotaError) Error() string { return e.msg }

func parseQuota(codeAssist, modelsResp map[string]any) *Quota {
	q := &Quota{Models: []ModelQuota{}}
	if codeAssist != nil {
		if plan := mapStr(mapPath(codeAssist, "planInfo"), "planType"); plan != "" {
			q.PlanType = plan
		}
		if monthly, ok := mapNum(mapPath(codeAssist, "planInfo"), "monthlyPromptCredits"); ok {
			if avail, ok2 := mapNum(codeAssist, "availablePromptCredits"); ok2 && monthly > 0 {
				used := monthly - avail
				q.PromptCredits = &PromptCredits{
					Available: avail, Monthly: monthly,
					UsedPct:      clampPct(used / monthly * 100),
					RemainingPct: clampPct(avail / monthly * 100),
				}
			}
		}
	}
	if modelsResp != nil {
		if modelsMap, ok := modelsResp["models"].(map[string]any); ok {
			for id, raw := range modelsMap {
				m, _ := raw.(map[string]any)
				if m == nil {
					continue
				}
				if !showQuotaModel(id, m) {
					continue
				}
				label := mapStr(m, "displayName", "label", "model")
				if label == "" {
					label = id
				}
				mq := ModelQuota{ModelID: id, Label: label}
				if qi, ok := m["quotaInfo"].(map[string]any); ok && qi != nil {
					if f, ok := mapNum(qi, "remainingFraction"); ok {
						c := clamp01(f)
						mq.RemainingFraction = &c
						rp := clampPct(c * 100)
						mq.RemainingPct = &rp
						up := clampPct(100 - rp)
						mq.UsedPct = &up
					}
					if b, ok := qi["isExhausted"].(bool); ok && b {
						mq.Exhausted = true
					}
					if rs, _ := qi["resetTime"].(string); strings.TrimSpace(rs) != "" {
						mq.ResetTime = strings.TrimSpace(rs)
						if ms := millisUntilReset(rs); ms != nil {
							mq.ResetsInMs = ms
						}
					}
					if mq.RemainingFraction != nil && *mq.RemainingFraction == 0 {
						mq.Exhausted = true
					}
				}
				q.Models = append(q.Models, mq)
			}
		}
	}
	// Most-used first so the heaviest quota consumers surface at the top;
	// label breaks ties for a stable order.
	sort.SliceStable(q.Models, func(a, b int) bool {
		ua, ub := modelUsed(q.Models[a]), modelUsed(q.Models[b])
		if ua != ub {
			return ua > ub
		}
		return q.Models[a].Label < q.Models[b].Label
	})
	return q
}

func modelUsed(m ModelQuota) float64 {
	if m.Exhausted {
		return 100
	}
	if m.UsedPct != nil {
		return *m.UsedPct
	}
	return -1
}

// showQuotaModel mirrors the community quota tools: skip internal,
// autocomplete-only and image models, and anything without quota info.
func showQuotaModel(id string, m map[string]any) bool {
	if strings.HasPrefix(id, "chat_") || strings.HasPrefix(id, "tab_") {
		return false
	}
	if strings.HasPrefix(id, "rev") {
		return false
	}
	if strings.Contains(id, "image") {
		return false
	}
	if strings.Contains(id, "mquery") || strings.Contains(id, "lite") {
		return false
	}
	if _, ok := m["quotaInfo"].(map[string]any); !ok {
		return false
	}
	return true
}

func mapPath(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if sub, ok := m[key].(map[string]any); ok {
		return sub
	}
	return nil
}

func mapStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func mapNum(m map[string]any, key string) (float64, bool) {
	switch n := m[key].(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

func clampPct(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 100 {
		return 100
	}
	return f
}

func millisUntilReset(resetTime string) *int64 {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(resetTime))
	if err != nil {
		return nil
	}
	ms := t.Sub(time.Now()).Milliseconds()
	if ms <= 0 {
		return nil
	}
	return &ms
}
