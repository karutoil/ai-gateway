package catalog

// Regression tests for the curated-seed defects:
//
//	P0-4  seeds used ON CONFLICT DO UPDATE SET on every column, so an operator
//	      price override (2.40) reverted to the baked-in value (0.24) on every
//	      boot and every 24h sync, and an operator capability correction was
//	      silently re-flagged.
//	P1-11 image/video seeds ran outside the sync transaction with `_, _ =`
//	      error swallowing and were unreachable at boot, so a fresh deploy that
//	      could not reach models.dev listed zero image/video models for up to 24h.

import (
	"testing"

	"ai-gateway/internal/db"
	"ai-gateway/internal/decision"
	"ai-gateway/internal/imagegen"
	"ai-gateway/internal/videogen"
)

func openCatalogTestDB(t *testing.T) *Store {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return NewStore(database)
}

// TestOperatorPriceSurvivesReseed is the headline P0-4 regression: an operator
// edit to a curated row must survive every seed path (boot seed, per-registry
// seed, and sync) because those paths are insert-only.
func TestOperatorPriceSurvivesReseed(t *testing.T) {
	s := openCatalogTestDB(t)

	if _, err := s.SeedDecisionModels(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	const id = "cloudflare/clef"
	before, err := s.Get(id)
	if err != nil {
		t.Fatalf("clef missing after seed: %v", err)
	}
	if before.InputCost == 2.40 {
		t.Skip("registry already carries the operator price; adjust the test constant")
	}

	// Operator negotiates a 10x rate and corrects a capability flag.
	if _, err := s.db.Exec(db.Q(`UPDATE models_catalog SET input_cost=2.40, output_cost=0, context_window=12345, name='Operator Clef', decision=0 WHERE id=?`), id); err != nil {
		t.Fatal(err)
	}
	s.InvalidateCache()

	reseed := map[string]func() (int, error){
		"SeedDecisionModels":   s.SeedDecisionModels,
		"SeedAllCuratedModels": s.SeedAllCuratedModels,
	}
	for name, fn := range reseed {
		if _, err := fn(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		s.InvalidateCache()
		m, err := s.Get(id)
		if err != nil {
			t.Fatalf("%s: clef vanished: %v", name, err)
		}
		if m.InputCost != 2.40 {
			t.Fatalf("%s reverted the operator price: input_cost = %v, want 2.40", name, m.InputCost)
		}
		if m.ContextWindow != 12345 {
			t.Fatalf("%s reverted the operator context window: %d, want 12345", name, m.ContextWindow)
		}
		if m.Name != "Operator Clef" {
			t.Fatalf("%s reverted the operator name: %q", name, m.Name)
		}
		// The stored flag, not the derived struct field: CatalogModel.Decision is
		// also derived from the modalities blob ("decision" output), so read the
		// column the seed would have clobbered.
		var storedDecision int
		if err := s.db.QueryRow(db.Q(`SELECT COALESCE(decision,0) FROM models_catalog WHERE id=?`), id).Scan(&storedDecision); err != nil {
			t.Fatalf("%s: read decision column: %v", name, err)
		}
		if storedDecision != 0 {
			t.Fatalf("%s re-flagged a row the operator un-flagged (decision=%d)", name, storedDecision)
		}
		// A sync must not revert the operator edits either.
		if _, err := s.SyncFromBytes([]byte(`{"openai":{"id":"openai","name":"OpenAI","api":"openai","models":{}}}`)); err != nil {
			t.Fatalf("%s: sync: %v", name, err)
		}
		s.InvalidateCache()
		m, err = s.Get(id)
		if err != nil {
			t.Fatalf("%s after sync: %v", name, err)
		}
		if m.InputCost != 2.40 {
			t.Fatalf("%s: sync reverted the operator price to %v", name, m.InputCost)
		}
	}
}

// TestSeedsAreInsertOnlyOnEveryColumn proves the seeds cannot overwrite ANY
// operator-editable column of an existing row, not just price — the old
// DO UPDATE list included name/description/family/modalities and all three
// capability flags.
func TestSeedsAreInsertOnlyOnEveryColumn(t *testing.T) {
	s := openCatalogTestDB(t)
	if _, err := s.SeedImageModels(); err != nil {
		t.Fatal(err)
	}
	var id, modalities string
	if err := s.db.QueryRow(`SELECT id, modalities FROM models_catalog WHERE image=1 ORDER BY id LIMIT 1`).Scan(&id, &modalities); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(db.Q(`UPDATE models_catalog SET name='Kept', description='Kept', family='kept', modalities=? , input_cost=9.99 WHERE id=?`), `{"output":["text"]}`, id); err != nil {
		t.Fatal(err)
	}
	s.InvalidateCache()
	if _, err := s.SeedImageModels(); err != nil {
		t.Fatal(err)
	}
	m, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "Kept" || m.Description != "Kept" || m.Family != "kept" || m.InputCost != 9.99 {
		t.Fatalf("seed overwrote operator columns: %+v", m)
	}
	if m.Modalities != `{"output":["text"]}` {
		t.Fatalf("seed overwrote operator modalities: %q", m.Modalities)
	}
}

// TestSeedAllCuratedModelsSeedsEveryRegistry pins the single boot entry point:
// one call must produce the decision, image AND video rows, so a deployment
// that never reaches models.dev still lists all three capability classes.
func TestSeedAllCuratedModelsSeedsEveryRegistry(t *testing.T) {
	s := openCatalogTestDB(t)
	n, err := s.SeedAllCuratedModels()
	if err != nil {
		t.Fatalf("SeedAllCuratedModels: %v", err)
	}
	want := len(decision.All()) + len(imagegen.All()) + len(videogen.All())
	if n != want {
		t.Fatalf("seeded %d rows, want %d (decision %d + image %d + video %d)",
			n, want, len(decision.All()), len(imagegen.All()), len(videogen.All()))
	}
	var d, i, v int
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(decision),0), COALESCE(SUM(image),0), COALESCE(SUM(video),0) FROM models_catalog`).Scan(&d, &i, &v); err != nil {
		t.Fatal(err)
	}
	if d == 0 || i == 0 || v == 0 {
		t.Fatalf("capability rows after SeedAllCuratedModels: decision=%d image=%d video=%d", d, i, v)
	}

	// A second call writes nothing at all: the seed-version marker short-circuits
	// before any INSERT, so a boot with an unchanged registry is free.
	n2, err := s.SeedAllCuratedModels()
	if err != nil {
		t.Fatalf("second SeedAllCuratedModels: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second SeedAllCuratedModels inserted %d rows, want 0 (unchanged registry)", n2)
	}
	var marker string
	if err := s.db.QueryRow(`SELECT value FROM system_config WHERE key=?`, curatedSeedVersionKey).Scan(&marker); err != nil {
		t.Fatalf("seed version marker missing: %v", err)
	}
	if marker != curatedSeedVersion(curatedSeedRows()) {
		t.Fatalf("seed version marker = %q, want %q", marker, curatedSeedVersion(curatedSeedRows()))
	}

	// Bumping the marker (simulating a new registry release) makes the seeder
	// run again — but still insert-only, so nothing existing is modified.
	if _, err := s.db.Exec(db.Q(`UPDATE system_config SET value='stale' WHERE key=?`), curatedSeedVersionKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SeedAllCuratedModels(); err != nil {
		t.Fatalf("SeedAllCuratedModels with bumped marker: %v", err)
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM models_catalog`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != want {
		t.Fatalf("row count after bumped-marker reseed = %d, want %d (no duplicates)", total, want)
	}
}

// TestSeedErrorsPropagate proves the seeds no longer swallow failures with
// `_, _ =`. Dropping the catalog table makes every INSERT fail; the error must
// reach the caller instead of a silent empty catalog.
func TestSeedErrorsPropagate(t *testing.T) {
	s := openCatalogTestDB(t)
	if _, err := s.db.Exec(`DROP TABLE models_catalog`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SeedAllCuratedModels(); err == nil {
		t.Fatal("SeedAllCuratedModels swallowed a hard failure (dropped table)")
	}
	if _, err := s.SeedDecisionModels(); err == nil {
		t.Fatal("SeedDecisionModels swallowed a hard failure (dropped table)")
	}
	if _, err := s.SeedImageModels(); err == nil {
		t.Fatal("SeedImageModels swallowed a hard failure (dropped table)")
	}
	if _, err := s.SeedVideoModels(); err == nil {
		t.Fatal("SeedVideoModels swallowed a hard failure (dropped table)")
	}
}

// TestSyncRollsBackWhenSeedFails proves the curated seeds now run INSIDE the
// sync transaction (P1-11). With system_config gone the seed step fails, so the
// whole sync must roll back — previously the models.dev rows were committed
// first and the seed ran afterwards on a separate connection with its error
// discarded, leaving a half-synced catalog.
func TestSyncRollsBackWhenSeedFails(t *testing.T) {
	s := openCatalogTestDB(t)
	if _, err := s.db.Exec(`DROP TABLE system_config`); err != nil {
		t.Fatal(err)
	}
	_, err := s.SyncFromBytes([]byte(`{"acme":{"id":"acme","name":"Acme","api":"openai","models":{
		"solo-1":{"id":"acme/solo-1","name":"Solo","cost":{"input":1,"output":2}}}}}`))
	if err == nil {
		t.Fatal("sync succeeded although the curated seed step failed")
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM models_catalog`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("sync left %d rows committed despite a failing seed; the seed must run inside the sync tx", n)
	}
}

// TestSyncPopulatesShortID proves the sync keeps the indexed short_id column in
// sync with the id, which is what makes the exact-match lookup tier work.
func TestSyncPopulatesShortID(t *testing.T) {
	s := openCatalogTestDB(t)
	if _, err := s.SyncFromBytes([]byte(`{"acme":{"id":"acme","name":"Acme","api":"openai","models":{
		"grok-4.6":{"id":"acme/grok-4.6","name":"Grok","cost":{"input":1,"output":2}}}}}`)); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		"acme/grok-4.6": "grok-4.6",
	} {
		var got string
		if err := s.db.QueryRow(db.Q(`SELECT COALESCE(short_id,'') FROM models_catalog WHERE id=?`), id).Scan(&got); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got != want {
			t.Errorf("%s short_id = %q, want %q", id, got, want)
		}
	}
	// Curated seeds must populate it too.
	if _, err := s.SeedAllCuratedModels(); err != nil {
		t.Fatal(err)
	}
	var missing int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM models_catalog WHERE short_id IS NULL OR short_id=''`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("%d catalog rows have no short_id after seeding", missing)
	}
}
