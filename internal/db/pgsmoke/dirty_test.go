package pgsmoke

import (
	"os"
	"strings"
	"testing"

	"ai-gateway/internal/db"
)

// TestFailedMigrationMarkedDirtyOnPostgres proves the migration failure path
// writes the dirty flag on the Postgres dialect too. The upsert generated for
// Postgres runs through Rebind (`VALUES($1, FALSE)`) and BoolLit, so a missing
// version bind or a malformed dialect rendering would leave the failed version
// unrecorded and make the next boot silently retry it instead of aborting with
// "marked dirty".
func TestFailedMigrationMarkedDirtyOnPostgres(t *testing.T) {
	dsn := os.Getenv("GATEWAY_PG_DSN")
	if dsn == "" {
		t.Skip("GATEWAY_PG_DSN not set; skipping Postgres smoke test")
	}
	t.Setenv("DATABASE_URL", dsn)
	database, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("open/migrate: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	// Break the schema so the replay of migration 20 fails genuinely:
	// lb_rules_v2 exists but lacks the columns its INSERT ... SELECT targets.
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS lb_rules_v2`,
		`CREATE TABLE lb_rules_v2 (id TEXT PRIMARY KEY)`,
		`DELETE FROM schema_migrations`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	err = db.Migrate(database)
	if err == nil {
		t.Fatal("expected the broken schema to fail Migrate")
	}
	if !strings.Contains(err.Error(), "migration 20") {
		t.Fatalf("error should name migration 20: %v", err)
	}

	var dirty bool
	if qerr := database.QueryRow(`SELECT COALESCE(dirty,FALSE) FROM schema_migrations WHERE version=20`).Scan(&dirty); qerr != nil {
		t.Fatalf("failed migration 20 not recorded in schema_migrations: %v", qerr)
	}
	if !dirty {
		t.Fatal("migration 20 failed but is not marked dirty; next boot would retry it silently")
	}

	err = db.Migrate(database)
	if err == nil || !strings.Contains(err.Error(), "marked dirty") {
		t.Fatalf("Migrate must abort on the dirty version, got: %v", err)
	}

	// Restore a consistent ledger so later tests in this package migrate cleanly.
	for _, stmt := range []string{
		`DELETE FROM schema_migrations WHERE version=20`,
		`DROP TABLE lb_rules_v2`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("cleanup %s: %v", stmt, err)
		}
	}
	// Replay the whole set against the restored schema; every remaining
	// statement is either applied already (tolerated) or re-applies cleanly.
	if err := db.Migrate(database); err != nil {
		t.Fatalf("post-failure replay: %v", err)
	}
}
