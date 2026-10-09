package handler

// Reporting endpoints behind the dashboard's attribution, cost and
// performance pages:
//
//	GET /api/analytics/attribution  who used the gateway (app / end user)
//	GET /api/analytics/cost         what it cost, and what caching saved
//	GET /api/analytics/performance  how the upstreams behaved
//
// All three share one window contract (range shorthand or explicit from/to),
// one tenant-scoping path, and the same two-pass percentile style used by
// Stats and LogGroup so the queries stay portable across SQLite and Postgres.
//
// Scoping note: these endpoints aggregate EVERY request_logs row in the
// window, so they must apply the same org + ownership narrowing as /api/logs.
// Anything less would leak another tenant's spend through a rollup.

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

	"github.com/rs/zerolog/log"
)

/* ------------------------------------------------------------------ */
/* Window + scope                                                      */
/* ------------------------------------------------------------------ */

// analyticsWindows maps the shorthand ranges the UI offers.
var analyticsWindows = map[string]time.Duration{
	"1h":   time.Hour,
	"6h":   6 * time.Hour,
	"24h":  24 * time.Hour,
	"7d":   7 * 24 * time.Hour,
	"30d":  30 * 24 * time.Hour,
	"90d":  90 * 24 * time.Hour,
	"365d": 365 * 24 * time.Hour,
}

// analyticsWindow resolves the reporting window plus the caller's row scope.
//
// Precedence: explicit from/to (RFC3339) > range shorthand > 7d default. An
// explicit window is what makes "compare this month to last month" possible;
// the shorthand is what the quick-pick buttons send.
func (h *AdminHandler) analyticsWindow(w http.ResponseWriter, r *http.Request) (where string, args []any, rng string, start, end time.Time, hourly bool, ok bool) {
	now := time.Now().UTC()
	end = now
	start = now.Add(-7 * 24 * time.Hour)
	rng = "7d"

	q := r.URL.Query()
	fromStr := strings.TrimSpace(q.Get("from"))
	toStr := strings.TrimSpace(q.Get("to"))
	usedExplicit := false
	if fromStr != "" {
		if t, err := time.Parse(time.RFC3339, fromStr); err == nil {
			start = t.UTC()
			usedExplicit = true
		}
	}
	if toStr != "" {
		if t, err := time.Parse(time.RFC3339, toStr); err == nil {
			end = t.UTC()
			usedExplicit = true
		}
	}
	if usedExplicit {
		rng = "custom"
		// Tolerate a reversed pair rather than returning an empty report.
		if start.After(end) {
			start, end = end, start
		}
	} else if r := strings.TrimSpace(q.Get("range")); r != "" {
		if d, known := analyticsWindows[r]; known {
			start = now.Add(-d)
			rng = r
		}
	}

	// Same scope contract as /api/logs: org-bound callers must resolve an org,
	// and keys:read_own-only callers see only their own keys' traffic.
	orgID := ""
	if hasAll, _, _ := logScopeOwnOnly(r, h, "rl"); hasAll {
		var resolved bool
		if orgID, resolved = h.resolveScope(r); !resolved {
			httperr.Forbidden(w, "org scope required")
			return "", nil, rng, start, end, hourly, false
		}
	}
	whereSQL, argList := logFilterWhere(orgID, q)
	sep := " WHERE "
	if strings.Contains(whereSQL, "WHERE") {
		sep = " AND "
	}
	whereSQL += sep + "rl.created_at >= ? AND rl.created_at <= ?"
	argList = append(argList, start, end)

	if _, scopeSQL, scopeArgs := logScopeOwnOnly(r, h, "rl"); scopeSQL != "" {
		whereSQL += " AND " + scopeSQL
		argList = append(argList, scopeArgs...)
	}

	// Hourly buckets keep short windows readable; multi-day windows use a
	// calendar day so a week does not turn into hundreds of tiny bars.
	span := end.Sub(start)
	hourly = span <= 48*time.Hour
	return whereSQL, argList, rng, start, end, hourly, true
}

/* ------------------------------------------------------------------ */
/* Attribution                                                         */
/* ------------------------------------------------------------------ */

