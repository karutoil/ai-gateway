package handler

// Per-API-key analytics: GET /api/keys/{id}/analytics?range=24h|7d|30d
//
// Aggregates request_logs rows attributed to a single gateway key (via the
// denormalized key_id column stamped by the proxy). The primary attribution
// path is key_id; a key_prefix fallback keeps legacy rows (written before
// migration 012) visible, matching the boot-time backfill heuristic.
//
// Scope: the key's stored org_id gates access, mirroring the cross-tenant
// guards in Stats/Logs. Dashboard-session requests (key_id NULL) are never
// attributed to any real key.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/db"
	"ai-gateway/internal/httperr"
	"ai-gateway/internal/rbac"

	"github.com/go-chi/chi/v5"
)

type keyDaily struct {
	Day      string  `json:"day"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
	Requests int64   `json:"requests"`
}

type keyTopModel struct {
	Model    string  `json:"model"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
	Requests int64   `json:"requests"`
}

type keyEndpointRow struct {
	Endpoint string  `json:"endpoint"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
	Requests int64   `json:"requests"`
	Failed   int64   `json:"failed"`
}

type keyErrRow struct {
	Status int    `json:"status"`
	Count  int64  `json:"count"`
	Sample string `json:"sample,omitempty"`
}

// parseRangeWindow maps a range query param to a UTC start time. Only
// calendar-aligned windows are supported (24h / 7d / 30d) — per-key analytics
// are consumed by the dashboard, not by programmatic clients. Unknown values
// fall back to 7d (same default as Stats).
func parseRangeWindow(rng string) (time.Time, string) {
	switch rng {
	case "24h":
		return time.Now().UTC().Add(-24 * time.Hour), "24h"
	case "30d":
		return time.Now().UTC().Add(-30 * 24 * time.Hour), "30d"
	default:
		return time.Now().UTC().Add(-7 * 24 * time.Hour), "7d"
	}
}

// KeyAnalytics returns usage rollups for one gateway key over a time range.
func (h *AdminHandler) KeyAnalytics(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		httperr.Invalid(w, "key id required")
		return
	}

	// Load the key first so org scoping uses its stored org_id (the
	// cross-tenant guard), not a client-supplied parameter.
	var keyOrgID, keyPrefix, keyName, keyCreatedBy string
	err := h.DB.QueryRow(db.Q(`SELECT COALESCE(org_id,''), prefix, name, COALESCE(created_by,'') FROM gateway_keys WHERE id=?`), id).Scan(&keyOrgID, &keyPrefix, &keyName, &keyCreatedBy)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	// Ownership guard: keys:read_own-only callers may only view analytics for
	// keys they created (mirrors ListKeys scoping). 404, not 403, so key ids
	// from other users are not enumerable.
	role := auth.GetRole(r)
	if role != "admin" {
		perms := auth.GetPerms(r)
		if perms != nil && rbac.Has(perms, "keys:read_own") && !rbac.Has(perms, "keys:read") {
			uid := h.callerUserID(r)
			if keyCreatedBy == "" || keyCreatedBy != uid {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
		}
	}
	// Same scoping contract as resolveScope: admins see everything; other
	// roles only their own org; unscoped non-admins only while the
	// deployment has no organizations (single-tenant). Denials 404 (not
	// 403) so key ids from other orgs are not enumerable.

	// Own-key-scoped callers bypass the org check when the key is theirs:
	// the ownership guard above already restricted them to their own keys,
	// and org membership is orthogonal to key ownership.
	ownScopedOwner := false
	if role != "admin" {
		perms := auth.GetPerms(r)
		if perms == nil {
			perms = rbac.Defaults(role)
		}
		ownScopedOwner = rbac.Has(perms, rbac.PermKeysReadOwn) &&
			rbac.Has(perms, rbac.PermLogsRead) == false &&
			keyCreatedBy != "" && keyCreatedBy == h.callerUserID(r)
	}
	if auth.GetRole(r) != "admin" && !ownScopedOwner {
		claim := auth.GetOrgID(r)
		if claim == "" {
			if !h.globalScopeAllowed() {
				httperr.Forbidden(w, "org scope required")
				return
			}
		} else if claim != keyOrgID {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
	}

	start, rng := parseRangeWindow(r.URL.Query().Get("range"))

	// Attribution predicate: key_id first (all rows since migration 012),
	// prefix fallback for legacy rows; the key_id IS NULL guard keeps the
	// OR branch from double-counting migrated rows.
	attrib := `(rl.key_id = ? OR (rl.key_id IS NULL AND rl.key_prefix = ?))`
	baseWhere := ` WHERE rl.created_at >= ? AND ` + attrib
	// Argument order follows the predicate: window first, then the two
	// attribution params (id, prefix).
	baseArgs := []any{start, id, keyPrefix}

	// Totals over the whole table (badge numbers on the Keys page)...
	var allReqs, allTokens, allFailed int64
	var allCost float64
	h.DB.QueryRow(db.Q(`SELECT COUNT(*), COALESCE(SUM(rl.total_tokens),0), COALESCE(SUM(rl.cost_usd),0), COALESCE(SUM(CASE WHEN rl.status >= 400 THEN 1 ELSE 0 END),0) FROM request_logs rl WHERE rl.key_id = ? OR (rl.key_id IS NULL AND rl.key_prefix = ?)`), id, keyPrefix).Scan(&allReqs, &allTokens, &allCost, &allFailed)

	// ...and over the selected window (modal KPIs).
	var rngReqs, rngTokens, rngFailed int64
	var rngCost float64
	h.DB.QueryRow(db.Q(`SELECT COUNT(*), COALESCE(SUM(rl.total_tokens),0), COALESCE(SUM(rl.cost_usd),0), COALESCE(SUM(CASE WHEN rl.status >= 400 THEN 1 ELSE 0 END),0) FROM request_logs rl`+baseWhere), baseArgs...).Scan(&rngReqs, &rngTokens, &rngCost, &rngFailed)
	rngSuccessful := rngReqs - rngFailed

	// Daily/hourly buckets (hourly for 24h, matching Stats).
	bucketExpr := "date(rl.created_at)"
	if rng == "24h" {
		bucketExpr = `strftime('%Y-%m-%dT%H:00:00Z', rl.created_at)`
	}
	daily := []keyDaily{}
	rows, err := h.DB.Query(db.Q(`SELECT `+bucketExpr+`, COALESCE(SUM(rl.total_tokens),0), COALESCE(SUM(rl.cost_usd),0), COUNT(*) FROM request_logs rl`+baseWhere+` GROUP BY `+bucketExpr+` ORDER BY 1`), baseArgs...)
	if err == nil {
		for rows.Next() {
			var d keyDaily
			var day sql.NullString
			if rows.Scan(&day, &d.Tokens, &d.Cost, &d.Requests) == nil && day.Valid {
				d.Day = day.String
				daily = append(daily, d)
			}
		}
		rows.Close()
	}

	// Top models within the window.
	topModels := []keyTopModel{}
	rows2, err2 := h.DB.Query(db.Q(`SELECT rl.model, COALESCE(SUM(rl.total_tokens),0), COALESCE(SUM(rl.cost_usd),0), COUNT(*) FROM request_logs rl`+baseWhere+` AND rl.model != '' GROUP BY rl.model ORDER BY SUM(rl.total_tokens) DESC LIMIT 10`), baseArgs...)
	if err2 == nil {
		for rows2.Next() {
			var m keyTopModel
			if rows2.Scan(&m.Model, &m.Tokens, &m.Cost, &m.Requests) == nil {
				topModels = append(topModels, m)
			}
		}
		rows2.Close()
	}

	// Endpoint breakdown within the window.
	endpoints := []keyEndpointRow{}
	rows3, err3 := h.DB.Query(db.Q(`SELECT COALESCE(NULLIF(rl.endpoint,''),'unknown'), COALESCE(SUM(rl.total_tokens),0), COALESCE(SUM(rl.cost_usd),0), COUNT(*), COALESCE(SUM(CASE WHEN rl.status >= 400 THEN 1 ELSE 0 END),0) FROM request_logs rl`+baseWhere+` GROUP BY COALESCE(NULLIF(rl.endpoint,''),'unknown') ORDER BY COUNT(*) DESC LIMIT 10`), baseArgs...)
	if err3 == nil {
		for rows3.Next() {
			var e keyEndpointRow
			if rows3.Scan(&e.Endpoint, &e.Tokens, &e.Cost, &e.Requests, &e.Failed) == nil {
				endpoints = append(endpoints, e)
			}
		}
		rows3.Close()
	}

	// Latency percentiles within the window.
	var latencies []int64
	rows4, err4 := h.DB.Query(db.Q(`SELECT rl.latency_ms FROM request_logs rl`+baseWhere+` AND rl.latency_ms IS NOT NULL`), baseArgs...)
	if err4 == nil {
		for rows4.Next() {
			var v sql.NullInt64
			if rows4.Scan(&v) == nil && v.Valid {
				latencies = append(latencies, v.Int64)
			}
		}
		rows4.Close()
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	var p50, p95 int64
	var avgLat float64
	if len(latencies) > 0 {
		p50 = percentile(latencies, 50)
		p95 = percentile(latencies, 95)
		var sum int64
		for _, v := range latencies {
			sum += v
		}
		avgLat = float64(sum) / float64(len(latencies))
	}

	// Error breakdown within the window: count per status code with the most
	// recent error text seen for that status (same shape as Stats.errors).
	errors := []keyErrRow{}
	rows5, err5 := h.DB.Query(db.Q(`SELECT rl.status, COUNT(*), (SELECT rl2.error FROM request_logs rl2 WHERE rl2.status = rl.status AND rl2.error IS NOT NULL AND rl2.created_at >= ? AND (rl2.key_id = ? OR (rl2.key_id IS NULL AND rl2.key_prefix = ?)) ORDER BY rl2.created_at DESC LIMIT 1) FROM request_logs rl`+baseWhere+` AND rl.status >= 400 GROUP BY rl.status ORDER BY COUNT(*) DESC LIMIT 10`),
		start, id, keyPrefix, start, id, keyPrefix)
	if err5 == nil {
		for rows5.Next() {
			var e keyErrRow
			var status sql.NullInt64
			var sample sql.NullString
			if rows5.Scan(&status, &e.Count, &sample) == nil && status.Valid {
				e.Status = int(status.Int64)
				e.Sample = sample.String
				errors = append(errors, e)
			}
		}
		rows5.Close()
	}

	// Avg TTFT within the window.
	var avgTTFT float64
	h.DB.QueryRow(db.Q(`SELECT COALESCE(AVG(rl.ttft_ms),0) FROM request_logs rl`+baseWhere+` AND rl.ttft_ms > 0`), baseArgs...).Scan(&avgTTFT)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"key_id": id,
		"prefix": keyPrefix,
		"name":   keyName,
		"range":  rng,
		"all_time": map[string]any{
			"requests": allReqs,
			"tokens":   allTokens,
			"cost":     allCost,
			"failed":   allFailed,
		},
		"range_requests":   rngReqs,
		"range_tokens":     rngTokens,
		"range_cost":       rngCost,
		"range_failed":     rngFailed,
		"range_successful": rngSuccessful,
		"daily":            daily,
		"top_models":       topModels,
		"endpoints":        endpoints,
		"latency": map[string]any{
			"p50":   p50,
			"p95":   p95,
			"avg":   avgLat,
			"count": len(latencies),
		},
		"ttft_avg": avgTTFT,
		"errors":   errors,
	})
}
