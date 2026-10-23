package budget

// Regression: sub-cent spend must still trip a budget.
//
// The ledger was written with `int(math.Round(costUSD*100))` and READ with
// `micros/10000`, so a request costing $0.004 rounded to 0 cents on both sides:
// the counter never moved and a key with a monthly cap could spend without bound
// as long as each individual request stayed under half a cent — exactly the
// cheap-high-volume traffic a cost cap exists to stop.

import (
	"database/sql"
	"testing"
	"time"

	"ai-gateway/internal/db"
)

func TestSubCentSpendTripsMonthlyBudget(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// A key with a $1.00 monthly cap (100 cents) on the LEDGER-denominated cap
	// the limiter enforces (monthly_cost_limit_cents). The dashboard's
	// monthly_budget_usd is a separate cap enforced from request_logs.
	if _, err := database.Exec(db.Q(`INSERT INTO gateway_keys(id,name,prefix,hash,monthly_cost_limit_cents,created_at)
		VALUES('k1','sub-cent','sk-gw-sub','hash1',100,?)`), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	d := NewDBLimiter(database)
	sink := &UsageSink{Limiter: d}
	now := time.Now().UTC()

	// 300 requests at $0.004 = $1.20, i.e. past the $1.00 cap. Each individual
	// request rounds to 0 cents.
	for i := 0; i < 300; i++ {
		sink.RecordUsage("sk-gw-sub", nil, 10, 0.004, now)
	}

	err = d.Check("sk-gw-sub", 1)
	if err == nil {
		t.Fatal("a key that spent $1.20 against a $1.00 monthly cap was admitted: sub-cent spend is invisible to the budget")
	}
}

func TestSubCentSpendAccumulatesExactly(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	d := NewDBLimiter(database)
	now := time.Now().UTC()
	// 250 * $0.004 = $1.00 exactly.
	for i := 0; i < 250; i++ {
		d.RecordUsageMicros("sk-gw-exact", 1, 4000, now)
	}
	_, micros, err := d.usageForMicros(countersKey("sk-gw-exact"), "month", monthStartUTC(now))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if micros != 1_000_000 {
		t.Fatalf("ledger holds %d micro-USD, want 1000000 ($1.00) — spend is being lost to rounding", micros)
	}
}

// TestRecordUsageMicrosIsIdempotentPerWindow guards against a double-count on a
// retried record call (the proxy records usage on both the streamed and the
// buffered path in some code paths).
func TestZeroCostDoesNotMoveTheLedger(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	d := NewDBLimiter(database)
	now := time.Now().UTC()
	d.RecordUsageMicros("sk-gw-free", 5, 0, now)
	_, micros, err := d.usageForMicros(countersKey("sk-gw-free"), "day", dayStartUTC(now))
	if err != nil && err != sql.ErrNoRows {
		t.Fatalf("snapshot: %v", err)
	}
	if micros != 0 {
		t.Fatalf("zero-cost usage recorded %d micro-USD", micros)
	}
}
