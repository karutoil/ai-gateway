package devin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const quotaPath = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"

// Quota is the parsed Devin seat/quota snapshot (plan + remaining budget).
type Quota struct {
	Plan            string   `json:"plan"`
	DailyRemaining  *float64 `json:"daily_remaining_pct,omitempty"`  // 0-100 remaining
	WeeklyRemaining *float64 `json:"weekly_remaining_pct,omitempty"` // 0-100 remaining
	DailyUsed       *float64 `json:"daily_used_pct,omitempty"`       // 0-100 used
	WeeklyUsed      *float64 `json:"weekly_used_pct,omitempty"`      // 0-100 used
	DailyResetAt    *int64   `json:"daily_reset_at_unix,omitempty"`
	WeeklyResetAt   *int64   `json:"weekly_reset_at_unix,omitempty"`
	HideDaily       bool     `json:"hide_daily"`
	HideWeekly      bool     `json:"hide_weekly"`
	ExtraBalanceUSD float64  `json:"extra_balance_usd"`
}

// FetchQuota asks Devin for the seat/quota status for a session token.
// base may be empty (default host). Mirrors pi-devin-provider quota.ts.
func FetchQuota(sessionToken, base string, client *http.Client) (*Quota, error) {
	c := client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	if strings.TrimSpace(base) == "" {
		base = Host()
	}
	payload, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"apiKey":           NormalizeSessionToken(sessionToken),
			"ideName":          "devin",
			"ideVersion":       "1.108.2",
			"extensionName":    "devin",
			"extensionVersion": "1.108.2",
			"locale":           "en",
		},
	})
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+quotaPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("connect-protocol-version", "1")
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("devin quota unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("devin quota request failed (%d)", resp.StatusCode)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("devin quota returned invalid JSON")
	}
	return parseQuota(data)
}

func parseQuota(payload map[string]any) (*Quota, error) {
	userStatus := mapOr(payload, "userStatus", "user_status")
	planInfo := mapOr(payload, "planInfo", "plan_info")
	planStatus := mapOr(userStatus, "planStatus", "plan_status")
	plan := strOr(planInfo, "planName", "plan_name")
	if plan == "" {
		plan = strOr(mapOr(planStatus, "planInfo", "plan_info"), "planName", "plan_name")
	}
	if plan == "" {
		return nil, fmt.Errorf("invalid devin quota response")
	}
	q := &Quota{Plan: plan}
	if v := pctOr(planStatus, "dailyQuotaRemainingPercent", "daily_quota_remaining_percent"); v != nil {
		q.DailyRemaining = v
		used := clampPct(100 - *v)
		q.DailyUsed = &used
	}
	if v := pctOr(planStatus, "weeklyQuotaRemainingPercent", "weekly_quota_remaining_percent"); v != nil {
		q.WeeklyRemaining = v
		used := clampPct(100 - *v)
		q.WeeklyUsed = &used
	}
	if v := intOr(planStatus, "dailyQuotaResetAtUnix", "daily_quota_reset_at_unix"); v != nil {
		q.DailyResetAt = v
	}
	if v := intOr(planStatus, "weeklyQuotaResetAtUnix", "weekly_quota_reset_at_unix"); v != nil {
		q.WeeklyResetAt = v
	}
	if b, ok := boolOr(planInfo, "hideDailyQuota", "hide_daily_quota"); ok {
		q.HideDaily = b
	}
	if b, ok := boolOr(planInfo, "hideWeeklyQuota", "hide_weekly_quota"); ok {
		q.HideWeekly = b
	}
	if v := intOr(planStatus, "overageBalanceMicros", "overage_balance_micros"); v != nil {
		q.ExtraBalanceUSD = float64(*v) / 1e6
	}
	return q, nil
}

func mapOr(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if sub, ok := m[k].(map[string]any); ok {
			return sub
		}
	}
	return nil
}

func strOr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func intOr(m map[string]any, keys ...string) *int64 {
	for _, k := range keys {
		var f float64
		switch n := m[k].(type) {
		case float64:
			f = n
		case float32:
			f = float64(n)
		case int:
			f = float64(n)
		case int64:
			f = float64(n)
		case json.Number:
			v, err := n.Float64()
			if err != nil {
				continue
			}
			f = v
		default:
			continue
		}
		v := int64(f)
		return &v
	}
	return nil
}

func pctOr(m map[string]any, keys ...string) *float64 {
	v := intOr(m, keys...)
	if v == nil {
		return nil
	}
	f := clampPct(float64(*v))
	return &f
}

func boolOr(m map[string]any, keys ...string) (bool, bool) {
	for _, k := range keys {
		if b, ok := m[k].(bool); ok {
			return b, true
		}
	}
	return false, false
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
