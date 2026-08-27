package resilience

import (
	"sync"
	"testing"
	"time"
)

// waitForState polls the shipped State() API until it reports want, failing
// the test if the transition does not happen within `within`. Cooldown-based
// transitions are driven by real time (the breaker owns its own clock), so
// tests use short cooldowns and a polling deadline instead of fixed sleeps.
func waitForState(t *testing.T, cb *MemoryCircuitBreaker, id, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if got := cb.State(id); got == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("state %q not reached within %v (last observed %q)", want, within, cb.State(id))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestClosedCircuitAdmitsByDefault exercises the constructor and the healthy
// steady state: fresh providers start closed and admit everything.
func TestClosedCircuitAdmitsByDefault(t *testing.T) {
	cb := NewMemoryCircuitBreakerFull(2, time.Second, 50*time.Millisecond, 1)
	if got := cb.State("p"); got != "closed" {
		t.Fatalf("fresh circuit = %q, want closed", got)
	}
	for i := 0; i < 3; i++ {
		if !cb.Allow("p") {
			t.Fatalf("closed circuit refused admission on call %d", i)
		}
	}
}

// TestThresholdFailuresOpenCircuit covers closed --(threshold failures)--> open:
// failures below threshold must NOT open; reaching them via both HTTP 5xx and
// transport-level signals (status 0) must.
func TestThresholdFailuresOpenCircuit(t *testing.T) {
	cb := NewMemoryCircuitBreakerFull(2, time.Minute, time.Second, 1)

	cb.Record("p", 500)
	if got := cb.State("p"); got != "closed" {
		t.Fatalf("one failure below threshold: state = %q, want closed", got)
	}
	if !cb.Allow("p") {
		t.Fatal("one failure below threshold must still admit")
	}

	cb.Record("p", 0) // transport-level failure counts like a 5xx
	if got := cb.State("p"); got != "open" {
		t.Fatalf("threshold reached: state = %q, want open", got)
	}
	if cb.Allow("p") {
		t.Fatal("open circuit must refuse admission")
	}
}

// Test429NeutralDoesNotTrip covers the documented neutrality rule: quota-noise
// responses neither accumulate failures while closed nor break/reopen the
// circuit during half-open probing.
func Test429NeutralDoesNotTrip(t *testing.T) {
	cb := NewMemoryCircuitBreakerFull(1, time.Minute, 60*time.Millisecond, 1)

	for i := 0; i < 10; i++ {
		cb.Record("p", 429)
	}
	if got := cb.State("p"); got != "closed" {
		t.Fatalf("repeated 429s tripped the circuit: state = %q, want closed", got)
	}
	if !cb.Allow("p") {
		t.Fatal("closed circuit with only 429 history must admit")
	}

	// Open it for real, ride out cooldown, then exercise the half-open probe.
	cb.Record("p", 500)
	waitForState(t, cb, "p", "open", time.Second)
	waitForState(t, cb, "p", "half_open", time.Second)
	if !cb.Allow("p") {
		t.Fatal("half-open must admit one probe")
	}
	cb.Record("p", 429) // neutral probe outcome: no reopen, but no credit either
	if got := cb.State("p"); got == "closed" {
		t.Fatal("a neutral probe must not count as a success toward closing")
	}
	if !cb.Allow("p") {
		t.Fatal("neutral probe freed the slot without progress; another probe must be admitted")
	}
	cb.Record("p", 200) // real success now closes it
	if got := cb.State("p"); got != "closed" {
		t.Fatalf("success after neutral probe: state = %q, want closed", got)
	}
}

// TestCooldownExpiryOpensHalfOpenSingleProbe covers open --(cooldown)-->
// half-open and proves only ONE concurrent probe is admitted while a probe is
// in flight.
func TestCooldownExpiryOpensHalfOpenSingleProbe(t *testing.T) {
	const cooldown = 60 * time.Millisecond
	cb := NewMemoryCircuitBreakerFull(1, time.Minute, cooldown, 2)

	cb.Record("p", 500)
	if got := cb.State("p"); got != "open" {
		t.Fatalf("pre-cooldown state = %q, want open", got)
	}
	if cb.Allow("p") {
		t.Fatal("during cooldown admission must be refused")
	}

	waitForState(t, cb, "p", "half_open", time.Second)

	if !cb.Allow("p") {
		t.Fatal("after cooldown the first probe must be admitted")
	}
	if cb.Allow("p") {
		t.Fatal("second concurrent probe must be refused (single-flight)")
	}
	if got := cb.State("p"); got != "half_open" {
		t.Fatalf("while a probe is in flight state = %q, want half_open", got)
	}
}

// TestRequiredSuccessesCloseCircuit covers half_open --(N successes)--> closed
// with closeAfter=2: one success keeps it probing, two consecutive ones close.
func TestRequiredSuccessesCloseCircuit(t *testing.T) {
	cb := NewMemoryCircuitBreakerFull(1, time.Minute, 40*time.Millisecond, 2)

	cb.Record("p", 503)
	waitForState(t, cb, "p", "half_open", time.Second)

	if !cb.Allow("p") {
		t.Fatal("first probe refused")
	}
	cb.Record("p", 200) // success #1 of 2
	if got := cb.State("p"); got != "half_open" {
		t.Fatalf("one of two required successes: state = %q, want half_open", got)
	}

	if !cb.Allow("p") {
		t.Fatal("probe after first success refused")
	}
	cb.Record("p", 204) // success #2 of 2
	if got := cb.State("p"); got != "closed" {
		t.Fatalf("required successes reached: state = %q, want closed", got)
	}
	if !cb.Allow("p") {
		t.Fatal("re-closed circuit must admit freely again")
	}
}

// TestFailedProbeReopensCircuit covers half_open --(any failure)--> open with
// a FRESH cooldown, then full recovery on the next attempt.
func TestFailedProbeReopensCircuit(t *testing.T) {
	cb := NewMemoryCircuitBreakerFull(1, time.Minute, 40*time.Millisecond, 1)

	cb.Record("p", 500)
	waitForState(t, cb, "p", "half_open", time.Second)
	if !cb.Allow("p") {
		t.Fatal("first probe refused")
	}
	cb.Record("p", 502)
	if got := cb.State("p"); got != "open" {
		t.Fatalf("failed probe: state = %q, want open (immediate reopen)", got)
	}
	if cb.Allow("p") {
		t.Fatal("reopened circuit must refuse until the new cooldown elapses")
	}

	// Full loop back to closed after the fresh cooldown.
	waitForState(t, cb, "p", "half_open", time.Second)
	if !cb.Allow("p") {
		t.Fatal("probe after reopen refused")
	}
	cb.Record("p", 200)
	if got := cb.State("p"); got != "closed" {
		t.Fatalf("post-reopen recovery: state = %q, want closed", got)
	}
}

// TestOldFailuresExpireFromWindow verifies failures outside the sliding window
// cannot combine with fresh ones to trip the threshold.
func TestOldFailuresExpireFromWindow(t *testing.T) {
	cb := NewMemoryCircuitBreakerFull(2, 80*time.Millisecond, time.Second, 1)

	cb.Record("p", 500)
	time.Sleep(150 * time.Millisecond) // first failure ages out of the window
	cb.Record("p", 500)
	if got := cb.State("p"); got != "closed" {
		t.Fatalf("stale + fresh failure must stay below threshold: state = %q", got)
	}
	cb.Record("p", 500)
	if got := cb.State("p"); got != "open" {
		t.Fatalf("two fresh failures must open: state = %q", got)
	}
}

// TestProvidersAreIsolated checks per-provider accounting doesn't bleed across IDs.
func TestProvidersAreIsolated(t *testing.T) {
	cb := NewMemoryCircuitBreakerFull(1, time.Minute, time.Second, 1)
	cb.Record("a", 500)
	if got := cb.State("a"); got != "open" {
		t.Fatalf("provider a = %q, want open", got)
	}
	if got := cb.State("b"); got != "closed" {
		t.Fatalf("provider b = %q, want closed despite provider a being open", got)
	}
	if !cb.Allow("b") {
		t.Fatal("healthy provider b must admit while provider a is open")
	}
}

// TestConcurrentAllowRecordStateSpray races Allow/Record/State from many
// goroutines against ONE shared provider ID. It asserts only aggregate sanity
// (every Record admits/refuses coherently, final state is one of the three
// legal states); its main job is to give the race detector real contention on
// the production shared-state object.
func TestConcurrentAllowRecordStateSpray(t *testing.T) {
	cb := NewMemoryCircuitBreakerFull(3, 100*time.Millisecond, 5*time.Millisecond, 2)

	const workers = 12
	const iters = 200
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(seed int) {
			defer wg.Done()
			statuses := []int{200, 500, 502, 429, 0}
			allowed, refused := 0, 0
			for i := 0; i < iters; i++ {
				switch cb.State("shared") {
				case "closed", "open", "half_open":
				default:
					t.Errorf("illegal state %q reported", cb.State("shared"))
					return
				}
				if cb.Allow("shared") {
					allowed++
					cb.Record("shared", statuses[(seed+i)%len(statuses)])
				} else {
					refused++
				}
			}
			if allowed+refused != iters {
				t.Errorf("allow/refuse bookkeeping mismatch: %d+%d != %d", allowed, refused, iters)
			}
		}(w)
	}
	wg.Wait()

	if got := cb.State("shared"); got != "closed" && got != "open" && got != "half_open" {
		t.Fatalf("final state %q illegal", got)
	}
}
