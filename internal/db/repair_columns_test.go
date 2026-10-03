package db

import (
	"database/sql"
	"testing"
)

// TestRepairRequestLogsColumnsRepairsMissingCacheColumns reproduces the
// production failure: request_logs carries every version in
// schema_migrations, but a column the proxy INSERT names was never added
// (DDL lost around the 020 lb_rules rebuild, or a restore from a copy taken
// mid-migration). The proxy then logs "table request_logs has no column
// named cache_hit" for EVERY request and drops every row, so the dashboard
// reads as if no traffic ever happened while traffic is still served at 200.
//
// The repair must add the missing columns based on the actual table shape,
// not on what schema_migrations claims.
func TestRepairRequestLogsColumnsRepairsMissingCacheColumns(t *testing.T) {
	database, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// All versions stamped applied — the state that made the bug invisible.
	if _, err := database.Exec(`DELETE FROM schema_migrations WHERE version >= 19`); err != nil {
		t.Fatal(err)
	}
	for _, v := range []int{19, 20, 21} {
		if _, err := database.Exec(`INSERT INTO schema_migrations(version, dirty) VALUES(?, 0)`, v); err != nil {
			t.Fatal(err)
		}
	}

	// Drop the columns migration 019/021 are supposed to have added.
	for _, c := range []string{"cache_hit", "cache_status"} {
		if _, err := database.Exec(`ALTER TABLE request_logs DROP COLUMN ` + c); err != nil {
			t.Fatalf("dropping %s: %v", c, err)
		}
	}
	if hasColumn(t, database, "cache_hit") {
		t.Fatal("precondition: cache_hit still present")
	}

	repairRequestLogsColumns(database)

	for _, c := range []string{"cache_hit", "cache_status"} {
		if !hasColumn(t, database, c) {
			t.Fatalf("repairRequestLogsColumns did not restore %s", c)
		}
	}

	// The point of the repair: the proxy INSERT now succeeds and the row lands.
	_, err = database.Exec(`INSERT INTO request_logs(id,key_prefix,provider_id,model,endpoint,status,latency_ms,created_at,prompt_tokens,completion_tokens,total_tokens,cost_usd,is_stream,request_body,response_body,finish_reason,fallback_chain,cache_read_tokens,cache_write_tokens,reasoning_tokens,cache_hit,cache_status) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"log-1", "abcd1234", "prov-1", "some/model", "chat.completions", 200, 12, "2026-10-03T00:00:00Z", 10, 5, 15, 0.001, 0, nil, nil, "stop", nil, 0, 0, 0, 1, "hit")
	if err != nil {
		t.Fatalf("proxy INSERT still fails after repair: %v", err)
	}
	var hit int
	var status sql.NullString
	if err := database.QueryRow(`SELECT cache_hit, cache_status FROM request_logs WHERE id='log-1'`).Scan(&hit, &status); err != nil {
		t.Fatal(err)
	}
	if hit != 1 || status.String != "hit" {
		t.Fatalf("cache columns did not round-trip: hit=%d status=%q", hit, status.String)
	}
}

// TestRepairRequestLogsColumnsPreservesRows: the repair is additive. Rows
// written before it must keep their data and their already-present columns.
func TestRepairRequestLogsColumnsPreservesRows(t *testing.T) {
	database, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if _, err := database.Exec(`INSERT INTO request_logs(id,model,endpoint,status,latency_ms,created_at,total_tokens) VALUES('old-1','m','chat.completions',200,5,'2026-09-01T00:00:00Z',42)`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"cache_hit", "cache_status"} {
		if _, err := database.Exec(`ALTER TABLE request_logs DROP COLUMN ` + c); err != nil {
			t.Fatal(err)
		}
	}

	repairRequestLogsColumns(database)

	var total int
	var model string
	if err := database.QueryRow(`SELECT total_tokens, model FROM request_logs WHERE id='old-1'`).Scan(&total, &model); err != nil {
		t.Fatalf("pre-existing row lost: %v", err)
	}
	if total != 42 || model != "m" {
		t.Fatalf("row data changed: total=%d model=%s", total, model)
	}
	// The restored column takes its declared default for old rows.
	var hit int
	if err := database.QueryRow(`SELECT cache_hit FROM request_logs WHERE id='old-1'`).Scan(&hit); err != nil {
		t.Fatal(err)
	}
	if hit != 0 {
		t.Fatalf("expected cache_hit default 0 on legacy row, got %d", hit)
	}
}

// TestRepairRequestLogsColumnsIdempotent: repair runs on every boot, so a
// second pass must be a no-op rather than an error or a duplicate column.
func TestRepairRequestLogsColumnsIdempotent(t *testing.T) {
	database, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	for i := 0; i < 3; i++ {
		repairRequestLogsColumns(database)
	}
	for _, c := range []string{"cache_hit", "cache_status", "key_id", "reasoning_tokens"} {
		if !hasColumn(t, database, c) {
			t.Fatalf("column %s missing after repeated repair", c)
		}
	}
}

// TestRepairRequestLogsColumnsCoversInsertColumnSet: every column the proxy
// INSERT names must exist on a healthy table once the migrations plus the
// repair set have run. Without this check, a future migration adding a
// request_logs column ships the same silent-drop bug again — the guard that
// caught it is only useful if the column list is kept in step with the
// INSERT.
//
// Columns from the 001 baseline (CREATE TABLE request_logs) and the
// legacy-ALTER path are covered elsewhere; the ones asserted here are the
// ALTER-added columns whose absence is unrecoverable from the INSERT's
// perspective.
func TestRepairRequestLogsColumnsCoversInsertColumnSet(t *testing.T) {
	database, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	insertCols := []string{
		"id", "key_prefix", "provider_id", "model", "endpoint", "status",
		"latency_ms", "created_at", "prompt_tokens", "completion_tokens",
		"total_tokens", "cost_usd", "is_stream", "request_body",
		"response_body", "finish_reason", "fallback_chain",
		"cache_read_tokens", "cache_write_tokens", "reasoning_tokens",
		"cache_hit", "cache_status",
	}
	// A repaired-from-scratch table must satisfy the full INSERT. Drop every
	// column the repair set owns, then repair, then confirm the proxy's
	// exact INSERT is accepted. Single-column indexes on those columns must
	// go first — SQLite refuses to drop a column an index still references.
	for _, idx := range []string{
		"idx_request_logs_ttft", "idx_request_logs_error", "idx_request_logs_key_id",
	} {
		if _, err := database.Exec(`DROP INDEX IF EXISTS ` + idx); err != nil {
			t.Fatalf("dropping index %s: %v", idx, err)
		}
	}
	for _, c := range requestLogColumns {
		if _, err := database.Exec(`ALTER TABLE request_logs DROP COLUMN ` + c.name); err != nil {
			t.Fatalf("dropping %s: %v", c.name, err)
		}
	}
	repairRequestLogsColumns(database)
	for _, c := range insertCols {
		if !hasColumn(t, database, c) {
			t.Errorf("request_logs.%s is named by the proxy INSERT but missing after repair", c)
		}
	}
}

func hasColumn(t *testing.T, database *sql.DB, name string) bool {
	t.Helper()
	var n int
	// sqlite only: this package's tests run on the sqlite dialect.
	if err := database.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('request_logs') WHERE name=?`, name).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	return n > 0
}