// attributionRow is one ranked identity (app or end user).
type attributionRow struct {
	Name     string  `json:"name"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
	Failed   int64   `json:"failed"`
	// Identities counts distinct counterpart identities: for an app, how many
	// end users it served; for an end user, how many apps they used. This is
	// what turns a flat ranking into something you can reason about.
	Identities int64  `json:"identities"`
	LastSeen   string `json:"last_seen,omitempty"`
}

// Attribution reports usage grouped by application and by end user.
//
// end_user is a RANKING, not a group-by dimension: it is unbounded by
// construction, so the query is bounded by LIMIT instead of materialising
// every identity. That keeps the high-cardinality dimension useful (top
// spenders, heaviest users) without the rollup cost that makes grouping by it
// untenable — the same filter/rank-versus-group split Langfuse enforces.
func (h *AdminHandler) Attribution(w http.ResponseWriter, r *http.Request) {
	whereSQL, args, rng, start, end, _, ok := h.analyticsWindow(w, r)
	if !ok {
		return
	}
	limit := 25
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 100 {
		limit = v
	}

	apps := h.attributionBy("app", "end_user", whereSQL, args, limit)
	users := h.attributionBy("end_user", "app", whereSQL, args, limit)

	// Coverage tells the operator how much traffic actually carries an
	// identity — without it, a low ranking is indistinguishable from an
	// integration that never sends attribution.
	var totalRows, attributedApps, attributedUsers, unpriced, unpricedTokens int64
	h.DB.QueryRow(db.Q(`SELECT COUNT(*) FROM request_logs rl`+whereSQL), args...).Scan(&totalRows)
	h.DB.QueryRow(db.Q(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN rl.end_user IS NOT NULL AND rl.end_user != '' THEN 1 ELSE 0 END),0) FROM request_logs rl`+whereSQL), args...).Scan(&attributedApps, &attributedUsers)
	h.DB.QueryRow(db.Q(`SELECT COUNT(*), COALESCE(SUM(rl.total_tokens),0) FROM request_logs rl`+whereSQL+` AND rl.cost_usd = 0 AND rl.total_tokens > 0`), args...).Scan(&unpriced, &unpricedTokens)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"range":     rng,
		"from":      start.Format(time.RFC3339),
		"to":        end.Format(time.RFC3339),
		"generated": time.Now().UTC().Format(time.RFC3339),
		"apps":      apps,
		"end_users": users,
		"coverage": map[string]any{
			"total_requests":       totalRows,
			"with_end_user":        attributedUsers,
			"unattributed":         totalRows - attributedUsers,
			"unpriced_requests":    unpriced,
			"unpriced_token_share": unpricedTokens,
		},
	})
}

// attributionBy ranks one identity column, counting distinct values of the
// counterpart column. col/counter are internal literals, never user input.
func (h *AdminHandler) attributionBy(col, counter, whereSQL string, args []any, limit int) []attributionRow {
	out := []attributionRow{}
	// Groups by the qualified column, not the alias — see costBy for why.
	rows, err := h.DB.Query(db.Q(`SELECT rl.`+col+` AS name, COUNT(*), COALESCE(SUM(rl.total_tokens),0), COALESCE(SUM(rl.cost_usd),0), COALESCE(SUM(CASE WHEN rl.status >= 400 THEN 1 ELSE 0 END),0), COUNT(DISTINCT NULLIF(rl.`+counter+`,'')), MAX(rl.created_at) FROM request_logs rl`+whereSQL+` AND rl.`+col+` IS NOT NULL AND rl.`+col+` != '' GROUP BY rl.`+col+` ORDER BY SUM(rl.cost_usd) DESC, COUNT(*) DESC LIMIT ?`),
		append(append([]any{}, args...), limit)...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var a attributionRow
		var last sql.NullString
		if err := rows.Scan(&a.Name, &a.Requests, &a.Tokens, &a.Cost, &a.Failed, &a.Identities, &last); err != nil {
			// Previously swallowed: a scan mismatch produced an empty ranking
			// that looked exactly like "no attributed traffic".
			log.Error().Err(err).Str("dimension", col).Msg("attribution row skipped: scan failed")
			continue
		}
		if last.Valid {
			if ts, ok := parseDBTime(last.String); ok {
				a.LastSeen = ts.UTC().Format(time.RFC3339)
			}
		}
		out = append(out, a)
	}
	return out
}

