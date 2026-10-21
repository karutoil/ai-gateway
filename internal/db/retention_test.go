package db

// Tests for the two schema/retention defects fixed in migration 030 and
// internal/db/retention.go:
//
//   - video_jobs had no retention at all (P0-3): expires_at was written and
//     never read, and nothing but the client's own DELETE removed a row;
//   - models_catalog lookups were a full table scan (P0-6): migration 030 adds
//     the stored, indexed short_id the exact-match tier needs.
//
// Everything here runs on SQLite (the CI default); the Postgres half lives in
// internal/db/pgsmoke.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// insertVideoJob writes one video_jobs row directly (the proxy's write path is
// in another package). status/expires/created are the only fields that matter
// to the sweeps.
func insertVideoJob(t *testing.T, d *sql.DB, id, status string, created time.Time, expires *time.Time) {
	t.Helper()
	var exp any
	if expires != nil {
		exp = *expires
	}
	if _, err := d.Exec(Q(`INSERT INTO video_jobs
		(id, provider_id, key_prefix, dialect, model, upstream_id, prompt, status, progress, created_at, updated_at, expires_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`),
		id, "prov-1", "sk-gw-test", "openai", "sora-2", "up_"+id, "a cat", status, 0, created, created, exp); err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

func countVideoJobs(t *testing.T, d *sql.DB) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM video_jobs`).Scan(&n); err != nil {
		t.Fatalf("count video_jobs: %v", err)
	}
	return n
}

func videoJobExists(t *testing.T, d *sql.DB, id string) bool {
	t.Helper()
	var n int
	if err := d.QueryRow(Q(`SELECT COUNT(*) FROM video_jobs WHERE id=?`), id).Scan(&n); err != nil {
		t.Fatalf("exists %s: %v", id, err)
	}
	return n > 0
}

// TestPruneVideoJobsDeletesOnlyExpiredTerminalRows is the core retention
// contract: the sweep removes terminal rows past expires_at and terminal rows
// past the retention window, and touches nothing else. In particular an
// in-flight row (queued / in_progress) must survive even when it is ancient or
// carries an expiry — deleting it would 404 a client that is still polling.
func TestPruneVideoJobsDeletesOnlyExpiredTerminalRows(t *testing.T) {
	d := openAt(t, freshSQLiteFile(t))
	now := time.Now().UTC()
	old := now.Add(-30 * 24 * time.Hour)
	pastExpiry := now.Add(-time.Hour)
	futureExpiry := now.Add(time.Hour)

	// Rows that MUST be deleted.
	insertVideoJob(t, d, "v_expired_completed", "completed", old, &pastExpiry)
	insertVideoJob(t, d, "v_expired_failed", "failed", now.Add(-time.Minute), &pastExpiry)
	insertVideoJob(t, d, "v_old_completed_noexpiry", "completed", old, nil)
	insertVideoJob(t, d, "v_old_cancelled", "cancelled", old.Add(-time.Hour), nil)

	// Rows that MUST survive.
	insertVideoJob(t, d, "v_live_queued", "queued", old, nil)                           // ancient but in flight
	insertVideoJob(t, d, "v_live_inprogress", "in_progress", old, &pastExpiry)          // expiry ignored while pending
	insertVideoJob(t, d, "v_recent_completed", "completed", now.Add(-time.Minute), nil) // inside retention window
	insertVideoJob(t, d, "v_future_expiry", "completed", old, &futureExpiry)            // not expired yet

	res, err := PruneVideoJobs(context.Background(), d, 500, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if res.Expired != 2 {
		t.Errorf("expired deletions = %d, want 2", res.Expired)
	}
	if res.Retention != 2 {
		t.Errorf("retention deletions = %d, want 2", res.Retention)
	}
	if res.Total() != 4 {
		t.Errorf("total deletions = %d, want 4", res.Total())
	}

	for _, id := range []string{
		"v_expired_completed", "v_expired_failed", "v_old_completed_noexpiry", "v_old_cancelled",
	} {
		if videoJobExists(t, d, id) {
			t.Errorf("%s should have been pruned", id)
		}
	}
	for _, id := range []string{
		"v_live_queued", "v_live_inprogress", "v_recent_completed", "v_future_expiry",
	} {
		if !videoJobExists(t, d, id) {
			t.Errorf("%s must be kept (still in flight or inside retention)", id)
		}
	}
	if got := countVideoJobs(t, d); got != 4 {
		t.Fatalf("remaining rows = %d, want 4", got)
	}

	// A second sweep is a no-op: nothing deleted twice, no error.
	res2, err := PruneVideoJobs(context.Background(), d, 500, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if res2.Total() != 0 {
		t.Fatalf("second sweep deleted %d rows, want 0", res2.Total())
	}
}

// TestPruneVideoJobsIsBatched proves the sweep honours the batch bound: with
// batchSize 2 and 5 expired rows it must take at least 3 DELETE statements and
// still delete everything.
func TestPruneVideoJobsIsBatched(t *testing.T) {
	d := openAt(t, freshSQLiteFile(t))
	now := time.Now().UTC()
	expired := now.Add(-time.Hour)
	for i := 0; i < 5; i++ {
		insertVideoJob(t, d, "v_batch_"+itoa(i), "completed", now.Add(-2*time.Hour), &expired)
	}
	res, err := PruneVideoJobs(context.Background(), d, 2, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if res.Expired != 5 {
		t.Fatalf("deleted %d rows, want 5", res.Expired)
	}
	if res.Batches < 3 {
		t.Fatalf("batches = %d, want >= 3 for batchSize 2 and 5 rows", res.Batches)
	}
	if got := countVideoJobs(t, d); got != 0 {
		t.Fatalf("remaining rows = %d, want 0", got)
	}
}

// TestPruneVideoJobsConcurrent proves the sweep is safe to run from several
// goroutines at once: no error, no double-delete, and the live rows survive.
// SQLite serialises the writers (busy_timeout on the DSN), so this also pins
// that the sweep never leaves a statement aborted mid-transaction.
func TestPruneVideoJobsConcurrent(t *testing.T) {
	d := openAt(t, freshSQLiteFile(t))
	now := time.Now().UTC()
	expired := now.Add(-time.Hour)
	const n = 40
	for i := 0; i < n; i++ {
		insertVideoJob(t, d, "v_conc_"+itoa(i), "completed", now.Add(-2*time.Hour), &expired)
		insertVideoJob(t, d, "v_live_conc_"+itoa(i), "in_progress", now.Add(-2*time.Hour), nil)
	}

	const workers = 4
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	var deleted int64
	var mu sync.Mutex
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := PruneVideoJobs(context.Background(), d, 7, 7*24*time.Hour)
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			deleted += res.Total()
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent prune: %v", err)
	}
	// Each row is deleted exactly once across all workers, so the sum of the
	// reported deletions must be exactly the number of expired rows.
	if deleted != n {
		t.Fatalf("workers reported %d deletions, want %d (no row may be counted twice)", deleted, n)
	}
	if got := countVideoJobs(t, d); got != n {
		t.Fatalf("remaining rows = %d, want %d in-flight rows untouched", got, n)
	}
}

// TestPruneVideoJobsCancelledContext proves the sweep does not run away: a
// cancelled context returns the error instead of sweeping everything.
func TestPruneVideoJobsCancelledContext(t *testing.T) {
	d := openAt(t, freshSQLiteFile(t))
	now := time.Now().UTC()
	expired := now.Add(-time.Hour)
	for i := 0; i < 3; i++ {
		insertVideoJob(t, d, "v_ctx_"+itoa(i), "completed", now.Add(-2*time.Hour), &expired)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PruneVideoJobs(ctx, d, 500, 7*24*time.Hour); err == nil {
		t.Fatal("cancelled context returned nil error")
	}
	// Best-effort: a canceled sweep must not have partially deleted rows in a
	// way that breaks the next one.
	if _, err := PruneVideoJobs(context.Background(), d, 500, 7*24*time.Hour); err != nil {
		t.Fatalf("follow-up sweep: %v", err)
	}
	if got := countVideoJobs(t, d); got != 0 {
		t.Fatalf("remaining rows = %d, want 0", got)
	}
}

// TestPruneVideoJobsNilDatabase proves the nil guard, so an unconfigured slog
// path cannot panic the boot goroutine.
func TestPruneVideoJobsNilDatabase(t *testing.T) {
	if _, err := PruneVideoJobs(context.Background(), nil, 100, time.Hour); err == nil {
		t.Fatal("nil database returned nil error")
	}
}

// TestVideoRetentionIndexesExist pins migration 030's retention indexes and the
// (key_prefix, created_at) list index that removes the temp B-tree sort from
// GET /v1/videos.
func TestVideoRetentionIndexesExist(t *testing.T) {
	d := openAt(t, freshSQLiteFile(t))
	want := []string{
		"idx_video_jobs_expires",
		"idx_video_jobs_status_created",
		"idx_video_jobs_key_created",
		"idx_models_catalog_short_id",
	}
	for _, idx := range want {
		var n int
		if err := d.QueryRow(Q(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`), idx).Scan(&n); err != nil {
			t.Fatalf("look for %s: %v", idx, err)
		}
		if n != 1 {
			t.Errorf("index %s missing after migration 030", idx)
		}
	}

	// The per-key list query must use idx_video_jobs_key_created rather than
	// sorting in a temp b-tree. sqlite3 puts the detail column in position 3 of
	// EXPLAIN QUERY PLAN.
	rows, err := d.Query(`EXPLAIN QUERY PLAN SELECT id FROM video_jobs WHERE key_prefix=? ORDER BY created_at DESC LIMIT 100`, "sk-gw-test")
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
	got := plan.String()
	if strings.Contains(got, "TEMP B-TREE") {
		t.Errorf("per-key list still sorts in a temp b-tree:\n%s", got)
	}
	if !strings.Contains(got, "idx_video_jobs_key_created") {
		t.Errorf("per-key list does not use idx_video_jobs_key_created:\n%s", got)
	}
	// And the expiry sweep must be an index range scan, not a full scan.
	rows2, err := d.Query(`EXPLAIN QUERY PLAN
		SELECT id FROM video_jobs WHERE expires_at IS NOT NULL AND expires_at < ? LIMIT 10`, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	defer rows2.Close()
	plan.Reset()
	for rows2.Next() {
		var id, parent, notused int
		var detail string
		if err := rows2.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if !strings.Contains(plan.String(), "idx_video_jobs_expires") {
		t.Errorf("expiry sweep is not index-driven:\n%s", plan.String())
	}
}

// TestShortIDBackfillPopulatesLegacyRows proves the migration-030 backfill gives
// rows that already existed a short_id, so the exact-match lookup tier works on
// an upgraded database and not only on a freshly synced one.
func TestShortIDBackfillPopulatesLegacyRows(t *testing.T) {
	path := freshSQLiteFile(t)
	d := openAt(t, path)
	// Simulate rows written before 030: short_id NULL.
	for _, stmt := range []string{
		`INSERT INTO models_catalog(id,provider,name,updated_at) VALUES('openai/gpt-4o','openai','GPT-4o',CURRENT_TIMESTAMP)`,
		`INSERT INTO models_catalog(id,provider,name,updated_at) VALUES('bare-model','legacy','Bare',CURRENT_TIMESTAMP)`,
		`UPDATE models_catalog SET short_id=NULL`,
	} {
		if _, err := d.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := applyModelShortIDBackfill(d); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	cases := map[string]string{
		"openai/gpt-4o": "gpt-4o",
		"bare-model":    "bare-model",
	}
	for id, want := range cases {
		var got sql.NullString
		if err := d.QueryRow(Q(`SELECT short_id FROM models_catalog WHERE id=?`), id).Scan(&got); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if !got.Valid || got.String != want {
			t.Errorf("%s short_id = %q (valid=%v), want %q", id, got.String, got.Valid, want)
		}
	}

	// The exact-match tier must use the index and find the row by bare slug.
	var n int
	if err := d.QueryRow(Q(`SELECT COUNT(*) FROM models_catalog WHERE short_id=?`), "gpt-4o").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("short_id lookup found %d rows, want 1", n)
	}
	rows, err := d.Query(`EXPLAIN QUERY PLAN SELECT id FROM models_catalog WHERE short_id=?`, "gpt-4o")
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
		t.Errorf("short_id lookup is not index-driven:\n%s", plan.String())
	}
	if strings.Contains(plan.String(), "SCAN models_catalog") {
		t.Errorf("short_id lookup still scans the table:\n%s", plan.String())
	}
}

