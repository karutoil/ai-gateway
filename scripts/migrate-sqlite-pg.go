package main

// Copy SQLite production data into Postgres (schema already migrated).
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
	"schema_migrations",
	"organizations",
	"dashboard_users",
	"providers",
	"gateway_keys",
	"models_catalog",
	"model_aliases",
	"system_config",
	"audit_logs",
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
	if len(os.Args) != 3 {
		fmt.Println("usage: migrate-sqlite-pg.go <sqlite-path> <postgres-url>")
		os.Exit(2)
	}
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
		copied := 0
		for sel.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := sel.Scan(ptrs...); err != nil {
				fmt.Printf("%s: scan fail %v\n", t, err)
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
			if _, err := dst.Exec(stmt, vals...); err != nil {
				fmt.Printf("%s: insert fail %v\n", t, err)
				break
			}
			copied++
		}
		sel.Close()
		fmt.Printf("%s: copied %d rows\n", t, copied)
	}
}
