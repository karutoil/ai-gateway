package budget

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"ai-gateway/internal/db"
	"ai-gateway/internal/webhook"

	"github.com/rs/zerolog/log"
)

// Limiter enforces per-key budget quotas (daily/monthly tokens and cost)
type Limiter interface {
	Check(prefix string, promptTokens int) error
	RecordUsage(prefix string, tokens int, costCents int, at time.Time)
}

// MemoryLimiter is used in unit tests for quota enforcement without DB
type MemoryLimiter struct {
	DailyTokenLimit int
	tokensUsed      map[string]int
	lastReset       map[string]time.Time
}

func NewMemoryLimiter(limit int) *MemoryLimiter {
	return &MemoryLimiter{DailyTokenLimit: limit, tokensUsed: make(map[string]int), lastReset: make(map[string]time.Time)}
}

func emitBillingOverQuota(prefix string, limit, used int64) {
	if webhook.Global != nil {
		webhook.Global.Emit("billing.over_quota", map[string]any{
			"prefix": prefix,
			"limit":  limit,
			"used":   used,
		})
	}
}

func (m *MemoryLimiter) Check(prefix string, promptTokens int) error {
	if m.DailyTokenLimit <= 0 {
		return nil
	}
	now := time.Now()
	if last, ok := m.lastReset[prefix]; !ok || now.Sub(last) >= 24*time.Hour {
		m.tokensUsed[prefix] = 0
		m.lastReset[prefix] = now.Truncate(24 * time.Hour)
	}
	if m.tokensUsed[prefix]+promptTokens > m.DailyTokenLimit {
		used := int64(m.tokensUsed[prefix] + promptTokens)
		emitBillingOverQuota(prefix, int64(m.DailyTokenLimit), used)
		return fmt.Errorf("over_quota: daily_token_limit %d exceeded", m.DailyTokenLimit)
	}
	return nil
}

func (m *MemoryLimiter) RecordUsage(prefix string, tokens int, _ int, _ time.Time) {
	m.tokensUsed[prefix] += tokens
}

// ---------------------------------------------------------------------------
// Typed quota errors
// ---------------------------------------------------------------------------

// ErrOverQuota marks user-facing quota exhaustion (HTTP 429 semantics).
var ErrOverQuota = errors.New("over_quota")

// QuotaError carries machine-readable context alongside ErrOverQuota.
type QuotaError struct {
	LimitKind string // daily_token | daily_cost | monthly_cost | org_monthly_cost
	Limit     int64
	Used      int64
	Prefix    string
	OrgID     string
}

func (q *QuotaError) Error() string {
	if q.OrgID != "" {
		return fmt.Sprintf("over_quota: %s exceeded for org %s (limit %d, used %d)", q.LimitKind, q.OrgID, q.Limit, q.Used)
	}
	return fmt.Sprintf("over_quota: %s exceeded for key %s (limit %d, used %d)", q.LimitKind, q.Prefix, q.Limit, q.Used)
}

func (q *QuotaError) Unwrap() error { return ErrOverQuota }

// ErrBudgetUnavailable signals the enforcement layer could not read its own
// state (transient store failure). SECURITY/AVAILABILITY POLICY: for keys WITH
// configured limits we fail CLOSED (503) instead of silently allowing spend.
var ErrBudgetUnavailable = errors.New("budget_unavailable")

// IsOverQuota reports whether err is an over_quota error.
func IsOverQuota(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrOverQuota) {
		return true
	}
	// Legacy substring fallback for MemoryLimiter/plain errors.
	return strings.Contains(err.Error(), "over_quota")
}

func WriteOverQuota(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	resp := map[string]interface{}{"error": map[string]interface{}{"message": "over_quota", "type": "over_quota_error"}}
	b, _ := json.Marshal(resp)
	w.Write(b)
}

func WriteBudgetUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte(`{"error":{"message":"budget state unavailable, retry shortly","type":"proxy_error"}}`))
}

// ---------------------------------------------------------------------------
// DB-backed ledger limiter (atomic, O(1) admission checks)
// ---------------------------------------------------------------------------

