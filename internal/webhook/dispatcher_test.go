package webhook

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// sinkEvent is one delivered webhook as captured by the test sink.
type sinkEvent struct {
	Event   string
	Header  string
	Payload json.RawMessage
}

// newSink spins up an httptest.Server whose handler delays every delivery by
// `delay` (proving Emit never waits on the sink) and records accepted events
// under a mutex.
func newSink(t *testing.T, delay time.Duration) (*httptest.Server, func() []sinkEvent) {
	t.Helper()
	var mu sync.Mutex
	var got []sinkEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("sink read body: %v", err)
		}
		time.Sleep(delay)
		var ev struct {
			Payload json.RawMessage `json:"payload"`
		}
		_ = json.Unmarshal(body, &ev)
		mu.Lock()
		got = append(got, sinkEvent{Event: r.URL.Path, Header: r.Header.Get("X-Webhook-Event"), Payload: ev.Payload})
		mu.Unlock()
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []sinkEvent {
		mu.Lock()
		defer mu.Unlock()
		out := make([]sinkEvent, len(got))
		copy(out, got)
		return out
	}
}

// waitForCount polls until the sink has seen n events or the deadline passes.
func waitForCount(t *testing.T, snapshot func() []sinkEvent, n int, within time.Duration) []sinkEvent {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		evs := snapshot()
		if len(evs) >= n {
			return evs
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected %d deliveries within %v, got %d", n, within, len(evs))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestEmitNeverBlocksOnSlowSinkAndDelivers drives the real HTTPDispatcher end
// to end against a deliberately slow sink: issuing many Emits must complete in
// a small fraction of the serial delivery cost (async queue, bounded), and
// every queued event must eventually reach the sink via the shipped worker.
func TestEmitNeverBlocksOnSlowSinkAndDelivers(t *testing.T) {
	const n = 8
	const delay = 250 * time.Millisecond // blocking producers would need >= 2s here
	srv, snapshot := newSink(t, delay)

	d := New(srv.URL).(*HTTPDispatcher)
	start := time.Now()
	for i := 0; i < n; i++ {
		d.Emit("audit.test", map[string]any{"seq": i})
	}
	elapsed := time.Since(start)
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("Emit blocked on the slow sink: %d emits took %v (async dispatch must return immediately)", n, elapsed)
	}

	evs := waitForCount(t, snapshot, n, 10*time.Second)
	d.Stop() // queued events are already flowing; stop once drained

	var events []string
	for _, ev := range evs {
		if ev.Header != "audit.test" {
			t.Errorf("delivery carries wrong X-Webhook-Event header: %q", ev.Header)
		}
		var payload struct {
			Seq int `json:"seq"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("payload not the marshaled event payload: %v (%s)", err, ev.Payload)
		}
		events = append(events, strconv.Itoa(payload.Seq))
	}
	sort.Strings(events)
	want := "0 1 2 3 4 5 6 7"
	if got := strings.Join(events, " "); got != want {
		t.Fatalf("single worker must deliver every queued event exactly once: got [%s], want [%s]", got, want)
	}
}

// TestFullQueueDropsInsteadOfBlocking floods a stalled sink past QueueDepth:
// producers must keep returning promptly (drop-oldest overflow policy), and
// after the sink is released the drain still makes progress and Stop joins
// cleanly.
func TestFullQueueDropsInsteadOfBlocking(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var delivered int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		delivered++
		mu.Unlock()
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	d := New(srv.URL).(*HTTPDispatcher)
	const total = QueueDepth*2 + 16 // far more than the bounded queue holds
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < total; i++ {
			d.Emit("audit.flood", map[string]int{"i": i})
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Emit wedged when the webhook queue was full — producers must never block")
	}

	close(release)
	d.Stop() // drains what survived the overflow policy, then joins

	mu.Lock()
	defer mu.Unlock()
	if delivered == 0 || delivered > total {
		t.Fatalf("post-release drain made no/invalid progress: %d delivered of %d emitted", delivered, total)
	}
}

// TestStopDrainsQueuedEventsAndHaltsWorker verifies Stop semantics on the real
// dispatcher: everything accepted before Stop reaches the sink exactly once,
// Stop returns only after the worker exited, and nothing is delivered
// afterwards.
func TestStopDrainsQueuedEventsAndHaltsWorker(t *testing.T) {
	srv, snapshot := newSink(t, 0)

	d := New(srv.URL).(*HTTPDispatcher)
	for i := 0; i < 10; i++ {
		d.Emit("audit.stop", map[string]int{"i": i})
	}
	d.Stop() // blocks until worker drained + exited

	evs := snapshot()
	if len(evs) != 10 {
		t.Fatalf("pre-Stop events must all be delivered by Stop: got %d, want 10", len(evs))
	}

	// Worker has provably exited (Stop joined it); late emits must be dropped,
	// not panic and not resurrect the worker.
	d.Emit("audit.after.stop", map[string]string{"late": "yes"})
	time.Sleep(200 * time.Millisecond)
	if got := snapshot(); len(got) != 10 {
		t.Fatalf("post-Stop emit was delivered: %d events, want 10", len(got))
	}

	d.Stop() // idempotent: second call must not panic or block
}

// TestUnmarshalablePayloadIsSkipped exercises the marshal-failure guard:
// an unmarshalable payload must neither enqueue nor crash the dispatcher while
// healthy events keep flowing.
func TestUnmarshalablePayloadIsSkipped(t *testing.T) {
	srv, snapshot := newSink(t, 0)
	d := New(srv.URL).(*HTTPDispatcher)

	d.Emit("audit.bad", make(chan int)) // json.Marshal fails
	d.Emit("audit.good", map[string]string{"ok": "1"})
	waitForCount(t, snapshot, 1, 5*time.Second)
	d.Stop()

	evs := snapshot()
	if len(evs) != 1 || evs[0].Header != "audit.good" {
		t.Fatalf("only the healthy event may reach the sink: %+v", evs)
	}
}

// TestNewFromEnvSelectsImplementation covers the env-driven constructor.
func TestNewFromEnvSelectsImplementation(t *testing.T) {
	t.Setenv("WEBHOOK_URL", "")
	if _, ok := NewFromEnv().(*EmptyDispatcher); !ok {
		t.Fatal("empty WEBHOOK_URL must yield EmptyDispatcher")
	}
	t.Setenv("WEBHOOK_URL", "http://127.0.0.1:1/hook")
	d := NewFromEnv()
	hd, ok := d.(*HTTPDispatcher)
	if !ok {
		t.Fatalf("set WEBHOOK_URL must yield HTTPDispatcher, got %T", d)
	}
	hd.Stop()
}

// TestConcurrentEmitSpray races Emitters against one dispatcher feeding a fast
// sink, then Stops. Total volume stays under QueueDepth so the drop-oldest
// overflow policy cannot legally fire, making exactly-once delivery assertable;
// the goroutine contention itself belongs to the race detector.
func TestConcurrentEmitSpray(t *testing.T) {
	srv, snapshot := newSink(t, 0)
	d := New(srv.URL).(*HTTPDispatcher)

	const workers, perWorker = 8, 30 // 240 total < QueueDepth(256)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				d.Emit("audit.spray", map[string]int{"w": seed, "i": i})
			}
		}(w)
	}
	wg.Wait()
	d.Stop()

	if got := len(snapshot()); got != workers*perWorker {
		t.Fatalf("concurrent emitters lost/duplicated events: got %d, want %d", got, workers*perWorker)
	}
}

// TestEmptyDispatcherNoOp keeps the default-path contract explicit.
func TestEmptyDispatcherNoOp(t *testing.T) {
	var d Dispatcher = &EmptyDispatcher{}
	d.Emit("anything", map[string]string{"k": "v"}) // must not panic
}