// parseDBTime parses the timestamp encodings a SQLite aggregate can hand back.
//
// MAX()/MIN() over a DATETIME column loses the column's type affinity, so the
// driver returns a raw STRING rather than a time.Time — scanning it into
// sql.NullTime fails outright ("unsupported Scan, storing driver.Value type
// string into type *time.Time"). The layouts below cover what the driver
// emits: a space separator instead of "T", and up to nanosecond precision with
// an offset. Postgres returns a real timestamp and is unaffected.
func parseDBTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
		time.DateTime,
		time.DateOnly,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

/* ------------------------------------------------------------------ */
/* Cost                                                                */
/* ------------------------------------------------------------------ */

// Cost reports spend, the cache discount, and where unpriced traffic hides.
//
// Cache savings are computed per row against the catalog: cached input would
// have been billed at the standard input rate before this gateway became
// cache-aware, so the difference is real, recoverable spend rather than a
// cosmetic metric. Rows whose model is absent from the catalog contribute no
// estimated savings because their cache-read rate is unknown. Separately,
// zero-cost rows with tokens are flagged as possible pricing gaps.
func (h *AdminHandler) Cost(w http.ResponseWriter, r *http.Request) {
	whereSQL, args, rng, start, end, hourly, ok := h.analyticsWindow(w, r)
	if !ok {
		return
	}

	const costJoin = ` LEFT JOIN models_catalog mc ON mc.id = rl.model`

	var totalCost, cacheSavings sql.NullFloat64
	var cacheRead, cacheWrite, promptTokens, completionTokens, reasoningTokens sql.NullInt64
	h.DB.QueryRow(db.Q(`SELECT COALESCE(SUM(rl.cost_usd),0),
		COALESCE(SUM(CASE WHEN mc.input_cost > 0 AND COALESCE(mc.cache_read_cost,0) > 0 AND mc.cache_read_cost < mc.input_cost
			THEN rl.cache_read_tokens * (mc.input_cost - mc.cache_read_cost) / 1000000.0 ELSE 0 END),0),
		COALESCE(SUM(rl.cache_read_tokens),0), COALESCE(SUM(rl.cache_write_tokens),0),
		COALESCE(SUM(rl.prompt_tokens),0), COALESCE(SUM(rl.completion_tokens),0),
		COALESCE(SUM(rl.reasoning_tokens),0)
		FROM request_logs rl`+costJoin+whereSQL), args...).
		Scan(&totalCost, &cacheSavings, &cacheRead, &cacheWrite, &promptTokens, &completionTokens, &reasoningTokens)

	// Bucketed spend + savings for the trend chart.
	bucketExpr := db.DateBucketExpr("rl.created_at")
	if hourly {
		bucketExpr = db.HourBucketExpr("rl.created_at")
	}
	type costPoint struct {
		Day          string  `json:"day"`
		Cost         float64 `json:"cost"`
		CacheSavings float64 `json:"cache_savings"`
		Requests     int64   `json:"requests"`
	}
	daily := []costPoint{}
	if rows, err := h.DB.Query(db.Q(`SELECT `+bucketExpr+` AS day, COALESCE(SUM(rl.cost_usd),0),
		COALESCE(SUM(CASE WHEN mc.input_cost > 0 AND COALESCE(mc.cache_read_cost,0) > 0 AND mc.cache_read_cost < mc.input_cost
			THEN rl.cache_read_tokens * (mc.input_cost - mc.cache_read_cost) / 1000000.0 ELSE 0 END),0), COUNT(*)
		FROM request_logs rl`+costJoin+whereSQL+` GROUP BY `+bucketExpr+` ORDER BY 1`), args...); err == nil {
		for rows.Next() {
			var p costPoint
			if err := rows.Scan(&p.Day, &p.Cost, &p.CacheSavings, &p.Requests); err != nil {
				log.Error().Err(err).Msg("cost trend bucket skipped: scan failed")
				continue
			}
			daily = append(daily, p)
		}
		if err := rows.Err(); err != nil {
			log.Error().Err(err).Msg("cost trend iteration failed")
		}
		rows.Close()
	} else {
		log.Error().Err(err).Msg("cost trend query failed")
	}

	// Cost by cache disposition, model, provider and app. app is the
	// attribution tie-in: spend per application is the chargeback view.
	byCacheStatus := h.costBy("COALESCE(NULLIF(rl.cache_status,''),'unknown')", whereSQL, args, costJoin, 10)
	byModel := h.costBy("COALESCE(NULLIF(rl.model,''),'(none)')", whereSQL, args, costJoin, 25)
	byProvider := h.costBy("COALESCE(NULLIF(p.name,''),'(none)')", whereSQL, args, ` LEFT JOIN providers p ON rl.provider_id = p.id`, 25)
	byApp := h.costBy("COALESCE(NULLIF(rl.app,''),'(unattributed)')", whereSQL, args, costJoin, 25)
	byKey := h.costBy("COALESCE(NULLIF(rl.key_prefix,''),'(none)')", whereSQL, args, costJoin, 25)

	var unpriced, unpricedTokens int64
	h.DB.QueryRow(db.Q(`SELECT COUNT(*), COALESCE(SUM(rl.total_tokens),0) FROM request_logs rl`+whereSQL+` AND rl.cost_usd = 0 AND rl.total_tokens > 0`), args...).Scan(&unpriced, &unpricedTokens)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"range":     rng,
		"from":      start.Format(time.RFC3339),
		"to":        end.Format(time.RFC3339),
		"generated": time.Now().UTC().Format(time.RFC3339),
		"totals": map[string]any{
			"cost_usd":           totalCost.Float64,
			"cache_savings_usd":  cacheSavings.Float64,
			"cache_read_tokens":  cacheRead.Int64,
			"cache_write_tokens": cacheWrite.Int64,
			"prompt_tokens":      promptTokens.Int64,
			"completion_tokens":  completionTokens.Int64,
			"reasoning_tokens":   reasoningTokens.Int64,
			"unpriced_requests":  unpriced,
			"unpriced_tokens":    unpricedTokens,
		},
		"daily":           daily,
		"by_cache_status": byCacheStatus,
		"by_model":        byModel,
		"by_provider":     byProvider,
		"by_app":          byApp,
		"by_key":          byKey,
	})
}

