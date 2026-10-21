// Package pgsmoke verifies the gateway schema and dialect helpers against a
// real Postgres. The suite shares ONE database across runs and uses fixed
// identifiers, so TestMain truncates the data tables once per run — otherwise a
// second run fails on primary-key and unique-name collisions and the suite can
// only be run against a freshly created database.
package pgsmoke

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"ai-gateway/internal/db"
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("GATEWAY_PG_DSN")
	if dsn == "" {
		// No Postgres configured: let the tests run and skip themselves.
		os.Exit(m.Run())
	}
	if err := resetDatabase(dsn); err != nil {
		fmt.Fprintf(os.Stderr, "pgsmoke: could not reset the test database: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// resetDatabase migrates, then truncates every table except the migration
// ledger so each run starts from an empty but fully migrated schema.
//
// The ledger is kept (that is what makes the TRUNCATE cheap), which is exactly
// why the schema version is verified first: a long-lived "gwtest" database from
// an older release would otherwise be truncated but never migrated, and the
// whole suite would run — and pass — against a schema several migrations behind
// the binary. That was P2-16: pgsmoke false greens.
//
// When MAX(schema_migrations.version) does not match db.LatestMigrationVersion()
// the public schema is dropped and recreated, so the following db.Open applies
// every migration from scratch. The database must be dedicated to the tests
// (documented in the package comment and by the DSN name "gwtest"); DROP SCHEMA
// public CASCADE is destructive by design here and nowhere else.
func resetDatabase(dsn string) error {
	os.Setenv("DATABASE_URL", dsn)
	database, err := db.Open(dsn)
	if err != nil {
		return err
	}
	defer database.Close()

	if dropped, err := ensureSchemaVersion(database); err != nil {
		return err
	} else if dropped {
		// Re-open so Migrate runs against the empty schema.
		database.Close()
		database, err = db.Open(dsn)
		if err != nil {
			return err
		}
		defer database.Close()
	}

	rows, err := database.Query(`SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename <> 'schema_migrations'`)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, `"`+t+`"`)
	}
	rows.Close()
	if len(tables) == 0 {
		return nil
	}
	// One statement with CASCADE so foreign keys do not dictate an order.
	return execTruncate(database, tables)
}

// ensureSchemaVersion reports whether it had to drop and recreate the public
// schema because the database was migrated by an older (or newer) binary.
func ensureSchemaVersion(database *sql.DB) (bool, error) {
	want := db.LatestMigrationVersion()
	var got sql.NullInt64
	// schema_migrations may legitimately be empty (or, on a brand-new database,
	// absent); db.Open's Migrate creates/populates it first, so a NULL max means
	// "nothing recorded" and always triggers the rebuild.
	err := database.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&got)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if got.Valid && int(got.Int64) == want {
		return false, nil
	}
	if _, err := database.Exec(`DROP SCHEMA public CASCADE`); err != nil {
		return false, fmt.Errorf("drop schema public (schema at %v, binary expects %d): %w", got, want, err)
	}
	if _, err := database.Exec(`CREATE SCHEMA public`); err != nil {
		return false, fmt.Errorf("recreate schema public: %w", err)
	}
	fmt.Fprintf(os.Stderr, "pgsmoke: schema was at version %v but the binary expects %d; dropped and recreated the public schema\n", sqlVersion(got), want)
	return true, nil
}

func sqlVersion(v sql.NullInt64) any {
	if !v.Valid {
		return "<none>"
	}
	return v.Int64
}

func execTruncate(database *sql.DB, tables []string) error {
	stmt := "TRUNCATE TABLE " + strings.Join(tables, ", ") + " RESTART IDENTITY CASCADE"
	_, err := database.Exec(stmt)
	return err
}
