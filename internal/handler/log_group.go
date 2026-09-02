package handler

// GET /api/logs/group — report-style rollup of request_logs grouped by one
// dimension, over the same filter set as /api/logs (log_filters.go).
//
// Designed for the dashboard's Reports panel: "spend by key last 7d",
// "p95 latency by model for failed calls", "error counts by provider" — any
// slice the log filter bar can express, aggregated instead of listed.
//
// Latency percentiles are computed in Go from a collected per-group list
// (same two-pass style as Stats) rather than SQL window functions, keeping
// the query portable across SQLite and Postgres.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"ai-gateway/internal/db"
	"ai-gateway/internal/httperr"
)

// logGroupDim describes one supported grouping dimension: the selected SQL
// expression (aliased "dim") and whether enrichment lookups apply.
type logGroupDim struct {
	expr string
}

var logGroupDims = map[string]logGroupDim{
	"key":           {expr: `COALESCE(NULLIF(rl.key_prefix,''),'(none)')`},
	"model":         {expr: `COALESCE(NULLIF(rl.model,''),'(none)')`},
	"provider":      {expr: `COALESCE(NULLIF(rl.provider_id,''),'(none)')`},
	"endpoint":      {expr: `COALESCE(NULLIF(rl.endpoint,''),'(none)')`},
	"finish_reason": {expr: `COALESCE(NULLIF(rl.finish_reason,''),'(none)')`},
	"status":        {expr: `CAST(rl.status AS TEXT)`},
	"error":         {expr: `CASE WHEN rl.error IS NOT NULL THEN 'error' ELSE 'ok' END`},
}

// LogGroup returns aggregated rows for one grouping dimension.
func (h *AdminHandler) LogGroup(w http.ResponseWriter, r *http.Request) {
	// Own-key-scoped callers bypass the org requirement: their view is
	// already bounded to keys they created (logScopeOwnOnly).
	orgID := ""
	if hasAll, _, _ := logScopeOwnOnly(r, h, "rl"); hasAll {
		var ok bool
		if orgID, ok = h.resolveScope(r); !ok {
			httperr.Forbidden(w, "org scope required")
			return
		}
	}
	q := r.URL.Query()

	dimName := strings.TrimSpace(q.Get("group_by"))
	if dimName == "" {
		dimName = "model"
	}
	dim, known := logGroupDims[dimName]
	if !known {
		httperr.Invalid(w, "group_by must be one of: key, model, provider, endpoint, finish_reason, status, error")
		return
	}

	limit := 20
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 && v <= 100 {
		limit = v
	}

	// Window: same semantics as Stats (24h/7d/30d, default 7d).
	start, rng := parseRangeWindow(q.Get("range"))

	whereSQL, args := logFilterWhere(orgID, q)
	// Ownership narrowing for keys:read_own-only callers (same as /api/logs).
	_, scopeSQL, scopeArgs := logScopeOwnOnly(r, h, "rl")
	if scopeSQL != "" {
		sep := " AND "
		if strings.Contains(whereSQL, "WHERE") {
			sep = " AND "
		} else {
			sep = " WHERE "
		}
		whereSQL += sep + scopeSQL
		args = append(args, scopeArgs...)
	}
	// The window is always applied (range param). Compose safely whether or
	// not the shared builder produced a WHERE clause.
	sep := " WHERE "
	if strings.Contains(whereSQL, "WHERE") {
		sep = " AND "
	}
	fullWhere := whereSQL + sep + "rl.created_at >= ?"
	baseArgs := append([]any{}, args...)
	baseArgs = append(baseArgs, start)

	// Aggregate pass: requests, tokens, cost, failures per group.
	type groupRow struct {
		Dim      string  `json:"group"`
		Requests int64   `json:"requests"`
		Tokens   int64   `json:"tokens"`
		Cost     float64 `json:"cost"`
		Failed   int64   `json:"failed"`
		AvgLatMs float64 `json:"avg_latency_ms"`
	}
	groups := []groupRow{}
	orderExpr := "COUNT(*) DESC"
	switch strings.TrimSpace(q.Get("order")) {
	case "cost":
		orderExpr = "SUM(rl.cost_usd) DESC"
	case "tokens":
		orderExpr = "SUM(rl.total_tokens) DESC"
	}
	rows, err := h.DB.Query(db.Q(`SELECT `+dim.expr+` AS dim, COUNT(*), COALESCE(SUM(rl.total_tokens),0), COALESCE(SUM(rl.cost_usd),0), COALESCE(SUM(CASE WHEN rl.status >= 400 THEN 1 ELSE 0 END),0), COALESCE(AVG(rl.latency_ms),0) FROM request_logs rl LEFT JOIN providers p ON rl.provider_id = p.id`+fullWhere+` GROUP BY dim ORDER BY `+orderExpr+` LIMIT ?`),
		append(baseArgs, limit)...)
	if err == nil {
		for rows.Next() {
			var g groupRow
			var d sql.NullString
			if rows.Scan(&d, &g.Requests, &g.Tokens, &g.Cost, &g.Failed, &g.AvgLatMs) == nil {
				g.Dim = d.String
				groups = append(groups, g)
			}
		}
		rows.Close()
	}

	// Latency pass: collect per-group latencies for percentiles. Bounded by
	// the same filters and a per-group cap to keep memory sane.
	type latList struct {
		dim   string
		latMs []int64
	}
	latMap := map[string][]int64{}
	rows2, err2 := h.DB.Query(db.Q(`SELECT `+dim.expr+` AS dim, rl.latency_ms FROM request_logs rl LEFT JOIN providers p ON rl.provider_id = p.id`+fullWhere+` AND rl.latency_ms IS NOT NULL LIMIT 100000`),
		baseArgs...)
	if err2 == nil {
		for rows2.Next() {
			var d sql.NullString
			var v sql.NullInt64
			if rows2.Scan(&d, &v) == nil && d.Valid && v.Valid {
				latMap[d.String] = append(latMap[d.String], v.Int64)
			}
		}
		rows2.Close()
	}

	// Enrichment: resolve display names for key prefixes / provider ids.
	keyNames := map[string]string{}
	provNames := map[string]string{}
	for _, g := range groups {
		if dimName == "key" {
			var name sql.NullString
			if err := h.DB.QueryRow(db.Q(`SELECT name FROM gateway_keys WHERE prefix=?`), g.Dim).Scan(&name); err == nil && name.Valid {
				keyNames[g.Dim] = name.String
			}
		} else if dimName == "provider" && g.Dim != "(none)" {
			var name sql.NullString
			if err := h.DB.QueryRow(db.Q(`SELECT name FROM providers WHERE id=?`), g.Dim).Scan(&name); err == nil && name.Valid {
				provNames[g.Dim] = name.String
			}
		}
	}

	// Assemble response with percentiles.
	type outRow struct {
		groupRow
		P50LatMs int64  `json:"p50_latency_ms"`
		P95LatMs int64  `json:"p95_latency_ms"`
		Name     string `json:"name,omitempty"`
	}
	out := make([]outRow, 0, len(groups))
	for _, g := range groups {
		lats := latMap[g.Dim]
		sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
		o := outRow{groupRow: g, P50LatMs: percentile(lats, 50), P95LatMs: percentile(lats, 95)}
		if dimName == "key" {
			o.Name = keyNames[g.Dim]
		} else if dimName == "provider" {
			o.Name = provNames[g.Dim]
		}
		out = append(out, o)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"group_by":  dimName,
		"range":     rng,
		"generated": time.Now().UTC().Format(time.RFC3339),
		"rows":      out,
	})
}
