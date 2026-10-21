package resilience

import (
	"crypto/rand"
	"encoding/binary"
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
	// Equal jitter: sleep d/2 + [0, d/2] so concurrent replicas do not
	// retry in lockstep and amplify upstream 429/5xx episodes.
	half := d / 2
	var jitter float64
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		jitter = float64(binary.LittleEndian.Uint64(b[:])) / float64(^uint64(0)) * half
	}
	return time.Duration(half + jitter)
}

var _ RetryPolicy = (*DefaultRetryPolicy)(nil)

// NonRetryable wraps a policy so that it never retries anything.
//
// It exists for billable, NON-IDEMPOTENT creates — /v1/images/generations and
// /v1/images/edits (see internal/proxy/images.go) — where the provider charges
// per generated asset and a second attempt after an ambiguous 5xx/timeout
// generates (and bills) a second asset for one client request. The safest
// rule is the strictest one: never retry, not even a 429, because an upstream
// that rate-limited AFTER generation on a metered provider has already
// incurred the charge. Callers that can prove a request never reached
// generation should implement their own guard instead of using this wrapper.
//
// A nil policy is returned as nil so it composes with a nil Retry field, and a
// nil argument is also nil (a nil Retry field already means "use the
// default", so wrapping is only ever done on a resolved policy).
type NonRetryable struct{ Inner RetryPolicy }

func (p *NonRetryable) ShouldRetry(int, int) bool { return false }

func (p *NonRetryable) Backoff(attempt int) time.Duration {
	if p == nil || p.Inner == nil {
		return 0
	}
	return p.Inner.Backoff(attempt)
}

var _ RetryPolicy = (*NonRetryable)(nil)

// NoRetry returns a policy that never retries: inner when inner is already
// non-retrying is returned unchanged so callers can compose freely.
func NoRetry(inner RetryPolicy) RetryPolicy {
	if inner == nil {
		return nil
	}
	if _, ok := inner.(*NonRetryable); ok {
		return inner
	}
	return &NonRetryable{Inner: inner}
}
