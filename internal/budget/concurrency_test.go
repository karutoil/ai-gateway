package budget

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
)

// TestDBLimiterConcurrentCheckRecordSpray drives the SHIPPED request-path
// admission path (DBLimiter.Check/RecordUsage against one real SQLite ledger)
// from many goroutines at once. Assertions are aggregate invariants only —
// contention itself is handed to the race detector via `-race`.
//
// Invariants checked:
//  1. every attempt resolves to exactly one of {admitted, over_quota} — never
//     an unexpected error or a silent double-count;
//  2. tokens recorded equal admitted attempts × batch size;
//  3. the spend_counters ledger sums to exactly what was recorded;
//  4. overshoot stays within the theoretical check-then-act bound:
//     limit + (workers-1) × promptTokens (concurrent checks may all admit
//     against near-full headroom before any of their records land).
func TestDBLimiterConcurrentCheckRecordSpray(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "spray.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	ks := apikey.NewStore(database)
	k, err := ks.Create("spray")
	if err != nil {
		t.Fatal(err)
	}
	const dailyLimit = 1000
	if _, err := database.Exec(`UPDATE gateway_keys SET daily_token_limit=? WHERE prefix=?`, dailyLimit, k.Prefix); err != nil {
		t.Fatal(err)
	}
	lim := NewDBLimiter(database)

	const (
		workers      = 16
		attemptsEach = 25
		promptTokens = 10
	)
	// Total supply = 4000 tokens ≫ limit, so the spray is guaranteed to cross
	// the quota and exercise both admit and deny outcomes.
	var admitted, denied atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < attemptsEach; i++ {
				err := lim.Check(k.Prefix, promptTokens)
				switch {
				case err == nil:
					admitted.Add(1)
					lim.RecordUsage(k.Prefix, promptTokens, 1, time.Now())
				case IsOverQuota(err):
					denied.Add(1)
				default:
					t.Errorf("unexpected Check outcome: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got := admitted.Load() + denied.Load(); got != workers*attemptsEach {
		t.Fatalf("attempts must resolve to admit/deny exactly once: %d+%d != %d", admitted.Load(), denied.Load(), workers*attemptsEach)
	}
	if denied.Load() == 0 || admitted.Load() == 0 {
		t.Fatalf("spray must cross the quota boundary: admitted=%d denied=%d", admitted.Load(), denied.Load())
	}

	wantTokens := admitted.Load() * promptTokens
	var ledgerTokens int64
	if err := database.QueryRow(
		`SELECT COALESCE(SUM(tokens),0) FROM spend_counters WHERE scope=? AND period='day'`,
		countersKey(k.Prefix),
	).Scan(&ledgerTokens); err != nil {
		t.Fatal(err)
	}
	if ledgerTokens != wantTokens {
		t.Fatalf("ledger drift: spend_counters has %d tokens, recorded admissions sum to %d", ledgerTokens, wantTokens)
	}

	maxOvershoot := int64(workers-1) * promptTokens
	if ledgerTokens > dailyLimit+maxOvershoot {
		t.Fatalf("unbounded overspend: %d tokens admitted against limit %d (theoretical max incl. race window: %d)",
			ledgerTokens, dailyLimit, dailyLimit+maxOvershoot)
	}
	t.Logf("admitted=%d denied=%d ledger_tokens=%d (limit %d)", admitted.Load(), denied.Load(), ledgerTokens, dailyLimit)
}

// TestDBLimiterRecordUsageAccumulates pins the counter arithmetic directly:
// repeated RecordUsage calls must ADD UP in both windows — the pre-fix upsert
// replaced the total with each delta, letting keys spend far past their limits.
func TestDBLimiterRecordUsageAccumulates(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "acc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	lim := NewDBLimiter(database) // request_logs empty → no backfill interference
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		lim.RecordUsage("pfx", 10, 2, now.Add(time.Duration(i)*time.Second))
	}

	dayTok, dayMicros, ok, err := lim.snapshot(countersKey("pfx"), "day", dayStartUTC(now))
	if err != nil || !ok {
		t.Fatalf("day window missing: ok=%v err=%v", ok, err)
	}
	if dayTok != 50 || dayMicros != 100000 { // 5 × (10 tokens, 2 cents = 20000 micros)
		t.Fatalf("day counters not accumulated: tokens=%d micros=%d", dayTok, dayMicros)
	}
	moTok, moMicros, _, err := lim.snapshot(countersKey("pfx"), "month", monthStartUTC(now))
	if err != nil || !ok {
		t.Fatalf("month window missing: ok=%v err=%v", ok, err)
	}
	if moTok != 50 || moMicros != 100000 {
		t.Fatalf("month counters not accumulated: tokens=%d micros=%d", moTok, moMicros)
	}
}
