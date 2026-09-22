// Package pgsmoke verifies the gateway schema and dialect helpers against a
// real Postgres. It is skipped unless GATEWAY_PG_DSN is set (e.g.
// postgres://postgres@127.0.0.1:5432/gwtest?sslmode=disable); the default
// `go test ./...` run never touches it.
package pgsmoke

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"ai-gateway/internal/db"
)

func openPG(t *testing.T) *sql.DB {
	t.Helper()
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
	if got := db.Dialect(); got != "postgres" {
		t.Fatalf("dialect = %q, want postgres", got)
	}
	return database
}

func TestPostgresMigrations(t *testing.T) {
	database := openPG(t)
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if n != 18 {
		t.Fatalf("migrations = %d, want 18", n)
	}
}

func TestPostgresCRUD(t *testing.T) {
	database := openPG(t)
	now := time.Now().UTC().Truncate(time.Microsecond)

	// providers incl. BYTEA + timestamptz round-trip.
	if _, err := database.Exec(db.Q(`INSERT INTO providers(id,name,type,base_url,api_key_enc,created_at) VALUES(?,?,?,?,?,?)`),
		"p1", "openai", "openai", "https://api.openai.com/v1", []byte{1, 2, 3}, now); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	var blob []byte
	var ts time.Time
	if err := database.QueryRow(db.Q(`SELECT api_key_enc, created_at FROM providers WHERE id=?`), "p1").Scan(&blob, &ts); err != nil {
		t.Fatalf("select provider: %v", err)
	}
	if string(blob) != string([]byte{1, 2, 3}) {
		t.Fatalf("blob round-trip = %v", blob)
	}

	// request_logs incl. BOOLEAN + aggregates + bucket exprs.
	if _, err := database.Exec(db.Q(`INSERT INTO request_logs(id,key_prefix,provider_id,model,endpoint,status,latency_ms,created_at,prompt_tokens,completion_tokens,total_tokens,cost_usd,is_stream) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		"r1", "sk-gw-test", "p1", "gpt-4o", "chat.completions", 200, 123, now, 10, 20, 30, 0.001, true); err != nil {
		t.Fatalf("insert request log: %v", err)
	}
	var isStream bool
	if err := database.QueryRow(db.Q(`SELECT is_stream FROM request_logs WHERE id=?`), "r1").Scan(&isStream); err != nil || !isStream {
		t.Fatalf("bool round-trip: %v (value=%v)", err, isStream)
	}
	var day string
	var sum float64
	var cnt int
	if err := database.QueryRow(db.Q(`SELECT `+db.DateBucketExpr("created_at")+` as day, COALESCE(SUM(cost_usd),0), COUNT(*) FROM request_logs WHERE created_at >= ? GROUP BY `+db.DateBucketExpr("created_at")), now.Add(-24*time.Hour)).Scan(&day, &sum, &cnt); err != nil {
		t.Fatalf("date bucket aggregate: %v", err)
	}
	if err := database.QueryRow(db.Q(`SELECT `+db.HourBucketExpr("created_at")+` FROM request_logs WHERE id=?`), "r1").Scan(&day); err != nil {
		t.Fatalf("hour bucket expr: %v", err)
	}

	// models_catalog BOOLEAN literal compare.
	if _, err := database.Exec(db.Q(`INSERT INTO models_catalog(id,provider,name,reasoning,tool_call,updated_at) VALUES(?,?,?,?,?,?)`),
		"openai/gpt-4o", "openai", "GPT-4o", true, true, now); err != nil {
		t.Fatalf("insert catalog: %v", err)
	}
	var reasoning bool
	if err := database.QueryRow(db.Q(`SELECT reasoning FROM models_catalog WHERE id=? AND reasoning=`+db.BoolLit(true)), "openai/gpt-4o").Scan(&reasoning); err != nil || !reasoning {
		t.Fatalf("bool literal compare: %v", err)
	}

	// INTEGER-flag columns keep numeric comparison on both dialects.
	if _, err := database.Exec(db.Q(`INSERT INTO dashboard_users(id,username,password_hash,role,created_at,updated_at,login_count,passkey_enabled,disabled) VALUES(?,?,?,?,?,?,?,?,?)`),
		"u1", "admin", "x", "admin", now, now, 0, 0, 0); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM dashboard_users WHERE disabled=0`).Scan(&cnt); err != nil {
		t.Fatalf("int-flag compare: %v", err)
	}
	if _, err := database.Exec(db.Q(`INSERT INTO webhooks(id,name,url,created_at,updated_at) VALUES(?,?,?,?,?)`),
		"w1", "w", "https://x.example.com/hook", now, now); err != nil {
		t.Fatalf("insert webhook: %v", err)
	}
	if err := database.QueryRow(db.Q(`SELECT COUNT(*) FROM webhooks WHERE enabled = 1`)).Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("webhook int-flag: %v (count=%d)", err, cnt)
	}

	// Upsert tail shared by both dialects.
	if _, err := database.Exec(db.Q(`INSERT INTO gateway_keys(id,name,prefix,hash,created_at) VALUES(?,?,?,?,?)`+db.UpsertEnd([]string{"id"}, []string{"name"})),
		"k1", "test", "sk-gw-test", "hash1", now); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Concat + ESCAPE LIKE + LIMIT placeholder.
	var mid string
	if err := database.QueryRow(db.Q(`SELECT model_id FROM provider_models WHERE model_id=? OR model_id LIKE '%/'||? ESCAPE '\' LIMIT 1`), "gpt-4o", "gpt-4o").Scan(&mid); err != nil {
		// empty table is fine — the point is the query parses and runs.
		if err != sql.ErrNoRows {
			t.Fatalf("concat-escape-like: %v", err)
		}
	}

	if _, err := db.BackfillKeyIDs(database); err != nil {
		t.Fatalf("backfill: %v", err)
	}
}