// costBy rolls spend up by one expression. expr is an internal literal.
func (h *AdminHandler) costBy(expr, whereSQL string, args []any, join string, limit int) []attributionRow {
	out := []attributionRow{}
	// GROUP BY the full expression, never the "name" alias: several joins bring
	// their own `name` column (models_catalog.name, providers.name), and the
	// alias is not guaranteed to win. Grouping by the wrong column silently
	// merges or splits buckets while still labelling them with expr — the
	// numbers look plausible and are wrong.
	rows, err := h.DB.Query(db.Q(`SELECT `+expr+` AS name, COUNT(*), COALESCE(SUM(rl.total_tokens),0), COALESCE(SUM(rl.cost_usd),0),
		COALESCE(SUM(CASE WHEN rl.status >= 400 THEN 1 ELSE 0 END),0), 0
		FROM request_logs rl`+join+whereSQL+` GROUP BY `+expr+` ORDER BY SUM(rl.cost_usd) DESC LIMIT ?`),
		append(append([]any{}, args...), limit)...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var a attributionRow
		if rows.Scan(&a.Name, &a.Requests, &a.Tokens, &a.Cost, &a.Failed, &a.Identities) == nil {
			out = append(out, a)
		}
	}
	return out
}

/* ------------------------------------------------------------------ */
/* Performance                                                         */
/* ------------------------------------------------------------------ */

