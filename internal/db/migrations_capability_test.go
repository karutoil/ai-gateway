package db

// Migration + dialect tests for the three NEW surfaces (decision / image /
// video). They run against the SQLite default because that is the path CI takes
// (`go test ./...` never sets GATEWAY_PG_DSN); the Postgres half lives in
// internal/db/pgsmoke and is skipped without a DSN.
//
// These pin the four things that broke or could break silently:
//   - a fresh database migrates all the way to LatestMigrationVersion,
//   - the capability columns exist with the documented INTEGER affinity and
//     0 defaults (so old rows stay classified),
//   - a dirty migration aborts boot instead of being skipped,
//   - BoolInt is the only sane way to write those columns from Go.

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// freshSQLiteFile opens a brand-new file-backed database. ":memory:" cannot be
// reused across connections and the migration ledger needs a stable file for
// the re-open assertions.
func freshSQLiteFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "gw.db")
}

func openAt(t *testing.T, path string) *sql.DB {
	t.Helper()
	t.Setenv("DATABASE_URL", "")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// TestFreshDatabaseMigratesToLatest proves a brand-new database applies every
// embedded migration (1..LatestMigrationVersion) and records them clean.
func TestFreshDatabaseMigratesToLatest(t *testing.T) {
	d := openAt(t, freshSQLiteFile(t))

	var n, dirty int
	if err := d.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN dirty THEN 1 ELSE 0 END),0) FROM schema_migrations`).Scan(&n, &dirty); err != nil {
		t.Fatal(err)
	}
	if want := LatestMigrationVersion(); n != want {
		t.Fatalf("applied migrations = %d, want %d", n, want)
	}
	// The literal is the drift guard: adding a migration without bumping the
	// embed list (or vice versa) makes this fail. Bump it in the SAME commit as
	// the new migration file — that is the point (it once went stale for two
	// migrations, which is why this assertion exists).
	if n != 33 {
		t.Fatalf("LatestMigrationVersion() = %d, expected the schema to be at 33 (add your migration to migrationsList and bump this literal)", n)
	}
	if dirty != 0 {
		t.Fatalf("%d migrations left dirty", dirty)
	}
	// Every version 1..LatestMigrationVersion present exactly once.
	for v := 1; v <= LatestMigrationVersion(); v++ {
		var cnt int
		if err := d.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=?`, v).Scan(&cnt); err != nil {
			t.Fatal(err)
		}
		if cnt != 1 {
			t.Errorf("migration %d recorded %d times", v, cnt)
		}
	}
}

// TestCapabilityColumnsExistWithIntegerAffinity proves migrations 026/027/028/029
// produced the columns the Go layer writes: decision/image/video INTEGER with a
// 0 default on both model tables, and the video_jobs table with its indexes.
func TestCapabilityColumnsExistWithIntegerAffinity(t *testing.T) {
	d := openAt(t, freshSQLiteFile(t))

	for _, table := range []string{"models_catalog", "provider_models"} {
		for _, col := range []string{"decision", "image", "video"} {
			typ, dflt, notnull, err := columnInfo(t, d, table, col)
			if err != nil {
				t.Fatalf("%s.%s: %v", table, col, err)
			}
			if typ != "INTEGER" {
				t.Errorf("%s.%s type = %q, want INTEGER", table, col, typ)
			}
			if dflt != "0" {
				t.Errorf("%s.%s default = %q, want 0", table, col, dflt)
			}
			if notnull {
				t.Errorf("%s.%s must stay nullable-friendly (NOT NULL would break legacy rows)", table, col)
			}
		}
	}

	// A legacy row inserted with only pre-existing columns must read back as
	// text/chat (all flags 0) — the "default 0 keeps existing rows classified"
	// promise from the migration comments.
	if _, err := d.Exec(`INSERT INTO models_catalog(id,provider,name) VALUES('legacy/model','legacy','Legacy')`); err != nil {
		t.Fatal(err)
	}
	var decision, image, video int
	if err := d.QueryRow(`SELECT decision, image, video FROM models_catalog WHERE id='legacy/model'`).
		Scan(&decision, &image, &video); err != nil {
		t.Fatal(err)
	}
	if decision != 0 || image != 0 || video != 0 {
		t.Fatalf("legacy row flags = %d/%d/%d, want 0/0/0", decision, image, video)
	}

	// video_jobs exists with the columns the proxy scans.
	cols := []string{"id", "provider_id", "key_prefix", "dialect", "model", "upstream_id",
		"prompt", "seconds", "size", "status", "progress", "error_code", "error_message",
		"video_url", "created_at", "updated_at", "completed_at", "expires_at"}
	for _, c := range cols {
		if _, _, _, err := columnInfo(t, d, "video_jobs", c); err != nil {
			t.Errorf("video_jobs.%s missing: %v", c, err)
		}
	}
}

