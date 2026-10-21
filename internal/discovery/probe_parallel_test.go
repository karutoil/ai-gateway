package discovery

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
)

// The context probe is a real completion request against the upstream, and
// Discover runs up to eight of them. Issued serially, that made one Discover
// call cost eight sequential round-trips — measured 38s against a provider
// whose /models answered in 2ms. The probes are independent, so Discover must
// overlap them: with a per-probe delay, total wall-clock has to stay near a
// single round-trip rather than eight.
func TestDiscoverProbesRunConcurrently(t *testing.T) {
	const modelCount = 8
	const probeDelay = 250 * time.Millisecond

	var inFlight, peak int32
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			// Probe endpoint: track how many overlap, then answer like an
			// upstream that leaks its real cap in a 400.
			cur := atomic.AddInt32(&inFlight, 1)
			mu.Lock()
			if cur > peak {
				peak = cur
			}
			mu.Unlock()
			time.Sleep(probeDelay)
			atomic.AddInt32(&inFlight, -1)
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"max_model_len=262144"}`))
			return
		}
		w.Write([]byte(`{"object":"list","data":[`))
		for i := 0; i < modelCount; i++ {
			if i > 0 {
				w.Write([]byte(","))
			}
			fmt.Fprintf(w, `{"id":"m%d","object":"model","owned_by":"openai"}`, i)
		}
		w.Write([]byte(`]}`))
	}))
	t.Cleanup(srv.Close)

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i + 3)
	}
	ps := provider.NewStore(database, mk)
	p, err := ps.Create("openai", models.ProviderOpenAI, srv.URL+"/v1", "sk-test")
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	s := &Service{db: database, providerStore: ps, client: srv.Client()}

	start := time.Now()
	n, err := s.Discover(p.ID)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if n != modelCount {
		t.Fatalf("discovered %d models, want %d", n, modelCount)
	}

	// All eight models are bare-listed (no declared Meta), so every one is
	// probed, and each probe leaked a cap that must be recorded.
	var probed int
	for i := 0; i < modelCount; i++ {
		id := fmt.Sprintf("m%d", i)
		rowID, source, ok := modelRow(t, s.db, p.ID, id)
		if !ok {
			t.Fatalf("%s not discovered", id)
		}
		if source != "provider" {
			t.Fatalf("%s source = %q, want %q (probe result was dropped)", id, source, "provider")
		}
		var ctxWindow int
		if err := s.db.QueryRow(`SELECT context_window FROM provider_models WHERE id=?`, rowID).Scan(&ctxWindow); err != nil {
			t.Fatalf("read %s context: %v", id, err)
		}
		if ctxWindow != 262144 {
			t.Fatalf("%s context_window = %d, want 262144 from the probe", id, ctxWindow)
		}
		probed++
	}
	if probed != modelCount {
		t.Fatalf("verified %d probes, want %d", probed, modelCount)
	}

	// Serial probing would need models*probeDelay (2s) or more. Allow generous
	// slack for a loaded CI box while still failing a regression to serial.
	serial := time.Duration(modelCount) * probeDelay
	if elapsed >= serial/2 {
		t.Errorf("Discover took %v with %d probes of %v each; concurrent probing should finish near %v, not ~%v",
			elapsed, modelCount, probeDelay, probeDelay, serial)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak < 2 {
		t.Errorf("peak concurrent probes = %d; probes appear to have run serially", peak)
	}
}

// The probe budget caps how many upstream requests one Discover run may make.
// Parallelizing must not quietly raise that ceiling.
func TestDiscoverProbeBudgetStillCapsRequests(t *testing.T) {
	const modelCount = 20
	var probes int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			atomic.AddInt32(&probes, 1)
			// Not a cap error: the probe learns nothing and must not count.
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"choices":[{"message":{"content":"x"}}]}`))
			return
		}
		w.Write([]byte(`{"object":"list","data":[`))
		for i := 0; i < modelCount; i++ {
			if i > 0 {
				w.Write([]byte(","))
			}
			fmt.Fprintf(w, `{"id":"m%d","object":"model","owned_by":"openai"}`, i)
		}
		w.Write([]byte(`]}`))
	}))
	t.Cleanup(srv.Close)

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i + 3)
	}
	ps := provider.NewStore(database, mk)
	p, err := ps.Create("openai", models.ProviderOpenAI, srv.URL+"/v1", "sk-test")
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	s := &Service{db: database, providerStore: ps, client: srv.Client()}

	if n, err := s.Discover(p.ID); err != nil || n != modelCount {
		t.Fatalf("discover = %d, %v; want %d, nil", n, err, modelCount)
	}
	if got := atomic.LoadInt32(&probes); got != probeBudgetPerDiscover {
		t.Errorf("sent %d probes for %d bare-listed models, want the budget of %d", got, modelCount, probeBudgetPerDiscover)
	}
}