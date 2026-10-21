package cache

import (
	"net/http"
	"testing"
	"time"
)

func TestMemoryCacheSetGetRoundTrip(t *testing.T) {
	m := NewMemoryCache(16)
	h := http.Header{"Content-Type": []string{"application/json"}}
	m.Set("k1", []byte("body"), 200, h, 60)

	body, status, gotHeaders, ok := m.Get("k1")
	if !ok || string(body) != "body" || status != 200 {
		t.Fatalf("round trip failed: ok=%v body=%q status=%d", ok, body, status)
	}
	if gotHeaders.Get("Content-Type") != "application/json" {
		t.Fatalf("headers lost: %v", gotHeaders)
	}
}

// Entries expire after the TTL.
func TestMemoryCacheTTLExpiry(t *testing.T) {
	m := NewMemoryCache(16)
	m.Set("k1", []byte("body"), 200, nil, 0) // ttl<=0 normalized to 30s
	m.Set("k2", []byte("body"), 200, nil, 1)
	time.Sleep(1100 * time.Millisecond)
	if _, _, _, ok := m.Get("k2"); ok {
		t.Fatal("k2 should have expired after its 1s ttl")
	}
	if _, _, _, ok := m.Get("k1"); !ok {
		t.Fatal("k1 should still be live (30s default ttl)")
	}
}

// Bodies and headers are stored as copies: caller-side mutation must not
// leak into the cache.
func TestMemoryCacheDefensiveCopies(t *testing.T) {
	m := NewMemoryCache(16)
	body := []byte("original")
	h := http.Header{"X-A": []string{"1"}}
	m.Set("k", body, 200, h, 60)
	body[0] = 'X'
	h.Set("X-A", "2")
	gotBody, _, gotHeaders, ok := m.Get("k")
	if !ok || string(gotBody) != "original" {
		t.Fatalf("body was not copied: %q", gotBody)
	}
	if gotHeaders.Get("X-A") != "1" {
		t.Fatalf("headers were not copied: %v", gotHeaders)
	}
}

// The LRU evicts the least recently used key once full.
func TestMemoryCacheLRUEviction(t *testing.T) {
	m := NewMemoryCache(2)
	m.Set("a", []byte("a"), 200, nil, 60)
	m.Set("b", []byte("b"), 200, nil, 60)
	// Touch "a" so "b" becomes the LRU tail.
	if _, _, _, ok := m.Get("a"); !ok {
		t.Fatal("a should be present")
	}
	m.Set("c", []byte("c"), 200, nil, 60)
	if _, _, _, ok := m.Get("b"); ok {
		t.Fatal("b (LRU tail) should have been evicted")
	}
	for _, k := range []string{"a", "c"} {
		if _, _, _, ok := m.Get(k); !ok {
			t.Fatalf("%s should survive the eviction", k)
		}
	}
}

// Overwriting an existing key updates in place without evicting.
func TestMemoryCacheOverwriteInPlace(t *testing.T) {
	m := NewMemoryCache(2)
	m.Set("a", []byte("v1"), 200, nil, 60)
	m.Set("b", []byte("v1"), 200, nil, 60)
	m.Set("a", []byte("v2"), 201, nil, 60)
	if _, status, _, ok := m.Get("a"); !ok || status != 201 {
		t.Fatalf("overwrite failed: ok=%v status=%d", ok, status)
	}
	if _, _, _, ok := m.Get("b"); !ok {
		t.Fatal("b must not be evicted by an in-place overwrite")
	}
}

func TestMemoryCacheInvalidatePattern(t *testing.T) {
	m := NewMemoryCache(16)
	m.Set("user:1", []byte("x"), 200, nil, 60)
	m.Set("user:2", []byte("x"), 200, nil, 60)
	m.Set("model:1", []byte("x"), 200, nil, 60)
	m.Invalidate("user:")
	if _, _, _, ok := m.Get("user:1"); ok {
		t.Fatal("user:1 should be invalidated")
	}
	if _, _, _, ok := m.Get("user:2"); ok {
		t.Fatal("user:2 should be invalidated")
	}
	if _, _, _, ok := m.Get("model:1"); !ok {
		t.Fatal("model:1 must survive a user: invalidation")
	}
	m.Invalidate("*")
	if _, _, _, ok := m.Get("model:1"); ok {
		t.Fatal("star invalidation should clear everything")
	}
}

// A RedisCache wired without a client (redis down / not configured) must
// degrade to miss/no-op semantics, never panic.
func TestRedisCacheNilClientIsSafe(t *testing.T) {
	r := NewRedisCacheWithClient(nil)
	if _, _, _, ok := r.Get("k"); ok {
		t.Fatal("nil client must miss")
	}
	r.Set("k", []byte("v"), 200, nil, 60) // must not panic
	r.Invalidate("*")                     // must not panic
	if err := r.Close(); err != nil {
		t.Fatalf("Close on nil client = %v, want nil", err)
	}

	// Same for a typed nil, which is how a nil *RedisCache arrives via an
	// interface or a nil check elsewhere.
	var nilCache *RedisCache
	if _, _, _, ok := nilCache.Get("k"); ok {
		t.Fatal("nil *RedisCache must miss")
	}
	nilCache.Set("k", []byte("v"), 200, nil, 60)
	nilCache.Invalidate("*")
	if err := nilCache.Close(); err != nil {
		t.Fatalf("Close on nil *RedisCache = %v, want nil", err)
	}
}

// A full cache must evict EXPIRED entries first (the sweep), and only then
// the LRU tail — never a random entry (the old sweep bug compared against
// the incoming entry's expiry, so any older-but-live entry looked expired).
func TestMemoryCacheEvictsExpiredBeforeLRU(t *testing.T) {
	m := NewMemoryCache(2)
	m.Set("stale", []byte("s"), 200, nil, 1)
	m.Set("live", []byte("l"), 200, nil, 60)
	time.Sleep(1100 * time.Millisecond)
	// Insert into a full cache: the expired "stale" must be swept so the
	// live "live" entry survives without touching the LRU tail loop.
	m.Set("new", []byte("n"), 200, nil, 60)
	if _, _, _, ok := m.Get("live"); !ok {
		t.Fatal("live entry evicted while an expired entry was sweepable")
	}
	if _, _, _, ok := m.Get("new"); !ok {
		t.Fatal("new entry missing")
	}
}