// columnInfo reads PRAGMA table_info for one column.
func columnInfo(t *testing.T, d *sql.DB, table, col string) (typ, dflt string, notnull bool, err error) {
	t.Helper()
	rows, qerr := d.Query(`PRAGMA table_info(` + table + `)`)
	if qerr != nil {
		return "", "", false, qerr
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name string
		var ctype string
		var nn int
		var dvalue sql.NullString
		var pk int
		if e := rows.Scan(&cid, &name, &ctype, &nn, &dvalue, &pk); e != nil {
			return "", "", false, e
		}
		if name == col {
			found = true
			typ, notnull = ctype, nn != 0
			dflt = dvalue.String
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", false, err
	}
	if !found {
		return "", "", false, sql.ErrNoRows
	}
	return typ, dflt, notnull, nil
}

// TestMigrateIsIdempotent proves a second boot does not re-apply anything and
// does not error (the container restart path).
func TestMigrateIsIdempotent(t *testing.T) {
	path := freshSQLiteFile(t)
	d := openAt(t, path)
	if err := Migrate(d); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if want := LatestMigrationVersion(); n != want {
		t.Fatalf("migrations after re-run = %d, want %d", n, want)
	}
}

// TestDirtyMigrationAbortsBoot proves the dirty flag is honored for the NEWEST
// migration too (not just the legacy hardening ones): the gateway must refuse
// to start rather than skip a partially-applied schema change.
func TestDirtyMigrationAbortsBoot(t *testing.T) {
	d := openAt(t, freshSQLiteFile(t))
	latest := LatestMigrationVersion()
	if _, err := d.Exec(`UPDATE schema_migrations SET dirty=1 WHERE version=?`, latest); err != nil {
		t.Fatal(err)
	}
	err := Migrate(d)
	if err == nil {
		t.Fatalf("Migrate with dirty version %d returned nil, want an abort", latest)
	}
	if !contains(err.Error(), "marked dirty") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !contains(err.Error(), itoa(latest)) {
		t.Fatalf("error does not name the dirty version %d: %v", latest, err)
	}
}

// TestPartialReplayAppliesPendingMigration proves the version ledger drives
// incremental application: deleting only the 029 row (simulating a database
// created by the previous release) makes the next boot apply 029 alone.
func TestPartialReplayAppliesPendingMigration(t *testing.T) {
	path := freshSQLiteFile(t)
	d := openAt(t, path)
	// DROP COLUMN cannot drop `video` while its index exists, so drop the index
	// first — mirroring a pre-029 schema.
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS idx_models_catalog_video`,
		`ALTER TABLE models_catalog DROP COLUMN video`,
		`ALTER TABLE provider_models DROP COLUMN video`,
		`DELETE FROM schema_migrations WHERE version=29`,
	} {
		if _, err := d.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	d.Close()

	d2 := openAt(t, path)
	if _, _, _, err := columnInfo(t, d2, "models_catalog", "video"); err != nil {
		t.Fatalf("029 was not re-applied: %v", err)
	}
	var dirty bool
	if err := d2.QueryRow(`SELECT COALESCE(dirty,0) FROM schema_migrations WHERE version=29`).Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("029 recorded dirty after a successful replay")
	}
}

// TestBoolIntRoundTrip pins the dialect helper itself: BoolInt must map to the
// 0/1 the INTEGER columns hold, and SQLite must read it back as an int.
// (The Postgres half — where a Go bool is rejected outright by lib/pq — lives
// in internal/db/pgsmoke.)
func TestBoolIntRoundTrip(t *testing.T) {
	if BoolInt(true) != 1 || BoolInt(false) != 0 {
		t.Fatalf("BoolInt(true)=%d BoolInt(false)=%d", BoolInt(true), BoolInt(false))
	}
	d := openAt(t, freshSQLiteFile(t))
	if _, err := d.Exec(`INSERT INTO models_catalog(id,provider,name,decision,image,video) VALUES(?,?,?,?,?,?)`,
		"boolint/model", "test", "BoolInt", BoolInt(false), BoolInt(false), BoolInt(true)); err != nil {
		t.Fatal(err)
	}
	var decision, image, video int
	if err := d.QueryRow(`SELECT decision, image, video FROM models_catalog WHERE id='boolint/model'`).
		Scan(&decision, &image, &video); err != nil {
		t.Fatal(err)
	}
	if decision != 0 || image != 0 || video != 1 {
		t.Fatalf("round-trip = %d/%d/%d, want 0/0/1", decision, image, video)
	}
	// Predicates the discovery/list paths rely on must work on the stored ints.
	// The predicate the capability listings use must see the row as video-only.
	var onlyVideo int
	if err := d.QueryRow(`SELECT COUNT(*) FROM models_catalog WHERE id='boolint/model' AND video=1 AND image=0 AND decision=0`).Scan(&onlyVideo); err != nil {
		t.Fatal(err)
	}
	if onlyVideo != 1 {
		t.Fatalf("video-only predicate matched %d rows, want 1", onlyVideo)
	}
}

// TestPostgresCapabilityDDLTranslates proves pgTranslateDDL keeps the new
// capability columns INTEGER on Postgres. A translation to BOOLEAN (or a
// dropped DEFAULT 0) would break the db.BoolInt writes and the sql.NullInt64
// reads that every capability path uses.
func TestPostgresCapabilityDDLTranslates(t *testing.T) {
	cases := []struct{ file, column string }{
		{"migrations/026_decision_models.sql", "decision"},
		{"migrations/027_image_models.sql", "image"},
		{"migrations/029_video_models.sql", "video"},
	}
	for _, c := range cases {
		raw, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		out := pgTranslateDDL(string(raw))
		addCol := "ADD COLUMN " + c.column + " INTEGER DEFAULT 0"
		if indexOf(out, addCol) < 0 {
			t.Errorf("%s: %q lost on the Postgres translation:\n%s", c.file, addCol, out)
		}
		if indexOf(out, "ADD COLUMN "+c.column+" BOOLEAN") >= 0 {
			t.Errorf("%s: %s was translated to BOOLEAN", c.file, c.column)
		}
		// The capability indexes must survive too.
		if indexOf(out, "idx_models_catalog_"+c.column) < 0 {
			t.Errorf("%s: capability index dropped", c.file)
		}
	}

	// 028 keeps `status NOT NULL DEFAULT 'queued'` and DATETIME -> TIMESTAMPTZ.
	raw, err := os.ReadFile("migrations/028_video_jobs.sql")
	if err != nil {
		t.Fatal(err)
	}
	out := pgTranslateDDL(string(raw))
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS video_jobs",
		"status        TEXT NOT NULL DEFAULT 'queued'",
		"idx_video_jobs_key",
		"idx_video_jobs_provider",
		"idx_video_jobs_created",
	} {
		if indexOf(out, want) < 0 {
			t.Errorf("028: %q missing after translation:\n%s", want, out)
		}
	}
	if indexOf(out, "TIMESTAMPTZ") < 0 {
		t.Errorf("028: timestamps not translated to TIMESTAMPTZ:\n%s", out)
	}
}

// TestLatestMigrationVersionIsMonotonic guards the introspection helper the
// boot path and pgsmoke both depend on.
func TestLatestMigrationVersionIsMonotonic(t *testing.T) {
	ms := migrationsList()
	if len(ms) == 0 {
		t.Fatal("no migrations")
	}
	for i := 1; i < len(ms); i++ {
		if ms[i].version != ms[i-1].version+1 {
			t.Fatalf("migration versions not contiguous at index %d: %d -> %d", i, ms[i-1].version, ms[i].version)
		}
	}
	if LatestMigrationVersion() != ms[len(ms)-1].version {
		t.Fatalf("LatestMigrationVersion = %d, want %d", LatestMigrationVersion(), ms[len(ms)-1].version)
	}
}

func contains(s, sub string) bool { return indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	n := len(sub)
	if n == 0 {
		return 0
	}
	for i := 0; i+n <= len(s); i++ {
		if s[i:i+n] == sub {
			return i
		}
	}
	return -1
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
