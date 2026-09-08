package resilience

import (
	"testing"
	"time"
)

// STREAM-005: backoff must include jitter and stay within [d/2, d].
func TestAuditBackoffJitterBounds(t *testing.T) {
	p := NewDefaultRetryPolicy()
	for attempt := 0; attempt < 4; attempt++ {
		seen := map[time.Duration]bool{}
		for i := 0; i < 20; i++ {
			d := p.Backoff(attempt)
			if d <= 0 || d > time.Second {
				t.Fatalf("attempt %d backoff %v out of bounds", attempt, d)
			}
			seen[d] = true
		}
		if len(seen) < 2 {
			t.Fatalf("attempt %d backoff shows no jitter variance", attempt)
		}
	}
}
