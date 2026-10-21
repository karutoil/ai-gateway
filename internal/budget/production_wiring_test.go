package budget

// Tests for the production wiring of MemoryLimiter via Chained: the
// GLOBAL_DAILY_TOKEN_LIMIT admission stage composed in front of the DB
// ledger in cmd/gateway/main.go. MemoryLimiter previously had zero
// production callers; these tests prove the exact pipeline main.go builds:
//
//	admit  = budget.Chained{memoryLimiter, dbLimiter}   (budget.Middleware)
//	usage  = budget.UsageSink{Limiter: db, Extra: mem}  (proxy usage recording)
//
// The DB stage is exercised through DBLimiter with a nil DB (its documented
// pass-through mode), which keeps these tests store-free while still proving
// the composition semantics: fail-closed admission + usage fan-out.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestChainedAdmissionFailClosed: any member's over-quota verdict denies —
// admission must fail CLOSED on the limit, not pass because the DB stage
// knows nothing about the in-memory cap.
func TestChainedAdmissionFailClosed(t *testing.T) {
	mem := NewMemoryLimiter(100)
	db := &DBLimiter{} // nil DB: documented pass-through stage
	chain := Chained{mem, db}

	mem.RecordUsage("pfx", 100, 0, time.Now())

	if err := chain.Check("pfx", 1); !IsOverQuota(err) {
		t.Fatalf("expected chained admission to fail closed on the memory cap, got %v", err)
	}
	// Another key is unaffected until usage is recorded for it too.
	if err := chain.Check("other", 1); err != nil {
		t.Fatalf("unrelated key must pass, got %v", err)
	}
	// Below the cap: admitted.
	if err := chain.Check("pfx2", 100); err != nil {
		t.Fatalf("key at exactly the cap boundary minus estimate must pass, got %v", err)
	}
}

// TestChainedRecordUsageFanOut: RecordUsage feeds every member, so the
// in-memory stage sees real post-response usage (a frozen counter would
// never trip the cap).
func TestChainedRecordUsageFanOut(t *testing.T) {
	mem := NewMemoryLimiter(10)
	db := &DBLimiter{}
	chain := Chained{mem, db}

	chain.RecordUsage("pfx", 10, 0, time.Now())
	if err := mem.Check("pfx", 1); !IsOverQuota(err) {
		t.Fatalf("usage recorded through Chained must reach the memory stage, got %v", err)
	}
}

// TestUsageSinkRecordsIntoExtra: the proxy usage sink (the production
// recording path) feeds the DB ledger AND the in-memory stage.
func TestUsageSinkRecordsIntoExtra(t *testing.T) {
	mem := NewMemoryLimiter(10)
	db := &DBLimiter{}
	sink := &UsageSink{Limiter: db, Extra: mem}

	org := "org1"
	sink.RecordUsage("pfx", &org, 10, 0.10, time.Now())

	if err := mem.Check("pfx", 1); !IsOverQuota(err) {
		t.Fatalf("UsageSink must record into Extra stage, got %v", err)
	}
}

// TestMiddlewareGlobalTokenCapEndToEnd drives budget.Middleware (the
// production admission point wired on the /v1 chain) with the Chained
// pipeline and proves the client-facing behavior: over the in-memory cap →
// 429 over_quota envelope; under it → request reaches the handler.
func TestMiddlewareGlobalTokenCapEndToEnd(t *testing.T) {
	mem := NewMemoryLimiter(100)
	chain := Chained{mem, &DBLimiter{}}

	var reached bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })
	mw := Middleware(chain)(next)

	do := func(prefix string, contentLength int64) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("X-Gateway-Key-Prefix", prefix)
		req.ContentLength = contentLength
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, req)
		return w
	}

	// Record 100 tokens of real usage for "pfx", exactly exhausting the cap.
	mem.RecordUsage("pfx", 100, 0, time.Now())

	// Admission with a non-trivial body estimate: over the cap → 429, and
	// the request never reaches the handler.
	wOver := do("pfx", 16)
	if wOver.Code != http.StatusTooManyRequests || reached {
		t.Fatalf("over-cap request: code=%d reached=%v, want 429/false", wOver.Code, reached)
	}
	if want := `"message":"over_quota"`; !strings.Contains(wOver.Body.String(), want) {
		t.Fatalf("over-cap body = %q, want over_quota envelope", wOver.Body.String())
	}

	// A different key is admitted (no usage recorded, no body estimate)
	// and reaches the handler.
	if w := do("other", 0); w.Code != http.StatusOK || !reached {
		t.Fatalf("other key: code=%d reached=%v, want 200/true", w.Code, reached)
	}
}
