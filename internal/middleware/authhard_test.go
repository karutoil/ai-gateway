package middleware

import (
	"sync"
	"sync/atomic"
	"testing"
)

// Regression for B13/rate-limiter races: concurrent AllowWithLimits under
// -race must never exceed the configured limits and never panic on the map.
func TestRateLimiterConcurrentEnforcement(t *testing.T) {
	rl := NewRateLimiter()
	const limit = 50
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rl.AllowWithLimits("race-key", RateLimits{RPM: limit}) {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if int(allowed.Load()) != limit {
		t.Fatalf("allowed %d, want exactly %d", allowed.Load(), limit)
	}
}

func TestRateLimiterAtomicMultiWindow(t *testing.T) {
	rl := NewRateLimiter()
	// RPH is far larger than RPM: denying by RPM must NOT consume an RPH slot
	// beyond what actually passed. Consume the whole RPM window first.
	for i := 0; i < 5; i++ {
		if !rl.AllowWithLimits("mw", RateLimits{RPM: 5, RPH: 100}) {
			t.Fatalf("attempt %d denied unexpectedly", i)
		}
	}
	if rl.AllowWithLimits("mw", RateLimits{RPM: 5, RPH: 100}) {
		t.Fatal("sixth request must be denied by RPM")
	}
	b, ok := rl.buckets["mw:h"]
	if !ok || b.count != 5 {
		t.Fatalf("RPH bucket should hold exactly 5 after 5 passes (partial-window leak), got %+v ok=%v", b, ok)
	}
}

func TestAuthRateLimiterBruteForce(t *testing.T) {
	l := NewAuthRateLimiter()
	const account = "victim"
	ok := 0
	for i := 0; i < AuthAttemptsPerAccountPerMinute+3; i++ {
		if l.allow("9.9.9.9", account) {
			ok++
		}
	}
	if ok != AuthAttemptsPerAccountPerMinute {
		t.Fatalf("per-account limiter allowed %d, want %d", ok, AuthAttemptsPerAccountPerMinute)
	}
}