// TestShortIDHelper pins the extraction helper the write paths and the backfill
// share.
func TestShortIDHelper(t *testing.T) {
	cases := map[string]string{
		"openai/gpt-4o":                  "gpt-4o",
		"google/gemini-2.5-flash":        "gemini-2.5-flash",
		"bare":                           "bare",
		"oc1/muse-spark-1.3-contributor": "muse-spark-1.3-contributor",
		"trailing/":                      "trailing/",
		"  spaced/id  ":                  "id",
		"":                               "",
	}
	for in, want := range cases {
		if got := ShortID(in); got != want {
			t.Errorf("ShortID(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMigrateReplayOnMigratedDatabaseIsNoOp is the P1-13 regression test.
//
// Replaying the migration set against a database that already has the schema —
// which is what happens to a legacy DB with an empty/sparse ledger — used to
// abort at migration 4 with "duplicate column name: last_login_at", mark it
// dirty, and then refuse every subsequent boot ("migration 4 is marked dirty").
// The statement loop must tolerate duplicate-object errors instead.
func TestMigrateReplayOnMigratedDatabaseIsNoOp(t *testing.T) {
	path := freshSQLiteFile(t)
	d := openAt(t, path)

	// Empty the ledger without touching the schema: the legacy/brownfield shape.
	if _, err := d.Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO models_catalog(id,provider,name,short_id) VALUES('openai/gpt-4o','openai','GPT-4o','gpt-4o')`); err != nil {
		t.Fatal(err)
	}
	// lb_rules is the one table a migration REBUILDS (020 does
	// create/copy/drop/rename). Replaying it must preserve rows and every column
	// added later (025's quota_threshold_pct is re-added afterwards, so the row
	// reappears with that column NULL — which is why this asserts the row and the
	// pre-020 columns rather than a full schema equality).
	if _, err := d.Exec(`INSERT INTO lb_rules(id, model, provider_id, position, created_at, strategy, model_override, weight, quota_threshold_pct)
		VALUES('lb1','gpt-4o','prov-1',0,CURRENT_TIMESTAMP,'round_robin','',1,75)`); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(d); err != nil {
		t.Fatalf("replaying migrations on an already-migrated database must not fail, got: %v", err)
	}

	var n, dirty int
	if err := d.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN dirty THEN 1 ELSE 0 END),0) FROM schema_migrations`).Scan(&n, &dirty); err != nil {
		t.Fatal(err)
	}
	if want := LatestMigrationVersion(); n != want {
		t.Fatalf("ledger has %d entries after replay, want %d", n, want)
	}
	if dirty != 0 {
		t.Fatalf("%d migrations left dirty after a tolerated replay", dirty)
	}
	// The operator's data must be intact.
	var name string
	if err := d.QueryRow(`SELECT name FROM models_catalog WHERE id='openai/gpt-4o'`).Scan(&name); err != nil {
		t.Fatalf("row lost during replay: %v", err)
	}
	if name != "GPT-4o" {
		t.Fatalf("row changed during replay: %q", name)
	}
	var lbModel string
	var lbWeight int
	if err := d.QueryRow(`SELECT model, weight FROM lb_rules WHERE id='lb1'`).Scan(&lbModel, &lbWeight); err != nil {
		t.Fatalf("lb_rules row lost during replay (migration 020 rebuilds the table): %v", err)
	}
	if lbModel != "gpt-4o" || lbWeight != 1 {
		t.Fatalf("lb_rules row corrupted during replay: %q/%d", lbModel, lbWeight)
	}
	var lbCount int
	if err := d.QueryRow(`SELECT COUNT(*) FROM lb_rules`).Scan(&lbCount); err != nil {
		t.Fatal(err)
	}
	if lbCount != 1 {
		t.Fatalf("lb_rules has %d rows after replay, want 1 (the 020 rebuild must not duplicate rows)", lbCount)
	}
	for _, col := range []string{"strategy", "model_override", "weight", "quota_threshold_pct"} {
		if _, _, _, err := columnInfo(t, d, "lb_rules", col); err != nil {
			t.Errorf("lb_rules.%s lost during replay: %v", col, err)
		}
	}

	// And the following boot must be clean, not a dirty abort.
	d.Close()
	d2 := openAt(t, path)
	var after int
	if err := d2.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != LatestMigrationVersion() {
		t.Fatalf("post-replay boot ledger = %d, want %d", after, LatestMigrationVersion())
	}
}

// TestLatestMigrationVersionMatchesMigrationCount pins LatestMigrationVersion()
// to the number of embedded migration files on disk. Adding a migration and
// forgetting either the go:embed or the migrationsList entry makes this fail
// instead of shipping a fresh boot that stops at an older version.
func TestLatestMigrationVersionMatchesMigrationCount(t *testing.T) {
	ms := migrationsList()
	files, err := filepath.Glob("migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(files), len(ms); got != want {
		t.Fatalf("%d .sql files on disk but %d embedded migrations", got, want)
	}
	if got, want := LatestMigrationVersion(), len(ms); got != want {
		t.Fatalf("LatestMigrationVersion() = %d, embedded migrations = %d", got, want)
	}
	for i, m := range ms {
		if m.version != i+1 {
			t.Fatalf("migration at index %d has version %d, want %d", i, m.version, i+1)
		}
		if strings.TrimSpace(m.sql) == "" {
			t.Fatalf("migration %d embeds empty SQL", m.version)
		}
	}
	// Every file must be embedded: a new .sql with no go:embed is exactly the
	// drift this guards against.
	for _, f := range files {
		base := filepath.Base(f)
		var want int
		if _, err := fmtSscanfVersion(base, &want); err != nil {
			t.Fatalf("migration file %q does not start with a version", base)
		}
		if want > LatestMigrationVersion() {
			t.Fatalf("%q is newer than LatestMigrationVersion()=%d: add the go:embed and migrationsList entry", base, LatestMigrationVersion())
		}
	}

	// An applied fresh database must agree with the constant.
	d := openAt(t, freshSQLiteFile(t))
	var applied int
	if err := d.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != LatestMigrationVersion() {
		t.Fatalf("applied migrations = %d, LatestMigrationVersion() = %d", applied, LatestMigrationVersion())
	}
}

// fmtSscanfVersion parses the leading "NNN_" of a migration filename.
func fmtSscanfVersion(base string, out *int) (int, error) {
	i := strings.IndexByte(base, '_')
	if i <= 0 {
		return 0, os.ErrInvalid
	}
	n := 0
	for _, c := range base[:i] {
		if c < '0' || c > '9' {
			return 0, os.ErrInvalid
		}
		n = n*10 + int(c-'0')
	}
	*out = n
	return 1, nil
}

// TestIsDuplicateObjectErr pins the tolerance classifier: only "already there"
// errors are tolerated, so a genuinely broken migration still aborts dirty.
func TestIsDuplicateObjectErr(t *testing.T) {
	dupes := []string{
		"duplicate column name: last_login_at",
		"table video_jobs already exists",
		"index idx_video_jobs_expires already exists",
		"pq: relation \"models_catalog\" already exists (42710)",
		"duplicate_table",
		"SQLSTATE 42701 duplicate_column",
	}
	for _, s := range dupes {
		if !isDuplicateObjectErr(errString(s)) {
			t.Errorf("%q should be tolerated as a duplicate object", s)
		}
	}
	fatal := []string{
		"syntax error near FROM",
		"no such table: models_catalog",
		"disk I/O error",
		"permission denied for table providers",
		"pq: integer out of range",
	}
	for _, s := range fatal {
		if isDuplicateObjectErr(errString(s)) {
			t.Errorf("%q must NOT be tolerated", s)
		}
	}
	if isDuplicateObjectErr(nil) {
		t.Error("nil error must not be a duplicate")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// TestLegacyDatabaseGetsImageVideoAndShortID is the P1-13 mirror check.
//
// A pre-026 database reaches Migrate with a legacy (or empty) ledger. The
// hand-written legacy ALTER path mirrored `decision` but not `image`/`video`, and
// the legacySchema block had no video_jobs table at all, so a brownfield upgrade
// could land without the three capability surfaces. This test strips the newer
// schema off a migrated database (the shape a legacy DB has), wipes the ledger,
// and asserts the legacy path puts every column/table back.
func TestLegacyDatabaseGetsImageVideoAndShortID(t *testing.T) {
	path := freshSQLiteFile(t)
	d := openAt(t, path)

	strips := []string{
		`DROP INDEX IF EXISTS idx_models_catalog_video`,
		`DROP INDEX IF EXISTS idx_models_catalog_image`,
		`DROP INDEX IF EXISTS idx_models_catalog_decision`,
		`DROP INDEX IF EXISTS idx_models_catalog_short_id`,
		`DROP INDEX IF EXISTS idx_models_catalog_id_lower`,
		`ALTER TABLE models_catalog DROP COLUMN video`,
		`ALTER TABLE models_catalog DROP COLUMN image`,
		`ALTER TABLE models_catalog DROP COLUMN decision`,
		`ALTER TABLE models_catalog DROP COLUMN short_id`,
		`ALTER TABLE provider_models DROP COLUMN video`,
		`ALTER TABLE provider_models DROP COLUMN image`,
		`ALTER TABLE provider_models DROP COLUMN decision`,
		`DROP TABLE IF EXISTS video_jobs`,
		`DELETE FROM schema_migrations`,
	}
	for _, stmt := range strips {
		if _, err := d.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	d.Close()

	d2 := openAt(t, path)
	for _, table := range []string{"models_catalog", "provider_models"} {
		for _, col := range []string{"decision", "image", "video"} {
			if _, _, _, err := columnInfo(t, d2, table, col); err != nil {
				t.Errorf("%s.%s missing after the legacy path ran: %v", table, col, err)
			}
		}
	}
	if _, _, _, err := columnInfo(t, d2, "models_catalog", "short_id"); err != nil {
		t.Errorf("models_catalog.short_id missing after the legacy path ran: %v", err)
	}
	for _, col := range []string{"id", "provider_id", "key_prefix", "status", "expires_at", "created_at"} {
		if _, _, _, err := columnInfo(t, d2, "video_jobs", col); err != nil {
			t.Errorf("video_jobs.%s missing after the legacy path ran: %v", col, err)
		}
	}
	// The retention indexes must be there too, or the sweep table-scans forever.
	for _, idx := range []string{"idx_video_jobs_expires", "idx_video_jobs_status_created", "idx_video_jobs_key_created", "idx_models_catalog_short_id"} {
		var n int
		if err := d2.QueryRow(Q(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`), idx).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("index %s missing after the legacy path ran", idx)
		}
	}
	// The pruner must work on the recreated table.
	now := time.Now().UTC()
	expired := now.Add(-time.Hour)
	insertVideoJob(t, d2, "v_legacy", "completed", now.Add(-2*time.Hour), &expired)
	if _, err := PruneVideoJobs(context.Background(), d2, 100, time.Hour); err != nil {
		t.Fatalf("prune on legacy-recreated table: %v", err)
	}
	if videoJobExists(t, d2, "v_legacy") {
		t.Error("legacy-recreated video_jobs was not pruned")
	}
}