// countersKey builds the scope column value for key/org rows.
func countersKey(prefix string) string { return "key:" + prefix }
func orgCountersKey(id string) string  { return "org:" + id }

func dayStartUTC(now time.Time) time.Time { return now.UTC().Truncate(24 * time.Hour) }

func monthStartUTC(now time.Time) time.Time {
	n := now.UTC()
	return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// boundString renders a time bound in the storage dialect's own textual form.
// go-sqlite3 persists timestamps as "YYYY-MM-DD HH:MM:SS(.frac)+00:00" while
// driver-parsed DATETIME columns surface as time.Time; lexicographic SQL
// comparisons only work against the matching shape. (RFC3339 with its 'T'
// separator compares GREATER than every space-form value and silently
// excludes history — the exact trap the mixed-dialect codebase must avoid.)
func boundString(start time.Time) string {
	if db.Dialect() == "postgres" {
		return start.Format(time.RFC3339Nano)
	}
	return start.UTC().Format("2006-01-02 15:04:05.999999999+00:00")
}

// DBLimiter enforces quotas against the atomic spend_counters ledger.
//
// The old implementation re-aggregated SUM(cost) across ALL of request_logs on
// every single request — O(rows-in-window) growth making admission cost grow
// forever and N concurrent requests sharing one stale snapshot (unbounded
// overspend bursts). Counters are updated atomically by RecordUsage and
// seeded once from history at construction.
type DBLimiter struct {
	DB *sql.DB
}

func NewDBLimiter(dbh *sql.DB) *DBLimiter {
	d := &DBLimiter{DB: dbh}
	d.backfillFromRequestLogs()
	return d
}

// backfillFromRequestLogs seeds current-period counters from historical rows
// once per boot. INSERT .. ON CONFLICT DO NOTHING keeps live increments made
// before/during seeding authoritative.
func (d *DBLimiter) backfillFromRequestLogs() {
	if d.DB == nil {
		return
	}
	now := time.Now().UTC()
	day := dayStartUTC(now)
	month := monthStartUTC(now)

	seed := func(period string, start time.Time) {
		rows, err := d.DB.Query(`SELECT COALESCE(key_prefix,''), COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_usd),0) FROM request_logs WHERE created_at >= ? GROUP BY key_prefix`, boundString(start))
		if err != nil {
			log.Warn().Err(err).Str("period", period).Msg("budget backfill query failed")
			return
		}
		// Materialize + CLOSE the result set BEFORE any writes: SQLite runs
		// MaxOpenConns(1), so interleaving row iteration with UPSERTs
		// deadlocks the sole connection.
		type agg struct {
			prefix string
			tokens int64
			cost   float64
		}
		var aggs []agg
		for rows.Next() {
			var a agg
			if err := rows.Scan(&a.prefix, &a.tokens, &a.cost); err == nil && a.prefix != "" {
				aggs = append(aggs, a)
			}
		}
		closeErr := rows.Close()
		rowErr := rows.Err()
		if closeErr != nil || rowErr != nil {
			log.Warn().Err(firstErr(closeErr, rowErr)).Str("period", period).Msg("budget backfill scan failed")
			return
		}
		for _, a := range aggs {
			d.seedLocked(countersKey(a.prefix), period, start, a.tokens, usdToMicros(a.cost))
		}
	}
	seed("day", day)
	seed("month", month)
}

// usdToMicros converts a float USD amount into integer micro-USD (1e-6).
func usdToMicros(usd float64) int64 {
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 {
		return 0
	}
	m := math.Round(usd * 1e6)
	if m > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(m)
}

// accumulateTail is the ON CONFLICT clause for live usage deltas: on a
// conflicting (scope,period,start_utc) row the incoming delta is ADDED to the
// running totals. It deliberately does not use db.UpsertEnd, which renders
// `SET col=excluded.col` replace semantics — replacing the window total with
// each request's own delta zeroed out all prior spend after the first write,
// so admission checks under-counted and quotas never tripped.
// SQLite ≥3.24 and Postgres share this syntax (via db.Q for placeholders only).
const accumulateTail = ` ON CONFLICT(scope,period,start_utc) DO UPDATE SET` + `
	tokens = tokens + excluded.tokens,` + `
	cost_micros = cost_micros + excluded.cost_micros,` + `
	updated_at = excluded.updated_at`

// recordLocked atomically accumulates into one (scope, period) row using an
// UPSERT that adds the delta to any existing totals.
func (d *DBLimiter) recordLocked(scope, period string, start time.Time, tokensDelta int64, costMicrosDelta int64) error {
	_, err := d.DB.Exec(
		db.Q(`INSERT INTO spend_counters(scope,period,start_utc,tokens,cost_micros,updated_at) VALUES(?,?,?,?,?,?)`)+accumulateTail,
		scope, period, start.Format(time.RFC3339Nano), tokensDelta, costMicrosDelta, time.Now().UTC())
	return err
}

// seedLocked seeds a counter row from authoritative history exactly once:
// an existing row already reflects all live increments, so re-seeding would
// double-count. Idempotent via DO NOTHING.
func (d *DBLimiter) seedLocked(scope, period string, start time.Time, tokens, costMicros int64) error {
	_, err := d.DB.Exec(
		db.Q(`INSERT INTO spend_counters(scope,period,start_utc,tokens,cost_micros,updated_at) VALUES(?,?,?,?,?,?)`)+
			` ON CONFLICT(scope,period,start_utc) DO NOTHING`,
		scope, period, start.Format(time.RFC3339Nano), tokens, costMicros, time.Now().UTC())
	return err
}

// snapshot returns accumulated tokens/micros for a scope+period; ok=false when
// no row exists yet.
func (d *DBLimiter) snapshot(scope, period string, start time.Time) (int64, int64, bool, error) {
	var tokens, micros sql.NullInt64
	err := d.DB.QueryRow(db.Q(`SELECT COALESCE(tokens,0), COALESCE(cost_micros,0) FROM spend_counters WHERE scope=? AND period=? AND start_utc=?`),
		scope, period, start.Format(time.RFC3339Nano)).Scan(&tokens, &micros)
	if err == sql.ErrNoRows {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return tokens.Int64, micros.Int64, true, nil
}

type keyLimits struct {
	orgID        string
	dailyTokens  int64
	dailyCostC   int64
	monthlyCostC int64
	orgMonthlyC  int64
}

func (d *DBLimiter) loadLimits(prefix string) (*keyLimits, error) {
	var k keyLimits
	var orgID sql.NullString
	var dt, dc, mc, omc sql.NullInt64
	err := d.DB.QueryRow(db.Q(`SELECT COALESCE(org_id,''), daily_token_limit, daily_cost_limit_cents, monthly_cost_limit_cents FROM gateway_keys WHERE prefix=? AND revoked_at IS NULL LIMIT 1`), prefix).
		Scan(&orgID, &dt, &dc, &mc)
	if err != nil {
		return nil, err // unknown prefix or store failure — caller decides policy
	}
	k.orgID = orgID.String
	if dt.Valid {
		k.dailyTokens = dt.Int64
	}
	if dc.Valid {
		k.dailyCostC = dc.Int64
	}
	if mc.Valid {
		k.monthlyCostC = mc.Int64
	}
	if k.orgID != "" {
		_ = d.DB.QueryRow(db.Q(`SELECT monthly_cost_limit_cents FROM organizations WHERE id=?`), k.orgID).Scan(&omc)
		if omc.Valid {
			k.orgMonthlyC = omc.Int64
		}
	}
	return &k, nil
}

// usedToday/usedThisMonth read atomic counter snapshots. Returns (tokensUsed,
// centsUsed, unavailableErr).
func (d *DBLimiter) usageFor(scope, kind string, start time.Time) (int64, int64, error) {
	tok, micros, _, err := d.snapshot(scope, kind, start)
	if err != nil {
		return 0, 0, err
	}
	return tok, microToCents(micros), nil
}

func microToCents(m int64) int64 {
	// micros are micro-USD (1e-6); cents are 1e-2 USD.
	return m / 10000
}

// Check implements Limiter.Check against atomic counters. Fail-closed on
// store errors for known keys; unknown prefixes pass through (they fail
// auth upstream anyway).
func (d *DBLimiter) Check(prefix string, promptTokens int) error {
	if d.DB == nil || prefix == "" {
		return nil
	}
	k, lerr := d.loadLimits(prefix)
	if lerr != nil {
		if errors.Is(lerr, sql.ErrNoRows) {
			return nil // unknown/revoked prefix: let authN/Z decide
		}
		// Known-shaped key row unreadable → conservative deny.
		return fmt.Errorf("%w: could not load key limits: %v", ErrBudgetUnavailable, lerr)
	}
	now := time.Now().UTC()
	day := dayStartUTC(now)
	month := monthStartUTC(now)

	dayTok, dayCents, uerr := d.usageFor(countersKey(prefix), "day", day)
	if uerr != nil {
		if hasAnyLimits(k) {
			return fmt.Errorf("%w: daily snapshot read failed: %v", ErrBudgetUnavailable, uerr)
		}
		return nil
	}
	_, monthCents, uerr := d.usageFor(countersKey(prefix), "month", month)
	if uerr != nil && hasAnyLimits(k) {
		return fmt.Errorf("%w: monthly snapshot read failed: %v", ErrBudgetUnavailable, uerr)
	}

	if k.dailyTokens > 0 && dayTok+int64(promptTokens) > k.dailyTokens {
		emitBillingOverQuota(prefix, k.dailyTokens, dayTok+int64(promptTokens))
		return &QuotaError{LimitKind: "daily_token", Limit: k.dailyTokens, Used: dayTok, Prefix: prefix}
	}
	if k.dailyCostC > 0 && dayCents > k.dailyCostC {
		emitBillingOverQuota(prefix, k.dailyCostC, dayCents)
		return &QuotaError{LimitKind: "daily_cost", Limit: k.dailyCostC, Used: dayCents, Prefix: prefix}
	}
	if k.monthlyCostC > 0 && monthCents > k.monthlyCostC {
		emitBillingOverQuota(prefix, k.monthlyCostC, monthCents)
		return &QuotaError{LimitKind: "monthly_cost", Limit: k.monthlyCostC, Used: monthCents, Prefix: prefix}
	}
	if k.orgMonthlyC > 0 && k.orgID != "" {
		_, orgMonthCents, oerr := d.usageFor(orgCountersKey(k.orgID), "month", month)
		if oerr != nil {
			return fmt.Errorf("%w: org snapshot read failed: %v", ErrBudgetUnavailable, oerr)
		}
		if orgMonthCents > k.orgMonthlyC {
			emitBillingOverQuota(k.orgID, k.orgMonthlyC, orgMonthCents)
			return &QuotaError{LimitKind: "org_monthly_cost", Limit: k.orgMonthlyC, Used: orgMonthCents, OrgID: k.orgID}
		}
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func hasAnyLimits(k *keyLimits) bool {
	return k.dailyTokens > 0 || k.dailyCostC > 0 || k.monthlyCostC > 0 || k.orgMonthlyC > 0
}

// RecordUsage atomically accumulates real post-response usage into BOTH the
// daily and monthly windows for the key scope.
func (d *DBLimiter) RecordUsage(prefix string, tokens int, costCents int, at time.Time) {
	if d.DB == nil || prefix == "" {
		return
	}
	now := at.UTC()
	t := int64(tokens)
	c := centsToMicros(costCents)
	recordWindow(d, countersKey(prefix), "day", dayStartUTC(now), t, c)
	recordWindow(d, countersKey(prefix), "month", monthStartUTC(now), t, c)
}

// RecordOrgUsage mirrors usage onto the org-scoped counters powering
// organization-level budgets.
func (d *DBLimiter) RecordOrgUsage(orgID string, tokens int, costCents int, at time.Time) {
	if d.DB == nil || orgID == "" {
		return
	}
	now := at.UTC()
	t := int64(tokens)
	c := centsToMicros(costCents)
	recordWindow(d, orgCountersKey(orgID), "day", dayStartUTC(now), t, c)
	recordWindow(d, orgCountersKey(orgID), "month", monthStartUTC(now), t, c)
}

func centsToMicros(cents int) int64 {
	if cents < 0 {
		return 0
	}
	c := int64(cents) * 10000 // cents (1e-2 USD) -> micro-USD (1e-6 USD)
	return c
}

func recordWindow(d *DBLimiter, scope, period string, start time.Time, tokens, costMicros int64) {
	if err := d.recordLocked(scope, period, start, tokens, costMicros); err != nil {
		// Non-fatal: admission check uses potentially-stale snapshot next
		// round; log loudly so drift is visible.
		log.Error().Err(err).Str("scope", scope).Str("period", period).Msg("spend counter update failed")
	}
}

// Middleware returns a chi-compatible middleware that enforces DB quotas.
// Fail modes:
//   - over_quota        → 429 over_quota_error envelope + webhook
//   - budget_unavailable→ 503 (fail closed for protected keys)
//   - other             → allowed (unknown-prefix passthrough handled upstream)
func Middleware(l Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			prefix := r.Header.Get("X-Gateway-Key-Prefix")
			if prefix == "" {
				// fallback: try Authorization header (in case GatewayAuth not yet set)
				auth := r.Header.Get("Authorization")
				if strings.HasPrefix(auth, "Bearer sk-gw-") {
					tok := strings.TrimPrefix(auth, "Bearer ")
					if len(tok) >= len("sk-gw-")+8 {
						prefix = tok[len("sk-gw-") : len("sk-gw-")+8]
					}
				}
				if prefix == "" {
					auth = r.Header.Get("x-api-key")
					if strings.HasPrefix(auth, "sk-gw-") && len(auth) >= len("sk-gw-")+8 {
						prefix = auth[len("sk-gw-") : len("sk-gw-")+8]
					}
				}
			}
			if prefix != "" {
				// Pre-flight token estimate from the declared body size (~4
				// bytes per token). Real usage is still recorded post-response;
				// this estimate only stops a single very large request from
				// blowing past a nearly-exhausted daily token budget unchecked.
				estimate := 0
				if r.ContentLength > 0 {
					estimate = int(r.ContentLength / 4)
				}
				err := l.Check(prefix, estimate)
				switch {
				case IsOverQuota(err):
					var quota *QuotaError
					if errors.As(err, &quota) {
						webhook.Global.Emit("billing.over_quota", map[string]any{
							"prefix": quota.Prefix, "org_id": quota.OrgID,
							"limit_kind": quota.LimitKind, "limit": quota.Limit, "used": quota.Used,
						})
					} else if webhook.Global != nil {
						webhook.Global.Emit("billing.over_quota", map[string]any{"prefix": prefix, "error": err.Error()})
					}
					WriteOverQuota(w)
					return
				case errors.Is(err, ErrBudgetUnavailable):
					WriteBudgetUnavailable(w)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// BudgetCheck is an alias for Middleware for handler path wiring.
func BudgetCheck(l Limiter) func(http.Handler) http.Handler { return Middleware(l) }

// UsageSink adapts DBLimiter to proxy.UsageSink (structural): it records real
// post-response token/cost outcomes for key scope and, when known, org scope.
type UsageSink struct {
	Limiter *DBLimiter
}

func (u *UsageSink) RecordUsage(keyPrefix string, orgID *string, tokens int, costUSD float64, ts time.Time) {
	if u == nil || u.Limiter == nil || keyPrefix == "" {
		return
	}
	cents := int(math.Round(costUSD * 100))
	u.Limiter.RecordUsage(keyPrefix, tokens, cents, ts)
	if orgID != nil && *orgID != "" {
		u.Limiter.RecordOrgUsage(*orgID, tokens, cents, ts)
	}
}

var _ Limiter = (*DBLimiter)(nil)
var _ Limiter = (*MemoryLimiter)(nil)
