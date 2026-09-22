package main

// Copy SQLite production data into Postgres (schema already migrated:
// boot the gateway once against the target first). Each table copies in one
// transaction with ON CONFLICT DO NOTHING (re-runnable for delta passes).
// schema_migrations is intentionally skipped — the target stamps its own.
// Usage: go run ./scripts/migrate-sqlite-pg.go <sqlite-path> <postgres-url>
import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
)

var tables = []string{
	"organizations",
	"dashboard_users",
	"providers",
	"gateway_keys",
	"models_catalog",
	"model_aliases",
	"system_config",
	"audit_logs",
	"response_turns",
	"request_logs",
	"memberships",
	"lb_rules",
	"provider_models",
	"personal_access_tokens",
	"webhooks",
	"user_permissions",
	"webauthn_credentials",
	"recovery_codes",
	"spend_counters",
}

func main() {
	if len(os.Args) != 3 && !(len(os.Args) == 4 && os.Args[3] == "verify") {
		fmt.Println("usage: migrate-sqlite-pg.go <sqlite-path> <postgres-url> [verify]")
		fmt.Println("  copy (default) then verify counts; 'verify' alone only compares")
		os.Exit(2)
	}
	verifyOnly := len(os.Args) == 4
	src, err := sql.Open("sqlite3", os.Args[1])
	if err != nil {
		panic(err)
	}
	defer src.Close()
	dst, err := sql.Open("postgres", os.Args[2])
	if err != nil {
		panic(err)
	}
	defer dst.Close()
	if verifyOnly {
		os.Exit(verifyCounts(src, dst))
	}
	if _, err := dst.Exec("SET session_replication_role='replica'"); err != nil {
		fmt.Println("warn: cannot disable FK checks:", err)
	}
	for _, t := range tables {
		var exists string
		if err := src.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, t).Scan(&exists); err != nil {
			fmt.Printf("%s: missing in sqlite, skip\n", t)
			continue
		}
		colsRows, err := src.Query(fmt.Sprintf(`PRAGMA table_info("%s")`, t))
		if err != nil {
			fmt.Printf("%s: pragma fail %v\n", t, err)
			continue
		}
		var cols []string
		for colsRows.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt sql.NullString
			if err := colsRows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil {
				cols = append(cols, name)
			}
		}
		colsRows.Close()
		if len(cols) == 0 {
			continue
		}
		// pg boolean columns for 0/1 conversion
		boolCols := map[string]bool{}
		pgRows, err := dst.Query(`SELECT column_name FROM information_schema.columns WHERE table_name=$1 AND data_type='boolean'`, t)
		if err == nil {
			for pgRows.Next() {
				var c string
				if pgRows.Scan(&c) == nil {
					boolCols[c] = true
				}
			}
			pgRows.Close()
		}
		sel, err := src.Query(fmt.Sprintf(`SELECT "%s" FROM "%s"`, strings.Join(cols, `","`), t))
		if err != nil {
			fmt.Printf("%s: select fail %v\n", t, err)
			continue
		}
		place := make([]string, len(cols))
		for i := range cols {
			place[i] = fmt.Sprintf("$%d", i+1)
		}
		stmt := fmt.Sprintf(`INSERT INTO "%s" ("%s") VALUES (%s) ON CONFLICT DO NOTHING`,
			t, strings.Join(cols, `","`), strings.Join(place, ","))
		tx, err := dst.Begin()
		if err != nil {
			fmt.Printf("%s: begin fail %v\n", t, err)
			sel.Close()
			continue
		}
		istmt, err := tx.Prepare(stmt)
		if err != nil {
			fmt.Printf("%s: prepare fail %v\n", t, err)
			sel.Close()
			_ = tx.Rollback()
			continue
		}
		copied := 0
		failed := false
		for sel.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := sel.Scan(ptrs...); err != nil {
				fmt.Printf("%s: scan fail %v\n", t, err)
				failed = true
				break
			}
			for i, c := range cols {
				if !boolCols[c] {
					continue
				}
				switch v := vals[i].(type) {
				case int64:
					vals[i] = v != 0
				case float64:
					vals[i] = v != 0
				case []byte:
					s := string(v)
					vals[i] = !(s == "0" || s == "" || s == "false" || s == "f")
				}
			}
			if _, err := istmt.Exec(vals...); err != nil {
				fmt.Printf("%s: insert fail %v\n", t, err)
				failed = true
				break
			}
			copied++
		}
		sel.Close()
		_ = istmt.Close()
		if failed {
			_ = tx.Rollback()
			fmt.Printf("%s: ROLLED BACK after %d rows\n", t, copied)
			continue
		}
		if err := tx.Commit(); err != nil {
			fmt.Printf("%s: commit fail %v\n", t, err)
			continue
		}
		fmt.Printf("%s: copied %d rows\n", t, copied)
	}
	fmt.Println("---- verifying ----")
	os.Exit(verifyCounts(src, dst))
}

// verifyCounts compares per-table row counts; exit 0 when every source row
// is present on the target (target may hold extra rows, e.g. a fresher
// models.dev catalog sync — reported, not failed).
func verifyCounts(src, dst *sql.DB) int {
	bad := 0
	for _, t := range tables {
		var srcName string
		if err := src.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, t).Scan(&srcName); err != nil {
			fmt.Printf("%-22s absent in sqlite, nothing to migrate\n", t)
			continue
		}
		var a, b int
		if err := src.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM "%s"`, t)).Scan(&a); err != nil {
			fmt.Printf("%-22s sqlite unreadable: %v\n", t, err)
			bad++
			continue
		}
		if err := dst.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM "%s"`, t)).Scan(&b); err != nil {
			fmt.Printf("%-22s pg unreadable: %v\n", t, err)
			bad++
			continue
		}
		status := "ok"
		if b < a {
			status = "MISSING ROWS"
			bad++
		} else if b > a {
			status = "ok (target has extra rows)"
		}
		fmt.Printf("%-22s sqlite=%-7d pg=%-7d %s\n", t, a, b, status)
	}
	if bad > 0 {
		fmt.Printf("%d TABLE(S) NEED ATTENTION\n", bad)
		return 1
	}
	fmt.Println("VERIFY PASSED")
	return 0
}
