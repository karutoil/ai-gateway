package pgsmoke

// Schema assertions for the NEW surfaces against a real Postgres. These are the
// half of the migration story SQLite cannot prove: the capability columns must
// be INTEGER (not BOOLEAN) on Postgres, or every db.BoolInt write fails with
//
//	pq: invalid input syntax for type integer: "false"
//
// and the video_jobs timestamps must be TIMESTAMPTZ so scanVideoJob can read
// them into time.Time. Skipped unless GATEWAY_PG_DSN is set.

import (
	"database/sql"
	"testing"
	"time"

	"ai-gateway/internal/db"
)

// columnType returns the Postgres data_type for table.column.
func columnType(t *testing.T, database *sql.DB, table, column string) string {
	t.Helper()
	var typ sql.NullString
	err := database.QueryRow(`
		SELECT data_type FROM information_schema.columns
		WHERE table_schema='public' AND table_name=$1 AND column_name=$2`,
		table, column).Scan(&typ)
	if err == sql.ErrNoRows || !typ.Valid {
		t.Fatalf("%s.%s does not exist", table, column)
	}
	if err != nil {
		t.Fatalf("%s.%s: %v", table, column, err)
	}
	return typ.String
}

// TestCapabilityColumnsAreIntegerOnPostgres pins the affinity the Go layer
// depends on. A BOOLEAN column here silently breaks db.BoolInt writes.
func TestCapabilityColumnsAreIntegerOnPostgres(t *testing.T) {
	database := openPG(t)
	for _, table := range []string{"models_catalog", "provider_models"} {
		for _, col := range []string{"decision", "image", "video"} {
			if got := columnType(t, database, table, col); got != "integer" {
				t.Errorf("%s.%s is %q on Postgres, want integer", table, col, got)
			}
		}
	}
}

// TestVideoJobsSchemaOnPostgres proves migration 028 produced the table the
// proxy scans, with the column types scanVideoJob expects.
func TestVideoJobsSchemaOnPostgres(t *testing.T) {
	database := openPG(t)

	want := map[string]string{
		"id":            "text",
		"provider_id":   "text",
		"key_prefix":    "text",
		"dialect":       "text",
		"model":         "text",
		"upstream_id":   "text",
		"prompt":        "text",
		"seconds":       "text",
		"size":          "text",
		"status":        "text",
		"progress":      "integer",
		"error_code":    "text",
		"error_message": "text",
		"video_url":     "text",
		"created_at":    "timestamp with time zone",
		"updated_at":    "timestamp with time zone",
		"completed_at":  "timestamp with time zone",
		"expires_at":    "timestamp with time zone",
	}
	for col, typ := range want {
		if got := columnType(t, database, "video_jobs", col); got != typ {
			t.Errorf("video_jobs.%s is %q, want %q", col, got, typ)
		}
	}

	// The ownership index must exist: every list/get path filters on key_prefix.
	// Migration 030 added the covering (key_prefix, created_at DESC) variant that
	// the list query actually needs; the original single-column index is kept.
	for _, name := range []string{"idx_video_jobs_key", "idx_video_jobs_key_created"} {
		var idx int
		if err := database.QueryRow(`SELECT COUNT(*) FROM pg_indexes WHERE tablename='video_jobs' AND indexname=$1`, name).Scan(&idx); err != nil {
			t.Fatal(err)
		}
		if idx != 1 {
			t.Errorf("%s missing: cross-key listing would be a table scan", name)
		}
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	// A full round-trip through the columns the proxy writes, including the
	// nullable timestamps, proves the DATETIME->TIMESTAMPTZ translation works.
	if _, err := database.Exec(db.Q(`INSERT INTO video_jobs
		(id, provider_id, key_prefix, dialect, model, upstream_id, prompt, seconds, size, status, progress, created_at, updated_at, completed_at, expires_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		"video_pg1", "p1", "sk-gw-test", "openai", "sora-2", "up_v1", "a cat", "8", "1280x720",
		"completed", 100, now, now, now, now); err != nil {
		t.Fatalf("insert video_jobs: %v", err)
	}
	var status string
	var progress int
	var completed, expires sql.NullTime
	if err := database.QueryRow(db.Q(`SELECT status, progress, completed_at, expires_at FROM video_jobs WHERE id=?`), "video_pg1").
		Scan(&status, &progress, &completed, &expires); err != nil {
		t.Fatalf("scan video_jobs: %v", err)
	}
	if status != "completed" || progress != 100 || !completed.Valid || !expires.Valid {
		t.Fatalf("video_jobs round-trip = %q/%d completed=%v expires=%v", status, progress, completed.Valid, expires.Valid)
	}
}
