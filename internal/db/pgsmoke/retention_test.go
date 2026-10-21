package pgsmoke

// Postgres coverage for the two defects fixed alongside migration 030:
//
//   - P0-3/video_jobs retention: the sweep must work on TIMESTAMPTZ (dialect
//     trap: a string bound would compare against `date_style` output and miss the
//     boundary row), and migration 030's indexes must exist by name.
//   - P0-6/catalog lookup: models_catalog.short_id must exist and be indexed, and
//     the backfill must have populated it.
//
// Skipped unless GATEWAY_PG_DSN is set.

import (
	"context"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/catalog"
	"ai-gateway/internal/db"
)

// TestShortIDColumnOnPostgres proves migration 030 landed with the type and index
// the lookup tier needs.
func TestShortIDColumnOnPostgres(t *testing.T) {
	database := openPG(t)
	if got := columnType(t, database, "models_catalog", "short_id"); got != "text" {
		t.Fatalf("models_catalog.short_id is %q, want text", got)
	}
	var idx int
	if err := database.QueryRow(`SELECT COUNT(*) FROM pg_indexes WHERE tablename='models_catalog' AND indexname='idx_models_catalog_short_id'`).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatal("idx_models_catalog_short_id missing: catalog lookups would scan")
	}

	// A freshly synced row carries the suffix, and the lookup resolves by it.
	cs := catalog.NewStore(database)
	if _, err := cs.SyncFromBytes([]byte(`{"shortid":{"id":"shortid","name":"ShortID","api":"openai","models":{
		"gpt-4o":{"id":"shortid/gpt-4o","name":"GPT-4o","cost":{"input":2.5,"output":10}}}}}`)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	var short string
	if err := database.QueryRow(db.Q(`SELECT COALESCE(short_id,'') FROM models_catalog WHERE id=?`), "shortid/gpt-4o").Scan(&short); err != nil {
		t.Fatal(err)
	}
	if short != "gpt-4o" {
		t.Fatalf("short_id = %q, want gpt-4o", short)
	}
	if m, err := cs.GetByShortID("gpt-4o"); err != nil || m.ID != "shortid/gpt-4o" {
		t.Fatalf("lookup by short id: %v (%+v)", err, m)
	}
}

// TestVideoRetentionIndexesOnPostgres pins the three retention/list indexes.
func TestVideoRetentionIndexesOnPostgres(t *testing.T) {
	database := openPG(t)
	for _, name := range []string{
		"idx_video_jobs_expires",
		"idx_video_jobs_status_created",
		"idx_video_jobs_key_created",
	} {
		var n int
		if err := database.QueryRow(`SELECT COUNT(*) FROM pg_indexes WHERE tablename='video_jobs' AND indexname=$1`, name).Scan(&n); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n != 1 {
			t.Errorf("index %s missing after migration 030", name)
		}
	}
}

// TestPruneVideoJobsOnPostgres exercises the sweep against TIMESTAMPTZ columns.
// The MUTABLE clock is what makes the string-vs-time.Time trap visible: a sweep
// that bound a formatted string would silently keep every expired row.
func TestPruneVideoJobsOnPostgres(t *testing.T) {
	database := openPG(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	old := now.Add(-30 * 24 * time.Hour)
	pastExpiry := now.Add(-time.Hour)
	futureExpiry := now.Add(time.Hour)

	insert := func(id, status string, created time.Time, expires *time.Time) {
		t.Helper()
		var exp any
		if expires != nil {
			exp = *expires
		}
		if _, err := database.Exec(db.Q(`INSERT INTO video_jobs
			(id, provider_id, key_prefix, dialect, model, upstream_id, prompt, status, progress, created_at, updated_at, expires_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`),
			id, "pgprov", "sk-gw-test", "openai", "sora-2", "up_"+id, "a cat", status, 0, created, created, exp); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert("video_pg_expired", "completed", old, &pastExpiry)
	insert("video_pg_retention", "completed", old, nil)
	insert("video_pg_live", "in_progress", old, &pastExpiry)
	insert("video_pg_recent", "completed", now.Add(-time.Minute), nil)
	insert("video_pg_future", "completed", old, &futureExpiry)

	res, err := db.PruneVideoJobs(context.Background(), database, 500, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if res.Expired != 1 || res.Retention != 1 {
		t.Fatalf("prune deleted expired=%d retention=%d, want 1/1", res.Expired, res.Retention)
	}
	for _, c := range []struct {
		id   string
		want bool
	}{
		{"video_pg_expired", false},
		{"video_pg_retention", false},
		{"video_pg_live", true},
		{"video_pg_recent", true},
		{"video_pg_future", true},
	} {
		var n int
		if err := database.QueryRow(db.Q(`SELECT COUNT(*) FROM video_jobs WHERE id=?`), c.id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if (n == 1) != c.want {
			t.Errorf("%s present=%v, want %v", c.id, n == 1, c.want)
		}
	}

	// The per-key list must be an index scan with no sort node. Postgres reports
	// the plan as text lines; a Sort node means idx_video_jobs_key_created is not
	// doing its job.
	rows, err := database.Query(db.Q(`EXPLAIN SELECT id FROM video_jobs WHERE key_prefix=? ORDER BY created_at DESC LIMIT 100`), "sk-gw-test")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan += line + "\n"
	}
	if contains(plan, "Seq Scan on video_jobs") {
		t.Errorf("per-key list is a sequential scan:\n%s", plan)
	}
	if contains(plan, "Sort") {
		t.Errorf("per-key list builds a sort node:\n%s", plan)
	}
	if !contains(plan, "idx_video_jobs_key_created") {
		t.Errorf("per-key list does not use idx_video_jobs_key_created:\n%s", plan)
	}
}

// TestMigrateReplayOnMigratedPostgresDatabase proves the duplicate-object
// tolerance works for lib/pq error strings too: replaying the migration set on a
// fully migrated Postgres database must be a no-op instead of a dirty abort.
func TestMigrateReplayOnMigratedPostgresDatabase(t *testing.T) {
	database := openPG(t)
	// Record the ledger contents, wipe it, and replay.
	if _, err := database.Exec(`CREATE TEMP TABLE ledger_backup AS SELECT version, dirty FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(database); err != nil {
		t.Fatalf("replaying migrations on an already-migrated Postgres database must not fail: %v", err)
	}
	var n, dirty int
	if err := database.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN dirty THEN 1 ELSE 0 END),0) FROM schema_migrations`).Scan(&n, &dirty); err != nil {
		t.Fatal(err)
	}
	if want := db.LatestMigrationVersion(); n != want {
		t.Fatalf("ledger has %d entries after replay, want %d", n, want)
	}
	if dirty != 0 {
		t.Fatalf("%d migrations left dirty after a tolerated replay", dirty)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
