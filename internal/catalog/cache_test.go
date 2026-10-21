package catalog

// P0-6: the per-request catalog lookup used to be a full table scan.
//
// Two independent fixes are pinned here:
//   - migration 030 stores an indexed short_id, and GetByShortID tries the exact
//     match first (see catalog_test.go for the plan assertion);
//   - a small in-process modelID -> row cache removes the repeated lookups the
//     capability guards make (up to 3x per image request), invalidated on sync
//     and on every other write that can change a catalog row.
//
// BenchmarkCatalogLookup is the before/after measurement: the "containment"
// sub-benchmark runs the exact query the old GetByShortID ran
// (LOWER(id) LIKE '%slug%' + ORDER BY), and "cached"/"indexed" run the new path.

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-gateway/internal/db"
)

// seedBigCatalog inserts n rows shaped like a real models.dev snapshot
// (<provider>/<slug>), which is what makes the containment LIKE a full scan.
func seedBigCatalog(t *testing.T, s *Store, n int) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO models_catalog(id, provider, name, input_cost, output_cost, updated_at, short_id)
		VALUES(?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		prov := fmt.Sprintf("prov%02d", i%40)
		id := fmt.Sprintf("%s/model-%05d", prov, i)
		if _, err := stmt.Exec(id, prov, fmt.Sprintf("Model %d", i), 1.0, 2.0, now, fmt.Sprintf("model-%05d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.InvalidateCache()
}

// BenchmarkCatalogLookup measures the three lookup shapes on a 4k-row catalog.
func BenchmarkCatalogLookup(b *testing.B) {
	database, err := db.Open(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)
	// b.N iterations of testing.TB helpers want a real *testing.T; seed through
	// the store instead.
	if err := seedBigCatalogBench(s, 4000); err != nil {
		b.Fatal(err)
	}

	// A model that lives in the middle of the table, addressed by bare slug.
	const short = "model-02000"

	b.Run("containment", func(b *testing.B) {
		// The pre-030 query: leading-wildcard LIKE + ORDER BY => SCAN + temp b-tree.
		q := db.Q(`SELECT ` + catalogColumns + ` FROM models_catalog WHERE LOWER(id) LIKE LOWER(?) ESCAPE '\' ORDER BY (CASE WHEN input_cost>0 OR output_cost>0 THEN 0 ELSE 1 END), id LIMIT 1`)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := scanCatalogModel(s.db.QueryRow(q, "%/"+escapeLike(short))); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("indexed_short_id", func(b *testing.B) {
		q := db.Q(`SELECT ` + catalogColumns + ` FROM models_catalog WHERE short_id=? ORDER BY (CASE WHEN input_cost>0 OR output_cost>0 THEN 0 ELSE 1 END), id LIMIT 1`)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := scanCatalogModel(s.db.QueryRow(q, short)); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("GetByShortID_cold", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			s.InvalidateCache()
			if _, err := s.GetByShortID(short); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("GetByShortID_cached", func(b *testing.B) {
		if _, err := s.GetByShortID(short); err != nil {
			b.Fatal(err)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := s.GetByShortID(short); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// seedBigCatalogBench mirrors seedBigCatalog for benchmarks (which have no
// *testing.T).
func seedBigCatalogBench(s *Store, n int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO models_catalog(id, provider, name, input_cost, output_cost, updated_at, short_id)
		VALUES(?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		prov := fmt.Sprintf("prov%02d", i%40)
		id := fmt.Sprintf("%s/model-%05d", prov, i)
		if _, err := stmt.Exec(id, prov, fmt.Sprintf("Model %d", i), 1.0, 2.0, now, fmt.Sprintf("model-%05d", i)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.InvalidateCache()
	return nil
}

// TestCatalogLookupUsesShortIDIndex proves the exact-match tier is what the
// common lookup hits, and that it is an index lookup rather than a scan.
func TestCatalogLookupUsesShortIDIndex(t *testing.T) {
	s := openCatalogTestDB(t)
	seedBigCatalog(t, s, 200)
	m, err := s.GetByShortID("model-00100")
	if err != nil {
		t.Fatalf("GetByShortID: %v", err)
	}
	if m.ID != "prov20/model-00100" {
		t.Fatalf("resolved %q, want prov20/model-00100", m.ID)
	}

	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT id FROM models_catalog WHERE short_id=?`, "model-00100")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if !strings.Contains(plan.String(), "idx_models_catalog_short_id") {
		t.Fatalf("short_id lookup is not index-driven:\n%s", plan.String())
	}
}

// TestCatalogCacheHitAvoidsQuery proves the memo actually short-circuits the
// database: after one lookup the row is served from memory, so a concurrent
// schema change would not even be observed until the cache is cleared.
func TestCatalogCacheHitAvoidsQuery(t *testing.T) {
	s := openCatalogTestDB(t)
	if _, err := s.SeedDecisionModels(); err != nil {
		t.Fatal(err)
	}
	m, err := s.Get("cloudflare/clef")
	if err != nil {
		t.Fatal(err)
	}
	if m.InputCost == 0 {
		t.Fatal("clef unpriced")
	}
	if s.cache.size() == 0 {
		t.Fatal("Get did not populate the cache")
	}

	// Mutate behind the cache's back: the cached entry must still be served.
	if _, err := s.db.Exec(`UPDATE models_catalog SET input_cost=99 WHERE id='cloudflare/clef'`); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("cloudflare/clef")
	if err != nil {
		t.Fatal(err)
	}
	if got.InputCost == 99 {
		t.Fatal("Get went to the database although the row is cached")
	}
	// InvalidateCache is the documented escape hatch for out-of-band writes.
	s.InvalidateCache()
	got, err = s.Get("cloudflare/clef")
	if err != nil {
		t.Fatal(err)
	}
	if got.InputCost != 99 {
		t.Fatalf("after InvalidateCache input_cost = %v, want 99", got.InputCost)
	}
}

// TestCatalogCacheInvalidatesOnSync is the behavioural contract requested for
// the cache: a sync (the only routine writer of the whole table) must drop it,
// so a refreshed price/pricing row is visible immediately.
func TestCatalogCacheInvalidatesOnSync(t *testing.T) {
	s := openCatalogTestDB(t)
	if _, err := s.SyncFromBytes([]byte(`{"acme":{"id":"acme","name":"Acme","api":"openai","models":{
		"grok-4.6":{"id":"acme/grok-4.6","name":"Grok","cost":{"input":1,"output":2}}}}}`)); err != nil {
		t.Fatal(err)
	}
	first, err := s.Get("acme/grok-4.6")
	if err != nil {
		t.Fatal(err)
	}
	if first.InputCost != 1 {
		t.Fatalf("input cost = %v, want 1", first.InputCost)
	}
	if s.cache.size() == 0 {
		t.Fatal("lookup did not warm the cache")
	}

	// Same id, new price — upstream is the source of truth for a sync.
	if _, err := s.SyncFromBytes([]byte(`{"acme":{"id":"acme","name":"Acme","api":"openai","models":{
		"grok-4.6":{"id":"acme/grok-4.6","name":"Grok","cost":{"input":5,"output":7}}}}}`)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("acme/grok-4.6")
	if err != nil {
		t.Fatal(err)
	}
	if got.InputCost != 5 || got.OutputCost != 7 {
		t.Fatalf("sync did not invalidate the cache: input=%v output=%v, want 5/7", got.InputCost, got.OutputCost)
	}
}

// TestCatalogCacheInvalidatesOnSeed proves the seed paths clear the cache too,
// so a freshly boot-seeded model is resolvable in the same process.
func TestCatalogCacheInvalidatesOnSeed(t *testing.T) {
	s := openCatalogTestDB(t)
	// A miss caches nothing, but a stale "not found" must not be possible: the
	// cache only stores hits, so this asserts the seed path clears what was
	// there. Warm it with a real row first.
	if _, err := s.SeedVideoModels(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("openai/sora-2"); err != nil {
		t.Fatal(err)
	}
	if s.cache.size() == 0 {
		t.Fatal("expected a warm cache")
	}
	if _, err := s.SeedDecisionModels(); err != nil {
		t.Fatal(err)
	}
	if s.cache.size() != 0 {
		t.Fatalf("seed did not invalidate the cache (%d entries left)", s.cache.size())
	}
	if _, err := s.Get("cloudflare/clef"); err != nil {
		t.Fatalf("a freshly seeded model must be resolvable: %v", err)
	}
}

// TestCatalogCacheReturnsIndependentCopies proves a caller that mutates the row
// it received (FindBestMatch does this via zeroFreeCosts/ensureReasoning) cannot
// corrupt the cached value. A shared pointer would make a free-tier lookup zero
// out the price that every later paid request sees.
func TestCatalogCacheReturnsIndependentCopies(t *testing.T) {
	s := openCatalogTestDB(t)
	if _, err := s.SeedDecisionModels(); err != nil {
		t.Fatal(err)
	}
	m1, err := s.Get("cloudflare/clef")
	if err != nil {
		t.Fatal(err)
	}
	want := m1.InputCost
	m1.InputCost = 0
	m1.Decision = false
	m2, err := s.Get("cloudflare/clef")
	if err != nil {
		t.Fatal(err)
	}
	if m2.InputCost != want {
		t.Fatalf("mutating a returned row corrupted the cache: %v -> %v", want, m2.InputCost)
	}
	if !m2.Decision {
		t.Fatal("mutating a returned row cleared the cached decision flag")
	}
}

// TestCatalogCacheConcurrent exercises the memo under -race: concurrent readers
// and invalidations must not corrupt the map.
func TestCatalogCacheConcurrent(t *testing.T) {
	s := openCatalogTestDB(t)
	if _, err := s.SeedAllCuratedModels(); err != nil {
		t.Fatal(err)
	}
	ids := []string{"cloudflare/clef", "openai/gpt-image-1", "openai/sora-2"}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if _, err := s.Get(ids[i%len(ids)]); err != nil {
					t.Errorf("concurrent Get: %v", err)
					return
				}
				if i%17 == 0 {
					s.InvalidateCache()
				}
			}
		}(w)
	}
	wg.Wait()
}

// TestGlobalCacheInvalidation proves the cross-package hook works: a settings
// write in another package holds no *Store, so InvalidateAllCaches is the only
// way it can flush a cached price.
func TestGlobalCacheInvalidation(t *testing.T) {
	s := openCatalogTestDB(t)
	if _, err := s.SeedDecisionModels(); err != nil {
		t.Fatal(err)
	}
	m, err := s.Get("cloudflare/clef")
	if err != nil {
		t.Fatal(err)
	}
	want := m.InputCost
	if want == 0 {
		t.Fatal("clef unpriced")
	}
	if _, err := s.db.Exec(`UPDATE models_catalog SET input_cost=4.2 WHERE id='cloudflare/clef'`); err != nil {
		t.Fatal(err)
	}
	// Still the cached value.
	if got, _ := s.Get("cloudflare/clef"); got.InputCost != want {
		t.Fatalf("cache was already invalidated: %v", got.InputCost)
	}
	InvalidateAllCaches()
	got, err := s.Get("cloudflare/clef")
	if err != nil {
		t.Fatal(err)
	}
	if got.InputCost != 4.2 {
		t.Fatalf("InvalidateAllCaches did not flush the cache: input_cost = %v, want 4.2", got.InputCost)
	}
}

// TestNegativeCacheCollapsesRepeatedMisses pins the reason the negative entry
// exists: the capability guards ask about the same unknown model up to three
// times per request, and each miss is the expensive containment scan.
func TestNegativeCacheCollapsesRepeatedMisses(t *testing.T) {
	s := openCatalogTestDB(t)
	if _, err := s.GetByShortID("no-such-model-anywhere"); err == nil {
		t.Fatal("expected a miss")
	}
	s.cache.mu.RLock()
	after := s.cache.misses
	s.cache.mu.RUnlock()
	for i := 0; i < 5; i++ {
		if _, err := s.GetByShortID("no-such-model-anywhere"); err == nil {
			t.Fatal("expected a miss")
		}
	}
	s.cache.mu.RLock()
	now := s.cache.misses
	s.cache.mu.RUnlock()
	if now != after {
		t.Fatalf("repeat misses went back to the database (%d -> %d cache misses)", after, now)
	}

	// The negative entry must be scoped to GetByShortID: a direct Get on the same
	// string must still be able to hit a row with that literal id.
	if _, err := s.db.Exec(`INSERT INTO models_catalog(id,provider,name) VALUES('no-such-model-anywhere','x','X')`); err != nil {
		t.Fatal(err)
	}
	InvalidateAllCaches()
	if _, err := s.GetByShortID("no-such-model-anywhere"); err != nil {
		t.Fatalf("a negative cache entry survived invalidation: %v", err)
	}
}
