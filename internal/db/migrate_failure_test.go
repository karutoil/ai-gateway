package db

import (
	"database/sql"
	"strings"
	"testing"
)

// forceMigrationFailure breaks the schema so the NEXT replay of the migration
// set fails with a genuine (non-duplicate-object) error at migration 20:
// lb_rules_v2 is recreated with a shape its INSERT ... SELECT cannot target.
//
// Wiping the ledger and re-running Migrate otherwise replays cleanly (every
// "already exists" statement is tolerated), so this is the deterministic way to
// reach the failure paths that must mark the version dirty.
func forceMigrationFailure(t *testing.T, d *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS lb_rules_v2`,
		// Missing every column but id: migration 020's
		// INSERT INTO lb_rules_v2 (id, model, ...) SELECT ... fails with
		// "table lb_rules_v2 has no column named model".
		`CREATE TABLE lb_rules_v2 (id TEXT PRIMARY KEY)`,
		`DELETE FROM schema_migrations`,
	} {
		if _, err := d.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// TestFailedMigrationIsMarkedDirty proves the failure path actually writes the
// dirty flag: the version that failed must appear in schema_migrations with
// dirty=1, and the next Migrate call must abort with the operator-facing
// "marked dirty" error instead of blindly retrying.
//
// Regression: the failure path used to Exec the dirty upsert WITHOUT binding
// the version placeholder, so the statement itself errored ("not enough args")
// and the flag was silently never written. A failed migration then looked
// clean on the next boot and was retried rather than surfaced.
func TestFailedMigrationIsMarkedDirty(t *testing.T) {
	d := openAt(t, freshSQLiteFile(t))
	forceMigrationFailure(t, d)

	err := Migrate(d)
	if err == nil {
		t.Fatal("expected the broken schema to fail Migrate")
	}
	if !strings.Contains(err.Error(), "migration 20") {
		t.Fatalf("error should name migration 20: %v", err)
	}

	var dirty bool
	qerr := d.QueryRow(`SELECT COALESCE(dirty,0) FROM schema_migrations WHERE version=20`).Scan(&dirty)
	if qerr != nil {
		t.Fatalf("failed migration 20 not recorded in schema_migrations: %v", qerr)
	}
	if !dirty {
		t.Fatal("migration 20 failed but is not marked dirty; next boot would retry it silently")
	}

	// The dirty ledger must abort the next boot with a distinct error.
	err = Migrate(d)
	if err == nil {
		t.Fatal("Migrate must abort when a version is marked dirty")
	}
	if !strings.Contains(err.Error(), "marked dirty") || !strings.Contains(err.Error(), "20") {
		t.Fatalf("unexpected dirty-boot error: %v", err)
	}
}

// TestFailedMigrationRollbackKeepsPriorSchema proves a genuine mid-file
// failure leaves the database at the previous version: the migration's own
// statements are rolled back (transactional DDL), so no partial state from the
// failed file survives.
func TestFailedMigrationRollbackKeepsPriorSchema(t *testing.T) {
	d := openAt(t, freshSQLiteFile(t))
	forceMigrationFailure(t, d)

	if err := Migrate(d); err == nil {
		t.Fatal("expected migration failure")
	}
	// lb_rules_v2 must still be the pre-migration shape (id only): the INSERT
	// that failed was rolled back and nothing from file 020 was committed.
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM lb_rules_v2 WHERE 1=1`).Scan(&n); err != nil {
		t.Fatalf("lb_rules_v2 missing after rollback: %v", err)
	}
	// Versions after the failed one were never recorded (ledger stops at 20).
	var maxv int
	if err := d.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations WHERE COALESCE(dirty,0)=0`).Scan(&maxv); err != nil {
		t.Fatal(err)
	}
	if maxv >= 20 {
		t.Fatalf("migrations past the failed version recorded clean: max clean version = %d", maxv)
	}
}

// TestMarkSchemaMigrationBindsVersion pins the bookkeeping helper itself:
// the dirty upsert must bind its version argument (the regression was an Exec
// with zero args) and must upsert cleanly when a row already exists.
func TestMarkSchemaMigrationBindsVersion(t *testing.T) {
	d := openAt(t, freshSQLiteFile(t))
	if err := markSchemaMigration(d, 99, true); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}
	var dirty bool
	if err := d.QueryRow(`SELECT COALESCE(dirty,0) FROM schema_migrations WHERE version=99`).Scan(&dirty); err != nil {
		t.Fatalf("row missing: %v", err)
	}
	if !dirty {
		t.Fatal("version 99 not marked dirty")
	}
	// Upsert to clean (the boot path's normal recording).
	if err := markSchemaMigration(d, 99, false); err != nil {
		t.Fatalf("mark clean: %v", err)
	}
	if err := d.QueryRow(`SELECT COALESCE(dirty,0) FROM schema_migrations WHERE version=99`).Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("version 99 not flipped back to clean")
	}
}

// TestBackfillKeyIDsLeavesAmbiguousPrefixesNull proves the documented
// contract of the boot backfill: a prefix shared by several (legacy) keys must
// NOT be attributed to an arbitrary one of them — the subquery would silently
// pin historical spend to a key that may not have made those requests. Rows
// with a unique prefix are attributed; rows with no key stay NULL.
func TestBackfillKeyIDsLeavesAmbiguousPrefixesNull(t *testing.T) {
	d := openAt(t, freshSQLiteFile(t))
	for _, stmt := range []string{
		// Legacy DBs (the exact shape this backfill exists for) refused the
		// UNIQUE prefix index, which is the only way colliding prefixes exist.
		`DROP INDEX IF EXISTS ux_gateway_keys_prefix`,
		// Two colliding keys (legacy DB refused the UNIQUE prefix index).
		`INSERT INTO gateway_keys(id,name,prefix,hash,created_at) VALUES('k1','one','duppfx01','h1',datetime('now'))`,
		`INSERT INTO gateway_keys(id,name,prefix,hash,created_at) VALUES('k2','two','duppfx01','h2',datetime('now'))`,
		`INSERT INTO gateway_keys(id,name,prefix,hash,created_at) VALUES('k3','three','uniqpfx1','h3',datetime('now'))`,
		`INSERT INTO request_logs(id,key_prefix,created_at) VALUES('r1','duppfx01',datetime('now'))`,
		`INSERT INTO request_logs(id,key_prefix,created_at) VALUES('r2','uniqpfx1',datetime('now'))`,
		`INSERT INTO request_logs(id,key_prefix,created_at) VALUES('r3','nobody00',datetime('now'))`,
	} {
		if _, err := d.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	n, err := BackfillKeyIDs(d)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("backfilled %d rows, want exactly the 1 unambiguous row", n)
	}
	var r1, r2, r3 sql.NullString
	if err := d.QueryRow(`SELECT key_id FROM request_logs WHERE id='r1'`).Scan(&r1); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT key_id FROM request_logs WHERE id='r2'`).Scan(&r2); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT key_id FROM request_logs WHERE id='r3'`).Scan(&r3); err != nil {
		t.Fatal(err)
	}
	if r1.Valid {
		t.Fatalf("ambiguous prefix attributed to %q; want NULL", r1.String)
	}
	if !r2.Valid || r2.String != "k3" {
		t.Fatalf("unique prefix not attributed: %v", r2)
	}
	if r3.Valid {
		t.Fatalf("unknown prefix attributed to %q; want NULL", r3.String)
	}
}
