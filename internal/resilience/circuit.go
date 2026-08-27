package resilience

import (
	"sync"
	"time"
)

// CircuitBreaker tracks per-provider failure state.
//
// State machine (per provider):
//
//	closed ──(threshold failures inside window)──▶ open
//	open ──(cooldown elapsed)────────────────────▶ half_open (single probe)
//	half_open ──(halfOpenSuccesses consecutive successes)──▶ closed
//	half_open ──(any failure)─────────────────────▶ open
//
// A failure is any 5xx or transport-level error signal (status >= 500).
// 429 is deliberately NEUTRAL: one tenant exhausting an org-wide quota must
// not circuit-break the provider for everyone.
type CircuitBreaker interface {
	Allow(providerID string) bool
	Record(providerID string, status int)
	State(providerID string) string // closed | open | half_open
}

type breakerState struct {
	failures      []time.Time
	openUntil     time.Time
	probeInFlight int // half-open: probes currently executing (admission gate)
	probeSuccess  int // consecutive probe successes toward closing
}

type MemoryCircuitBreaker struct {
	mu         sync.Mutex
	states     map[string]*breakerState
	threshold  int
	window     time.Duration
	openFor    time.Duration
	closeAfter int // consecutive half-open successes required to close
	maxProbes  int // concurrent probes allowed during half-open
}

func NewMemoryCircuitBreaker(threshold int, window, openFor time.Duration) *MemoryCircuitBreaker {
	return NewMemoryCircuitBreakerFull(threshold, window, openFor, 2)
}

// NewMemoryCircuitBreakerFull allows tuning every parameter. closeAfter is the
// number of consecutive successful half-open probes required to fully close
// the circuit; values < 1 are promoted to 1. During half-open at most
// maxHalfOpenConcurrentProbes requests reach the provider so recovery is
// probed, not slammed.
func NewMemoryCircuitBreakerFull(threshold int, window, openFor time.Duration, closeAfter int) *MemoryCircuitBreaker {
	if threshold <= 0 {
		threshold = 5
	}
	if window <= 0 {
		window = 60 * time.Second
	}
	if openFor <= 0 {
		openFor = 30 * time.Second
	}
	if closeAfter <= 0 {
		closeAfter = 1
	}
	return &MemoryCircuitBreaker{
		states:     make(map[string]*breakerState),
		threshold:  threshold,
		window:     window,
		openFor:    openFor,
		closeAfter: closeAfter,
		maxProbes:  1,
	}
}

const maxHalfOpenConcurrentProbes = 1

func (c *MemoryCircuitBreaker) state(providerID string) *breakerState {
	s, ok := c.states[providerID]
	if !ok {
		s = &breakerState{}
		c.states[providerID] = s
	}
	return s
}

// isFailure classifies an HTTP status for breaker accounting.
// status == 0 means transport-level failure (connection refused/reset/timeout).
func isFailureStatus(status int) bool {
	if status == 0 {
		return true
	}
	return status >= 500 && status <= 599
}

func (c *MemoryCircuitBreaker) Allow(providerID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.state(providerID)
	now := time.Now()

	switch {
	case s.openUntil.IsZero():
		// Fully closed.
		return true
	case now.Before(s.openUntil):
		// Cooldown active.
		return false
	default:
		// Cooldown elapsed → half-open. Admit bounded single-flight probes.
		if s.probeInFlight >= maxHalfOpenConcurrentProbes {
			return false
		}
		s.probeInFlight++
		return true
	}
}

func (c *MemoryCircuitBreaker) Record(providerID string, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.state(providerID)
	now := time.Now()

	if isFailureStatus(status) {
		s.probeSuccess = 0
		if s.probeInFlight > 0 {
			s.probeInFlight--
			// Probe failed during half-open: reopen immediately.
			s.openUntil = now.Add(c.openFor)
			return
		}
		kept := s.failures[:0]
		for _, t := range s.failures {
			if now.Sub(t) <= c.window {
				kept = append(kept, t)
			}
		}
		kept = append(kept, now)
		s.failures = kept
		if len(s.failures) >= c.threshold {
			s.failures = nil
			s.openUntil = now.Add(c.openFor)
		}
		return
	}

	// Neutral outcome (e.g. 429 quota noise): neither accumulates failures nor
	// advances probe progress.
	if status == 429 {
		if s.probeInFlight > 0 {
			s.probeInFlight--
		}
		return
	}

	// Real success (status < 500).
	if s.probeInFlight > 0 {
		s.probeInFlight--
		s.probeSuccess++
		if s.probeSuccess >= c.closeAfter {
			s.openUntil = time.Time{}
			s.probeSuccess = 0
			s.failures = nil
		}
		return
	}
	if s.openUntil.IsZero() {
		// Healthy steady-state success keeps a trimmed failure history so
		// isolated blinks don't linger forever (but we no longer wipe all
		// mid-window evidence as before).
		kept := s.failures[:0]
		for _, t := range s.failures {
			if now.Sub(t) <= c.window {
				kept = append(kept, t)
			}
		}
		s.failures = kept
	}
}

func (c *MemoryCircuitBreaker) State(providerID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.state(providerID)
	now := time.Now()
	if s.openUntil.IsZero() {
		return "closed"
	}
	if now.Before(s.openUntil) {
		return "open"
	}
	return "half_open"
}

var _ CircuitBreaker = (*MemoryCircuitBreaker)(nil)