// perfRow is the upstream-behaviour summary for one provider or model.
type perfRow struct {
	Name        string  `json:"name"`
	Requests    int64   `json:"requests"`
	Failed      int64   `json:"failed"`
	ErrorRate   float64 `json:"error_rate"`
	UptimePct   float64 `json:"uptime_pct"`
	P50Ms       int64   `json:"p50_ms"`
	P95Ms       int64   `json:"p95_ms"`
	P99Ms       int64   `json:"p99_ms"`
	AvgMs       int64   `json:"avg_ms"`
	TTFTP50Ms   int64   `json:"ttft_p50_ms"`
	TTFTP95Ms   int64   `json:"ttft_p95_ms"`
	AvgTPS      float64 `json:"avg_tps"`
	StreamShare float64 `json:"stream_share"`
	// Health is the provider's current health_check status (providers only).
	Health string `json:"health,omitempty"`
}

// Performance reports per-provider and per-model reliability and speed.
//
// Everything here is derived from request_logs, which the gateway has always
// recorded — latency, TTFT, status, tokens — but never aggregated. Uptime is
// the success ratio over the window, NOT the point-in-time health flag, so a
// provider that flaps for an hour shows it.
func (h *AdminHandler) Performance(w http.ResponseWriter, r *http.Request) {
	whereSQL, args, rng, start, end, hourly, ok := h.analyticsWindow(w, r)
	if !ok {
		return
	}

	providerJoin := ` LEFT JOIN providers p ON rl.provider_id = p.id`
	providers := h.perfBy("COALESCE(NULLIF(p.name,''),'(none)')", whereSQL, args, providerJoin, true, 50)
	models := h.perfBy("COALESCE(NULLIF(rl.model,''),'(none)')", whereSQL, args, providerJoin, false, 50)

	bucketExpr := db.DateBucketExpr("rl.created_at")
	if hourly {
		bucketExpr = db.HourBucketExpr("rl.created_at")
	}
	type perfPoint struct {
		Day      string `json:"day"`
		Requests int64  `json:"requests"`
		Failed   int64  `json:"failed"`
		AvgMs    int64  `json:"avg_ms"`
		AvgTTFT  int64  `json:"avg_ttft_ms"`
	}
	daily := []perfPoint{}
	if rows, err := h.DB.Query(db.Q(`SELECT `+bucketExpr+` AS day, COUNT(*),
		COALESCE(SUM(CASE WHEN rl.status >= 400 THEN 1 ELSE 0 END),0),
		COALESCE(AVG(rl.latency_ms),0), COALESCE(AVG(CASE WHEN rl.is_stream = `+db.BoolLit(true)+` THEN NULLIF(rl.ttft_ms,0) END),0)
		FROM request_logs rl`+whereSQL+` GROUP BY `+bucketExpr+` ORDER BY 1`), args...); err == nil {
		for rows.Next() {
			var p perfPoint
			// AVG() returns a float64 even over an INTEGER column, so the
			// averages must be scanned as float and narrowed explicitly.
			// Scanning them straight into int64 fails the conversion, and the
			// swallowed error silently produced an EMPTY trend series.
			var avgMs, avgTTFT float64
			if err := rows.Scan(&p.Day, &p.Requests, &p.Failed, &avgMs, &avgTTFT); err != nil {
				log.Error().Err(err).Msg("performance daily bucket skipped: scan failed")
				continue
			}
			p.AvgMs = int64(avgMs)
			p.AvgTTFT = int64(avgTTFT)
			daily = append(daily, p)
		}
		if err := rows.Err(); err != nil {
			log.Error().Err(err).Msg("performance trend iteration failed")
		}
		rows.Close()
	} else {
		log.Error().Err(err).Msg("performance trend query failed")
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"range":     rng,
		"from":      start.Format(time.RFC3339),
		"to":        end.Format(time.RFC3339),
		"generated": time.Now().UTC().Format(time.RFC3339),
		"providers": providers,
		"models":    models,
		"daily":     daily,
	})
}

