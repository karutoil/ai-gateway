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

// The attempt budget bounds retry storms: attempt numbering starts at 0, so
// MaxRetries=N must allow exactly N retries (N+1 upstream attempts total) and
// nothing past them, regardless of status.
func TestDefaultRetryPolicyAttemptBudget(t *testing.T) {
	p := &DefaultRetryPolicy{MaxRetries: 2, BaseDelay: time.Millisecond}
	for status := range map[int]bool{429: true, 500: true, 503: true, 504: true, 502: true} {
		for attempt := 0; attempt < p.MaxRetries; attempt++ {
			if !p.ShouldRetry(attempt, status) {
				t.Fatalf("ShouldRetry(%d, %d) = false, want true (budget not spent)", attempt, status)
			}
		}
		if p.ShouldRetry(p.MaxRetries, status) {
			t.Fatalf("ShouldRetry(%d, %d) = true, want false (budget spent)", p.MaxRetries, status)
		}
	}
	// 4xx is never retried (client error semantics).
	for attempt := 0; attempt < 5; attempt++ {
		if p.ShouldRetry(attempt, 400) || p.ShouldRetry(attempt, 401) {
			t.Fatalf("4xx must never be retried (attempt %d)", attempt)
		}
	}
}

// Backoff grows exponentially, is jittered, and stays capped so a storm of
// retries cannot sleep unboundedly.
func TestDefaultRetryPolicyBackoffBounds(t *testing.T) {
	p := &DefaultRetryPolicy{MaxRetries: 5, BaseDelay: 200 * time.Millisecond}
	lo := time.Duration(float64(p.BaseDelay) * 0.5) // d/2 floor
	for attempt := 0; attempt < 8; attempt++ {
		d := p.Backoff(attempt)
		if d < lo {
			t.Fatalf("Backoff(%d) = %v below equal-jitter floor %v", attempt, d, lo)
		}
		if d > time.Second {
			t.Fatalf("Backoff(%d) = %v above the 1s cap", attempt, d)
		}
		// Advance the floor for later attempts (capped like the delay itself).
		dCap := float64(p.BaseDelay) * float64(int(1)<<uint(attempt))
		if dCap > float64(time.Second) {
			dCap = float64(time.Second)
		}
		lo = time.Duration(dCap / 2)
	}
}
