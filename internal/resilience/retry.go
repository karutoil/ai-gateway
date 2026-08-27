package resilience

import (
	"math"
	"time"
)

// RetryPolicy is the Phase 1.6 scaffold for Phase 2 retries.
// 1.6 ships DefaultRetryPolicy but it is not wired to proxy — Phase 2 wires it.
type RetryPolicy interface {
	ShouldRetry(attempt int, status int) bool
	Backoff(attempt int) time.Duration
}

type DefaultRetryPolicy struct {
	MaxRetries int
	BaseDelay  time.Duration
}

func NewDefaultRetryPolicy() *DefaultRetryPolicy {
	return &DefaultRetryPolicy{MaxRetries: 2, BaseDelay: 200 * time.Millisecond}
}

func (p *DefaultRetryPolicy) ShouldRetry(attempt int, status int) bool {
	if attempt >= p.MaxRetries {
		return false
	}
	// retry only 5xx and 429/502; never 4xx (relies on unified error envelope from 1.6)
	if status == 429 || status == 502 || status == 503 || status == 504 {
		return true
	}
	if status >= 500 && status <= 599 {
		return true
	}
	return false
}

func (p *DefaultRetryPolicy) Backoff(attempt int) time.Duration {
	d := float64(p.BaseDelay) * math.Pow(2, float64(attempt))
	if d > float64(time.Second) {
		d = float64(time.Second)
	}
	return time.Duration(d)
}

var _ RetryPolicy = (*DefaultRetryPolicy)(nil)
