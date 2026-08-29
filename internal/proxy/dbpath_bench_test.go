package proxy

// Diagnostic micro-benchmark: cost of each per-request SQLite operation on
// the hot path (measured on this repo, 2026-08). Run with:
//
//	go test ./internal/proxy -bench BenchmarkDBPathPieces -benchtime=0.5s

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
)

func BenchmarkDBPathPieces(b *testing.B) {
	database, err := db.Open(filepath.Join(b.TempDir(), "micro.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer database.Close()

	ks := apikey.NewStore(database)
	k, err := ks.Create("micro")
	if err != nil {
		b.Fatal(err)
	}
	_ = k

	b.Run("Verify(key lookup)", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, ok := ks.Verify(k.Key); !ok {
				b.Fatal("verify failed")
			}
		}
	})

	b.Run("AliasLookup", func(b *testing.B) {
		b.ReportAllocs()
		var target string
		for i := 0; i < b.N; i++ {
			if err := database.QueryRow(db.Q(`SELECT target FROM model_aliases WHERE alias=?`), "gpt-4o-mini").Scan(&target); err == nil {
				b.Fatal("unexpected row")
			}
		}
	})

	b.Run("ProviderResolve", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			rows, err := database.Query(db.Q(`SELECT id FROM providers LIMIT 1`))
			if err != nil {
				b.Fatal(err)
			}
			rows.Close() // single-conn SQLite: an unclosed rows holds the only connection
		}
	})

	b.Run("RequestLogInsert", func(b *testing.B) {
		b.ReportAllocs()
		// The parent benchmark re-runs sub-benchmarks with growing b.N, so IDs
		// must be unique across runs.
		pfx := fmt.Sprintf("bench-%d", time.Now().UnixNano())
		for i := 0; i < b.N; i++ {
			if _, err := database.Exec(db.Q(`INSERT INTO request_logs(id,key_prefix,provider_id,model,endpoint,status,latency_ms,created_at,prompt_tokens,completion_tokens,total_tokens,cost_usd,is_stream) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`),
				fmt.Sprintf("%s-%d", pfx, i), "pfx", "pid", "m", "chat", 200, 1, "2026-01-01", 10, 10, 20, 0, 0); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("SpendCounterUpsert", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := database.Exec(db.Q(`INSERT INTO spend_counters(scope,period,start_utc,tokens,cost_micros,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(scope,period,start_utc) DO UPDATE SET tokens=tokens+excluded.tokens`),
				"bench", "day", "2026-01-01", 10, 10, "2026-01-01"); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("SpendCounterSnapshot", func(b *testing.B) {
		b.ReportAllocs()
		var tok int64
		for i := 0; i < b.N; i++ {
			if err := database.QueryRow(db.Q(`SELECT COALESCE(tokens,0) FROM spend_counters WHERE scope=? AND period=? AND start_utc=?`), "bench", "day", "2026-01-01").Scan(&tok); err != nil {
				b.Fatal(err)
			}
		}
	})
}
