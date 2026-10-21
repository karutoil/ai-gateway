package catalog

import (
	"sync"
	"sync/atomic"
	"time"

	"ai-gateway/internal/models"
)

// modelCacheTTL is the backstop lifetime of a cached catalog row. Explicit
// invalidation (sync, seeds, InvalidateCache) is the primary mechanism; the TTL
// exists so an out-of-band edit this process cannot observe — an operator UPDATE
// against models_catalog, or a settings write in another package — becomes
// visible without a restart. Pricing must never be stale for longer than this: a
// stale input_cost mis-bills and mis-enforces budgets.
const modelCacheTTL = 30 * time.Second

// modelCacheNegativeTTL is the lifetime of a "no such model" entry. It only
// exists to collapse the repeated misses inside one request burst (the image
// guards ask about the same unknown id up to three times and each miss is the
// expensive containment scan), so it is deliberately much shorter than the
// positive TTL: a model that appears out of band must become visible almost
// immediately, because a miss bills at fallback pricing or zero.
const modelCacheNegativeTTL = 1 * time.Second

// modelCacheMaxEntries bounds the memo. The catalog is 3-5k rows today and only
// models that see traffic are ever cached, so a cap far below the table size
// keeps the cache well under a megabyte. On overflow the whole map is dropped
// rather than maintaining LRU bookkeeping: a cold cache costs one indexed
// lookup, not a full scan.
const modelCacheMaxEntries = 4096

// modelCacheEntry holds a catalog row value (not a pointer) so a caller that
// mutates the row it got back — FindBestMatch does exactly that through
// ensureReasoning and zeroFreeCosts — cannot corrupt the cache.
type modelCacheEntry struct {
	model   models.CatalogModel
	found   bool
	expires time.Time
}

// modelCache is a tiny concurrency-safe modelID -> catalog row memo.
//
// Why it exists: the lookup used to be a full table scan with a temp B-tree sort
// (6.16 ms/request at 4k rows on SQLite) on every chat request, and the image
// guards ask about the same model up to three times per request. The stored
// short_id column (migration 030) fixes the cost of a single lookup; this removes
// the repeats. Both a positive hit (a row) and a negative hit (a known-missing
// id) are cached: on an unknown model the three repeated misses are the expensive
// path, not the hits.
type modelCache struct {
	mu      sync.RWMutex
	entries map[string]modelCacheEntry
	// generation is the global catalog generation this cache last observed. A
	// bump (see InvalidateAllCaches) invalidates every store's cache without the
	// caller needing a *Store reference, which is what makes a settings write in
	// another package able to flush it.
	generation int64
	// hits/misses are counters for the benchmark and the invalidation tests.
	hits   int64
	misses int64
}

// catalogGeneration is bumped by InvalidateAllCaches. Every live modelCache
// notices on its next read.
var catalogGeneration int64

// InvalidateAllCaches flushes the model cache of every catalog.Store in this
// process.
//
// This is the hook for writes made outside internal/catalog that can change what
// a cached row means — the dashboard settings PUT/DELETE
// (internal/handler/catalog.go PutSettings / DeleteSetting) being the live
// example: they change price_fallback_* and other pricing inputs through plain
// SQL, with no reference to the catalog Store. Those handlers should call
//
//	catalog.InvalidateAllCaches()
//
// after a successful write. It is a single atomic increment, safe to call at any
// time and safe to call when no store exists.
func InvalidateAllCaches() {
	atomic.AddInt64(&catalogGeneration, 1)
}

// syncGeneration drops this cache's entries when another goroutine bumped the
// global generation. Cheap enough for the hot path: one atomic load per read, and
// a map swap only on an actual invalidation.
func (c *modelCache) syncGeneration() {
	if c == nil {
		return
	}
	gen := atomic.LoadInt64(&catalogGeneration)
	c.mu.RLock()
	same := c.generation == gen
	c.mu.RUnlock()
	if same {
		return
	}
	c.mu.Lock()
	if c.generation != gen {
		c.entries = make(map[string]modelCacheEntry, 64)
		c.generation = gen
	}
	c.mu.Unlock()
}

func newModelCache() *modelCache {
	return &modelCache{entries: make(map[string]modelCacheEntry, 64)}
}

// get returns the cached resolution for key. ok reports a live cache hit; found
// reports whether that hit is a real row (a negative entry has found=false and
// the caller must return sql.ErrNoRows). A positive hit returns a struct copy, so
// callers get their own row to mutate.
func (c *modelCache) get(key string) (m *models.CatalogModel, found, ok bool) {
	if c == nil || key == "" {
		return nil, false, false
	}
	c.syncGeneration()
	c.mu.RLock()
	e, present := c.entries[key]
	c.mu.RUnlock()
	if !present || !time.Now().Before(e.expires) {
		c.mu.Lock()
		c.misses++
		c.mu.Unlock()
		return nil, false, false
	}
	c.mu.Lock()
	c.hits++
	c.mu.Unlock()
	if !e.found {
		return nil, false, true
	}
	copied := e.model // struct copy: callers may mutate their row
	return &copied, true, true
}

// put stores a copy of m under key.
func (c *modelCache) put(key string, m *models.CatalogModel) {
	if c == nil || m == nil || key == "" {
		return
	}
	c.store(key, modelCacheEntry{model: *m, found: true, expires: time.Now().Add(modelCacheTTL)})
}

// putMiss records that key resolved to nothing, for the short negative TTL.
func (c *modelCache) putMiss(key string) {
	if c == nil || key == "" {
		return
	}
	c.store(key, modelCacheEntry{found: false, expires: time.Now().Add(modelCacheNegativeTTL)})
}

func (c *modelCache) store(key string, e modelCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= modelCacheMaxEntries {
		c.entries = make(map[string]modelCacheEntry, 64)
	}
	c.entries[key] = e
}

// clear drops every entry. Called by every write that can change a catalog row.
func (c *modelCache) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.entries = make(map[string]modelCacheEntry, 64)
	c.mu.Unlock()
}

// size is the current entry count (test/benchmark helper).
func (c *modelCache) size() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// InvalidateCache drops the in-process model cache.
//
// Call it from anything that writes models_catalog or the pricing settings
// outside this package: the dashboard settings PUT (internal/handler/catalog.go)
// can change the price_fallback_* values, and an operator UPDATE against
// models_catalog is not observable at all. Sync and the curated seeds already
// invalidate internally. Cheap (one mutex + a map alloc) and safe on a nil Store.
func (s *Store) InvalidateCache() {
	if s == nil {
		return
	}
	s.cache.clear()
}
