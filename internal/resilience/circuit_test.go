package resilience

import (
	"testing"
	"time"
)

// The breaker must never wedge permanently: a probe slot consumed by a
// request that exits without Record (newUpstreamRequest failure,
// mid-stream-death+client-gone, a recovered panic) must be reclaimed, and
// the circuit must close after closeAfter consecutive successful probes.
func TestBreakerClosesAfterConsecutiveSuccessfulProbes(t *testing.T) {
	cb := NewMemoryCircuitBreakerFull(5, time.Minute, 30*time.Second, 2)
	id := "prov"
	for i := 0; i < 5; i++ {
		cb.Record(id, 503)
	}
	if got := cb.State(id); got != "open" {
		t.Fatalf("state = %q, want open", got)
	}
	// Simulate cooldown elapsed.
	cb.mu.Lock()
	cb.states[id].openUntil = cb.states[id].openUntil.Add(-2 * defaultProbeReclaimAfter)
	cb.mu.Unlock()

	if !cb.Allow(id) {
		t.Fatal("first probe denied")
	}
	cb.Record(id, 200)
	if got := cb.State(id); got == "closed" {
		t.Fatal("closed after a single successful probe (closeAfter=2)")
	}
	if !cb.Allow(id) {
		t.Fatal("second probe denied while half-open with a free slot")
	}
	cb.Record(id, 200)
	if got := cb.State(id); got != "closed" {
		t.Fatalf("state = %q after two clean probes, want closed", got)
	}
	if !cb.Allow(id) {
		t.Fatal("closed circuit denying requests")
	}
}

// Regression for the live incident: a leaked probe slot (Allow without any
// Record, ever) used to deny every future request forever. The slot must be
// reclaimed after defaultProbeReclaimAfter.
func TestBreakerReclaimsStuckProbe(t *testing.T) {
	cb := NewMemoryCircuitBreakerFull(5, time.Minute, 30*time.Second, 2)
	id := "prov"
	for i := 0; i < 5; i++ {
		cb.Record(id, 503)
	}
	cb.mu.Lock()
	cb.states[id].openUntil = cb.states[id].openUntil.Add(-2 * defaultProbeReclaimAfter)
	cb.mu.Unlock()

	if !cb.Allow(id) {
		t.Fatal("probe admission denied")
	}
	// Simulate the leak: the request exits WITHOUT Record, and time advances
	// past the reclaim deadline.
	cb.mu.Lock()
	cb.states[id].probeStarted = cb.states[id].probeStarted.Add(-2 * defaultProbeReclaimAfter)
	cb.mu.Unlock()

	if !cb.Allow(id) {
		t.Fatal("stuck probe slot not reclaimed — circuit wedged open forever")
	}
	// The reclaimed slot behaves like a fresh admission.
	cb.Record(id, 200)
	if cb.Allow(id) {
		cb.Record(id, 200)
	}
	if got := cb.State(id); got != "closed" {
		t.Fatalf("state = %q after reclaimed probes succeed, want closed", got)
	}
}

// A young stuck slot (within the deadline) must still be denied — reclaim
// must not turn maxProbes into infinity.
func TestBreakerYoungStuckSlotStillDenied(t *testing.T) {
	cb := NewMemoryCircuitBreakerFull(5, time.Minute, 30*time.Second, 2)
	id := "prov"
	for i := 0; i < 5; i++ {
		cb.Record(id, 503)
	}
	cb.mu.Lock()
	cb.states[id].openUntil = cb.states[id].openUntil.Add(-2 * defaultProbeReclaimAfter)
	cb.mu.Unlock()

	if !cb.Allow(id) {
		t.Fatal("probe admission denied")
	}
	if cb.Allow(id) {
		t.Fatal("second concurrent probe admitted despite maxProbes=1")
	}
}

// Release returns a slot without classifying; Release on a closed circuit is
// a no-op and must not corrupt state.
func TestReleaseSemantics(t *testing.T) {
	cb := NewMemoryCircuitBreakerFull(5, time.Minute, 30*time.Second, 2)
	id := "prov"
	cb.Release(id) // closed circuit: no-op
	if got := cb.State(id); got != "closed" {
		t.Fatalf("state = %q after no-op Release", got)
	}
	for i := 0; i < 5; i++ {
		cb.Record(id, 503)
	}
	cb.mu.Lock()
	cb.states[id].openUntil = cb.states[id].openUntil.Add(-2 * defaultProbeReclaimAfter)
	cb.mu.Unlock()
	if !cb.Allow(id) {
		t.Fatal("probe admission denied")
	}
	cb.Release(id)
	// Slot returned: the next admission is a fresh probe.
	if !cb.Allow(id) {
		t.Fatal("slot not returned by Release")
	}
	cb.Record(id, 503) // probe failed → reopen
	if got := cb.State(id); got != "open" {
		t.Fatalf("state = %q after failed probe, want open", got)
	}
}
