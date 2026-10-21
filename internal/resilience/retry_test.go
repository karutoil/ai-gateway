package resilience

import (
	"testing"
	"time"
)

// NonRetryable is the mechanism behind P0-2: billable, non-idempotent creates
// must never be retried, not even on 429 (a metered provider that rate-limits
// after generating has already charged).
func TestNonRetryableNeverRetries(t *testing.T) {
	inner := NewDefaultRetryPolicy()
	p := NoRetry(inner)
	if p == nil {
		t.Fatal("NoRetry must not return nil for a non-nil policy")
	}
	for _, status := range []int{0, 429, 500, 502, 503, 504, 408} {
		for attempt := 0; attempt < 5; attempt++ {
			if p.ShouldRetry(attempt, status) {
				t.Fatalf("NonRetryable.ShouldRetry(%d, %d) = true, want false", attempt, status)
			}
		}
	}
	// Backoff still delegates, so a caller that inspects it sees the inner
	// policy's schedule rather than a zero delay.
	if got := p.Backoff(1); got < 100*time.Millisecond {
		t.Fatalf("Backoff(1) = %v, want the inner policy's delay", got)
	}
}

// NoRetry is idempotent: wrapping twice does not change the decision.
func TestNoRetryIdempotentAndNilSafe(t *testing.T) {
	if got := NoRetry(nil); got != nil {
		t.Fatalf("NoRetry(nil) = %v, want nil (composes with a nil Retry field)", got)
	}
	once := NoRetry(NewDefaultRetryPolicy())
	twice := NoRetry(once)
	if once != twice {
		t.Fatal("NoRetry of an already-non-retrying policy must return it unchanged")
	}
}

// TestStatusFamily pins the metric label folding (bounded cardinality).
func TestStatusFamily(t *testing.T) {
	cases := map[int]string{0: "transport", 429: "429", 500: "5xx", 503: "5xx", 400: "4xx", 200: "other"}
	for status, want := range cases {
		if got := statusFamily(status); got != want {
			t.Errorf("statusFamily(%d) = %q, want %q", status, got, want)
		}
	}
}

// IncBillableRetrySkip must fold unknown endpoints so a caller cannot mint
// unbounded label values. It must not panic on the shared registry.
func TestIncBillableRetrySkipFoldsUnknownEndpoints(t *testing.T) {
	IncBillableRetrySkip("images.generations", 500)
	IncBillableRetrySkip("images.edits", 429)
	IncBillableRetrySkip("totally-new-endpoint", 500)
	IncBillableRetrySkip("", 0)
}