// perfBy aggregates reliability and latency for one grouping expression.
// Percentiles are computed in Go from a bounded per-group sample so the query
// stays portable across SQLite and Postgres.
func (h *AdminHandler) perfBy(expr, whereSQL string, args []any, join string, withHealth bool, limit int) []perfRow {
	type agg struct {
		row    perfRow
		lats   []int64
		ttfts  []int64
		stream int64
		tpsSum float64
		tpsN   int64
	}
	groups := map[string]*agg{}
	order := []string{}

	// Same alias hazard as costBy: providers.name collides with the alias.
	rows, err := h.DB.Query(db.Q(`SELECT `+expr+` AS name, COUNT(*),
		COALESCE(SUM(CASE WHEN rl.status >= 400 THEN 1 ELSE 0 END),0),
		COALESCE(AVG(rl.latency_ms),0),
		COALESCE(SUM(CASE WHEN rl.is_stream = `+db.BoolLit(true)+` THEN 1 ELSE 0 END),0)
		FROM request_logs rl`+join+whereSQL+` GROUP BY `+expr+` ORDER BY COUNT(*) DESC LIMIT ?`),
		append(append([]any{}, args...), limit)...)
	if err != nil {
		return []perfRow{}
	}
	for rows.Next() {
		var name string
		var reqs, failed, streams int64
		var avgMs float64
		if rows.Scan(&name, &reqs, &failed, &avgMs, &streams) != nil {
			continue
		}
		a := &agg{row: perfRow{Name: name, Requests: reqs, Failed: failed, AvgMs: int64(avgMs), StreamShare: ratio(streams, reqs)}}
		a.row.ErrorRate = ratio(failed, reqs)
		a.row.UptimePct = (1 - a.row.ErrorRate) * 100
		groups[name] = a
		order = append(order, name)
	}
	rows.Close()
	if len(order) == 0 {
		return []perfRow{}
	}

	// Second pass: per-group latency, TTFT and throughput samples.
	sample, err := h.DB.Query(db.Q(`SELECT `+expr+` AS name, rl.latency_ms, rl.ttft_ms, rl.response_ms, rl.completion_tokens
		FROM request_logs rl`+join+whereSQL+` LIMIT 200000`), args...)
	if err == nil {
		for sample.Next() {
			var name sql.NullString
			var lat, ttft, respMs, completion sql.NullInt64
			if sample.Scan(&name, &lat, &ttft, &respMs, &completion) != nil || !name.Valid {
				continue
			}
			a := groups[name.String]
			if a == nil {
				continue
			}
			if lat.Valid {
				a.lats = append(a.lats, lat.Int64)
			}
			if ttft.Valid && ttft.Int64 > 0 {
				a.ttfts = append(a.ttfts, ttft.Int64)
			}
			// Throughput over the post-TTFT window only: including the wait
			// for the first token would understate generation speed.
			if respMs.Valid && respMs.Int64 > 0 && completion.Valid && completion.Int64 > 0 {
				a.tpsSum += float64(completion.Int64) / (float64(respMs.Int64) / 1000.0)
				a.tpsN++
			}
		}
		sample.Close()
	}

	out := make([]perfRow, 0, len(order))
	for _, name := range order {
		a := groups[name]
		sort.Slice(a.lats, func(i, j int) bool { return a.lats[i] < a.lats[j] })
		sort.Slice(a.ttfts, func(i, j int) bool { return a.ttfts[i] < a.ttfts[j] })
		a.row.P50Ms = percentile(a.lats, 50)
		a.row.P95Ms = percentile(a.lats, 95)
		a.row.P99Ms = percentile(a.lats, 99)
		a.row.TTFTP50Ms = percentile(a.ttfts, 50)
		a.row.TTFTP95Ms = percentile(a.ttfts, 95)
		if a.tpsN > 0 {
			a.row.AvgTPS = a.tpsSum / float64(a.tpsN)
		}
		out = append(out, a.row)
	}

	if withHealth {
		for i := range out {
			var hs sql.NullString
			if h.DB.QueryRow(db.Q(`SELECT health_status FROM providers WHERE name=?`), out[i].Name).Scan(&hs) == nil && hs.Valid {
				out[i].Health = hs.String
			}
		}
	}
	return out
}

func ratio(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) / float64(whole)
}
