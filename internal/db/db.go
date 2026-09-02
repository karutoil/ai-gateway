package db

import (
	"database/sql"
	_ "embed"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
)

//go:embed migrations/001_initial.sql
var migration001SQL string

//go:embed migrations/002_hardening.sql
var migration002SQL string

//go:embed migrations/003_dashboard_rbac_passkey.sql
var migration003SQL string

//go:embed migrations/004_profile_last_login.sql
var migration004SQL string

//go:embed migrations/005_ttft_tps.sql
var migration005SQL string

//go:embed migrations/006_request_error.sql
var migration006SQL string

//go:embed migrations/007_gateway_key_limits.sql
var migration007SQL string

//go:embed migrations/008_production_hardening.sql
var migration008SQL string

//go:embed migrations/009_lb_rules.sql
var migration009SQL string

//go:embed migrations/010_usage_logging.sql
var migration010SQL string

//go:embed migrations/011_routing_strategies.sql
var migration011SQL string

//go:embed migrations/012_key_analytics.sql
var migration012SQL string

//go:embed migrations/013_user_permissions.sql
var migration013SQL string

//go:embed migrations/014_key_rotation.sql
var migration014SQL string

//go:embed migrations/015_key_management.sql
var migration015SQL string

//go:embed migrations/016_webhook_format.sql
var migration016SQL string

// Dialect returns the current SQL dialect based on DATABASE_URL.
// Returns "postgres" when DATABASE_URL starts with postgres:// or postgresql://, otherwise "sqlite".
// Phase 3 uses this to switch migrations and queries; Phase 2.5 keeps sqlite default.
func Dialect() string {
	url := os.Getenv("DATABASE_URL")
	if strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://") {
		return "postgres"
	}
	return "sqlite"
}

// DialectForURL is helper for testing / config injection.
func DialectForURL(url string) string {
	if strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://") {
		return "postgres"
	}
	return "sqlite"
}

// Rebind replaces "?" parameters with "$n" sequentially when Dialect()=="postgres".
// On sqlite it returns the query unchanged. This allows the same query strings to work
// on both dialects.
func Rebind(query string) string {
	if Dialect() != "postgres" {
		return query
	}
	var b strings.Builder
	n := 1
	for _, ch := range query {
		if ch == '?' {
			b.WriteString("$" + strconv.Itoa(n))
			n++
		} else {
			b.WriteRune(ch)
		}
	}
	return b.String()
}

// Q is helper alias for Rebind, for terse call sites: db.Q("SELECT ... WHERE id=?", id)
func Q(query string) string { return Rebind(query) }

// BoolLit renders a boolean literal in the active dialect.
// SQLite BOOLEAN columns store 0/1; Postgres needs TRUE/FALSE and rejects
// `col = 1` against native boolean columns.
func BoolLit(v bool) string {
	if Dialect() == "postgres" {
		if v {
			return "TRUE"
		}
		return "FALSE"
	}
	if v {
		return "1"
	}
	return "0"
}

// UpsertEnd returns the dialect-appropriate tail for an INSERT ... ON CONFLICT
// upsert keyed on conflictCols, updating updateCols. Both SQLite (3.24+) and
// Postgres accept the same `ON CONFLICT(cols) DO UPDATE SET` syntax, which
// replaces the previously SQLite-only `INSERT OR REPLACE` sites.
func UpsertEnd(conflictCols []string, updateCols []string) string {
	var b strings.Builder
	b.WriteString(" ON CONFLICT(")
	b.WriteString(strings.Join(conflictCols, ","))
	b.WriteString(") DO UPDATE SET ")
	for i, c := range updateCols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(c)
		b.WriteString("=excluded.")
		b.WriteString(c)
	}
	return b.String()
}

func Open(path string) (*sql.DB, error) {
	// Phase 3: postgres:// switches driver; otherwise sqlite path handling as before.
	if DialectForURL(path) == "postgres" {
		db, err := sql.Open("postgres", path)
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(10)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(0)
		if err := db.Ping(); err != nil {
			return nil, err
		}
		if err := Migrate(db); err != nil {
			return nil, err
		}
		return db, nil
	}
	// handle in-memory or URI style paths specially (no dir creation)
	if path != ":memory:" && !isURI(path) {
		dir := filepath.Dir(path)
		if dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return nil, err
			}
		}
	}
	dsn := path
	if !containsQuery(path) {
		dsn = path + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on"
	} else {
		// Parse real query params so coincidental substrings can't silently
		// drop WAL/busy_timeout protection, and always force busy_timeout.
		base := path
		params := ""
		if idx := strings.Index(path, "?"); idx != -1 {
			base = path[:idx]
			params = path[idx+1:]
		}
		vals, err := url.ParseQuery(params)
		if err != nil {
			// Unparseable DSN: pass through unchanged rather than guess.
			dsn = path
		} else {
			if vals.Get("_journal_mode") == "" {
				vals.Set("_journal_mode", "WAL")
			}
			if vals.Get("_busy_timeout") == "" {
				vals.Set("_busy_timeout", "5000")
			}
			if vals.Get("_foreign_keys") == "" {
				vals.Set("_foreign_keys", "on")
			}
			dsn = base + "?" + vals.Encode()
		}
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	if err := Migrate(db); err != nil {
		return nil, err
	}
	return db, nil
}

// Migrate applies versioned migrations via schema_migrations while keeping the legacy
// idempotent Migrate fallback for DBs created before 1.6.
func Migrate(db *sql.DB) error {
	// Ensure version table exists first so fresh + legacy DBs both have it.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, dirty BOOLEAN NOT NULL DEFAULT 0)`); err != nil {
		return err
	}

	migrations := []struct {
		version int
		sql     string
	}{
		{1, migration001SQL},
		{2, migration002SQL},
		{3, migration003SQL},
		{4, migration004SQL},
		{5, migration005SQL},
		{6, migration006SQL},
		{7, migration007SQL},
		{8, migration008SQL},
		{9, migration009SQL},
		{10, migration010SQL},
		{11, migration011SQL},
		{12, migration012SQL},
		{13, migration013SQL},
		{14, migration014SQL},
		{15, migration015SQL},
		{16, migration016SQL},
	}

	for _, m := range migrations {
		var cnt int
		var dirty bool
		if err := db.QueryRow(Rebind(`SELECT COUNT(*) FROM schema_migrations WHERE version=?`), m.version).Scan(&cnt); err != nil {
			return err
		}
		if cnt > 0 {
			// A previously-failed ("dirty") migration must not be silently skipped.
			dirtyExpr := "COALESCE(dirty,0)"
			if Dialect() == "postgres" {
				dirtyExpr = "COALESCE(dirty,FALSE)"
			}
			if err := db.QueryRow(Rebind(`SELECT `+dirtyExpr+` FROM schema_migrations WHERE version=?`), m.version).Scan(&dirty); err == nil && dirty {
				return fmt.Errorf("migration %d is marked dirty: inspect the database state, resolve manually, then reset the flag in schema_migrations", m.version)
			}
			continue
		}
		trimmed := strings.TrimSpace(m.sql)
		if trimmed != "" {
			// Run each migration file inside a transaction so a mid-file
			// failure cannot leave partial DDL: SQLite supports transactional
			// DDL; Postgres too. On failure the rollback leaves the DB at the
			// previous version and we mark dirty for the operator.
			tx, txErr := db.Begin()
			if txErr != nil {
				_, _ = db.Exec(upsertSchemaMigration(db, m.version, true))
				return fmt.Errorf("migration %d: begin transaction failed: %w", m.version, txErr)
			}
			if _, err := tx.Exec(trimmed); err != nil {
				tx.Rollback()
				_, _ = db.Exec(upsertSchemaMigration(db, m.version, true))
				return fmt.Errorf("migration %d failed: %w", m.version, err)
			}
			if err := tx.Commit(); err != nil {
				_, _ = db.Exec(upsertSchemaMigration(db, m.version, true))
				return fmt.Errorf("migration %d commit failed: %w", m.version, err)
			}
		}
		// Run idempotent ALTERs for hardening (budget columns etc). Ignores duplicate-column errors.
		if m.version == 2 {
			applyHardeningAlters(db)
		}
		if m.version == 8 {
			applyHardeningV2Alters(db)
		}
		if _, err := db.Exec(upsertSchemaMigration(db, m.version, false), m.version); err != nil {
			return fmt.Errorf("failed to record migration %d as applied: %w", m.version, err)
		}
	}

	// Legacy fallback: ensure base schema/tables exist even if a DB pre-dates versioning
	// and the embedded 001 was somehow skipped (e.g. legacy binary). This is the
	// original Migrate logic kept verbatim but now also guarded by IF NOT EXISTS.
	legacySchema := `
	CREATE TABLE IF NOT EXISTS providers (
		id TEXT PRIMARY KEY,
		name TEXT UNIQUE NOT NULL,
		type TEXT NOT NULL,
		base_url TEXT NOT NULL,
		api_key_enc BLOB NOT NULL,
		created_at DATETIME NOT NULL,
		last_health TEXT,
		health_status TEXT
	);
	CREATE TABLE IF NOT EXISTS gateway_keys (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		prefix TEXT NOT NULL,
		hash TEXT UNIQUE NOT NULL,
		last_used_at DATETIME,
		created_at DATETIME NOT NULL,
		revoked_at DATETIME,
		rate_limit_rpm INTEGER DEFAULT 60
	);
	CREATE TABLE IF NOT EXISTS request_logs (
		id TEXT PRIMARY KEY,
		key_prefix TEXT,
		provider_id TEXT,
		model TEXT,
		endpoint TEXT,
		status INTEGER,
		latency_ms INTEGER,
		created_at DATETIME NOT NULL,
		prompt_tokens INTEGER DEFAULT 0,
		completion_tokens INTEGER DEFAULT 0,
		total_tokens INTEGER DEFAULT 0,
		cost_usd REAL DEFAULT 0,
		is_stream BOOLEAN DEFAULT 0
	);
	CREATE TABLE IF NOT EXISTS models_catalog (
		id TEXT PRIMARY KEY,
		provider TEXT NOT NULL,
		name TEXT NOT NULL,
		description TEXT,
		family TEXT,
		context_window INTEGER,
		max_output INTEGER,
		input_cost REAL,
		output_cost REAL,
		cache_read_cost REAL,
		cache_write_cost REAL,
		reasoning BOOLEAN,
		tool_call BOOLEAN,
		structured_output BOOLEAN,
		attachment BOOLEAN,
		modalities TEXT,
		open_weights BOOLEAN,
		knowledge_cutoff TEXT,
		updated_at DATETIME,
		reasoning_type TEXT,
		reasoning_levels TEXT,
		reasoning_output_limits TEXT
	);
	CREATE TABLE IF NOT EXISTS model_aliases (
		alias TEXT PRIMARY KEY,
		target TEXT NOT NULL,
		created_at DATETIME NOT NULL
	);
	CREATE TABLE IF NOT EXISTS system_config (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at DATETIME NOT NULL
	);
	CREATE TABLE IF NOT EXISTS provider_models (
		id TEXT PRIMARY KEY,
		provider_id TEXT NOT NULL,
		model_id TEXT NOT NULL,
		display_name TEXT,
		owned_by TEXT,
		context_window INTEGER,
		max_output INTEGER,
		input_cost REAL,
		output_cost REAL,
		cache_read_cost REAL,
		cache_write_cost REAL,
		reasoning BOOLEAN,
		tool_call BOOLEAN,
		structured_output BOOLEAN,
		attachment BOOLEAN,
		modalities TEXT,
		source TEXT,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		reasoning_type TEXT,
		reasoning_levels TEXT,
		reasoning_output_limits TEXT,
		FOREIGN KEY(provider_id) REFERENCES providers(id) ON DELETE CASCADE,
		UNIQUE(provider_id, model_id)
	);
	CREATE INDEX IF NOT EXISTS idx_gateway_keys_hash ON gateway_keys(hash);
	CREATE INDEX IF NOT EXISTS idx_gateway_keys_prefix ON gateway_keys(prefix);
	CREATE INDEX IF NOT EXISTS idx_models_catalog_provider ON models_catalog(provider);
	CREATE INDEX IF NOT EXISTS idx_request_logs_model ON request_logs(model);
	CREATE INDEX IF NOT EXISTS idx_request_logs_created ON request_logs(created_at);
	CREATE INDEX IF NOT EXISTS idx_provider_models_provider ON provider_models(provider_id);
	CREATE INDEX IF NOT EXISTS idx_provider_models_model ON provider_models(model_id);
	`
	if _, err := db.Exec(legacySchema); err != nil {
		return err
	}
	// idempotent column additions for old DBs (including hardening budget cols + org scaffold)
	applyLegacyAlters(db)
	applyHardeningAlters(db)
	applyOrgScaffold(db)
	applyTTFTAlters(db)
	applyErrorAlters(db)
	applyGatewayKeyLimitsAlters(db)
	applyHardeningV2Alters(db)
	applyUsageLoggingAlters(db)
	applyRoutingAlters(db)
	applyKeyAnalyticsAlters(db)
	applyUserPermissionsAlters(db)
	applyKeyRotationAlters(db)
	applyKeyFeaturesAlters(db)
	return nil
}

// applyKeyFeaturesAlters adds key-management columns (IP allowlist, monthly
// budget) and creates the PAT/webhook tables for pre-015 databases.
func applyKeyFeaturesAlters(database *sql.DB) {
	execAlterIdempotent(database, "ALTER TABLE gateway_keys ADD COLUMN ip_allowlist TEXT NOT NULL DEFAULT ''")
	execAlterIdempotent(database, "ALTER TABLE gateway_keys ADD COLUMN monthly_budget_usd REAL NOT NULL DEFAULT 0")
	execAlterIdempotent(database, "ALTER TABLE webhooks ADD COLUMN format TEXT NOT NULL DEFAULT 'json'")
	// Normalize: rows pointing at Discord/Slack with the default format get
	// upgraded, since those platforms 400 on raw JSON envelopes. Explicit
	// non-default choices are never touched. Runs every boot (idempotent).
	database.Exec("UPDATE webhooks SET format='discord' WHERE format='json' AND (url LIKE '%discord.com/api/webhooks%' OR url LIKE '%discordapp.com/api/webhooks%')")
	database.Exec("UPDATE webhooks SET format='slack' WHERE format='json' AND url LIKE '%hooks.slack.com%'")
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS personal_access_tokens (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES dashboard_users(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			hash TEXT NOT NULL UNIQUE,
			prefix TEXT NOT NULL,
			scopes TEXT NOT NULL DEFAULT '',
			last_used_at DATETIME,
			expires_at DATETIME,
			created_at DATETIME NOT NULL,
			revoked_at DATETIME
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pats_user ON personal_access_tokens(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_pats_hash ON personal_access_tokens(hash)`,
		`CREATE TABLE IF NOT EXISTS webhooks (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			url TEXT NOT NULL,
			events TEXT NOT NULL DEFAULT '',
			secret TEXT NOT NULL DEFAULT '',
			org_id TEXT,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			last_status TEXT,
			last_delivery DATETIME
		)`,
	}
	for _, s := range stmts {
		if _, err := database.Exec(s); err != nil {
			log.Error().Err(err).Msg("key-features alter")
		}
	}
}

// applyKeyRotationAlters adds the key-rotation columns idempotently for
// pre-migration-014 databases, mirroring migrations/014_key_rotation.sql.
func applyKeyRotationAlters(db *sql.DB) {
	execAlterIdempotent(db, "ALTER TABLE gateway_keys ADD COLUMN previous_hash TEXT NOT NULL DEFAULT ''")
	execAlterIdempotent(db, "ALTER TABLE gateway_keys ADD COLUMN rotated_at DATETIME")
	execCreateIndexIdempotent(db, "idx_gateway_keys_previous_hash", "CREATE INDEX IF NOT EXISTS idx_gateway_keys_previous_hash ON gateway_keys(previous_hash)")
}

// execCreateIndexIdempotent creates an index if it does not already exist
// (CREATE INDEX IF NOT EXISTS is itself idempotent; wrapper for symmetry).
func execCreateIndexIdempotent(db *sql.DB, name, stmt string) {
	if _, err := db.Exec(stmt); err != nil {
		log.Error().Err(err).Msg("failed to create index " + name)
	}
}

// applyUserPermissionsAlters adds the fine-grained RBAC schema idempotently
// for DBs created before migration 013 (legacy unversioned databases),
// mirroring migrations/013_user_permissions.sql.
func applyUserPermissionsAlters(db *sql.DB) {
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS user_permissions (
		user_id    TEXT NOT NULL REFERENCES dashboard_users(id) ON DELETE CASCADE,
		permission TEXT NOT NULL,
		granted    INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		PRIMARY KEY (user_id, permission)
	)`)
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_user_permissions_user ON user_permissions(user_id)`); err != nil {
		log.Error().Err(err).Msg("failed to create idx_user_permissions_user")
	}
	execAlterIdempotent(db, "ALTER TABLE gateway_keys ADD COLUMN created_by TEXT")
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_gateway_keys_created_by ON gateway_keys(created_by)`); err != nil {
		log.Error().Err(err).Msg("failed to create idx_gateway_keys_created_by")
	}
}

// applyKeyAnalyticsAlters adds request_logs.key_id idempotently for DBs
// created before migration 012 (legacy unversioned databases), mirroring
// migrations/012_key_analytics.sql.
func applyKeyAnalyticsAlters(db *sql.DB) {
	execAlterIdempotent(db, "ALTER TABLE request_logs ADD COLUMN key_id TEXT")
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_request_logs_key_id ON request_logs(key_id)`); err != nil {
		log.Error().Err(err).Msg("failed to create idx_request_logs_key_id")
	}
}

// BackfillKeyIDs populates request_logs.key_id from gateway_keys.prefix for
// legacy rows written before per-key attribution existed. Best-effort: rows
// whose prefix matches multiple keys (legacy DBs refused the UNIQUE index on
// prefix) or no key at all are left NULL. Called once at boot after Migrate.
func BackfillKeyIDs(database *sql.DB) (int64, error) {
	if database == nil {
		return 0, nil
	}
	res, err := database.Exec(`UPDATE request_logs SET key_id = (SELECT k.id FROM gateway_keys k WHERE k.prefix = request_logs.key_prefix) WHERE key_id IS NULL AND key_prefix IN (SELECT prefix FROM gateway_keys)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// applyRoutingAlters adds the lb_rules strategy columns idempotently for DBs
// created before migration 011 (legacy unversioned databases).
func applyRoutingAlters(db *sql.DB) {
	cols := []string{
		"ALTER TABLE lb_rules ADD COLUMN strategy TEXT NOT NULL DEFAULT 'round_robin'",
		"ALTER TABLE lb_rules ADD COLUMN model_override TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE lb_rules ADD COLUMN weight INTEGER NOT NULL DEFAULT 1",
	}
	for _, stmt := range cols {
		execAlterIdempotent(db, stmt)
	}
}

// applyHardeningV2Alters adds production-hardening columns idempotently.
// All statements are tolerant of duplicate-column errors (existing DBs).
// execAlterIdempotent runs an additive ALTER, tolerating "column already
// exists" errors but LOGGING anything else — the old blanket `_, _ =` swallow
// hid genuine failures (disk full, permissions, locks) that only surfaced
// later as runtime query errors.
func execAlterIdempotent(db *sql.DB, stmt string) {
	if _, err := db.Exec(stmt); err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "duplicate column") || // sqlite
			strings.Contains(msg, "already exists") || // postgres 42710
			strings.Contains(msg, "duplicate name") { // sqlite alt form
			return
		}
		log.Error().Err(err).Str("stmt", stmt).Msg("schema ALTER failed (non-duplicate)")
	}
}

func applyHardeningV2Alters(db *sql.DB) {
	cols := []string{
		"ALTER TABLE dashboard_users ADD COLUMN token_version INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE organizations ADD COLUMN daily_cost_limit_cents INTEGER",
		"ALTER TABLE organizations ADD COLUMN monthly_cost_limit_cents INTEGER",
		"ALTER TABLE gateway_keys ADD COLUMN expires_at DATETIME",
		"ALTER TABLE gateway_keys ADD COLUMN metadata TEXT",
	}
	for _, stmt := range cols {
		execAlterIdempotent(db, stmt)
	}
	// Best-effort UNIQUE on gateway_keys.prefix. Refuses (with a warning) when
	// legacy rows already contain duplicate prefixes instead of deleting data.
	var dupes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM (SELECT prefix FROM gateway_keys GROUP BY prefix HAVING COUNT(*) > 1)`).Scan(&dupes); err == nil && dupes == 0 {
		_, _ = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS ux_gateway_keys_prefix ON gateway_keys(prefix)`)
	} else if dupes > 0 {
		logDuplicatePrefixes()
	}
}

func logDuplicatePrefixes() {
	// Kept as a function so the warning stays testable and quiet-by-default
	// until an operator resolves collisions manually.
	fmt.Println("WARNING: gateway_keys contains duplicate prefixes; skipping UNIQUE index creation (resolve duplicates, then re-run)")
}

func upsertSchemaMigration(db *sql.DB, version int, dirty bool) string {
	dirtyVal := BoolLit(false)
	if dirty {
		dirtyVal = BoolLit(true)
	}
	return Rebind(`INSERT INTO schema_migrations(version, dirty) VALUES(?, ` + dirtyVal + `)` + UpsertEnd([]string{"version"}, []string{"dirty"}))
}

func applyLegacyAlters(db *sql.DB) {
	cols := []string{
		"ALTER TABLE providers ADD COLUMN last_health TEXT",
		"ALTER TABLE providers ADD COLUMN health_status TEXT",
		"ALTER TABLE gateway_keys ADD COLUMN rate_limit_rpm INTEGER DEFAULT 60",
		"ALTER TABLE request_logs ADD COLUMN prompt_tokens INTEGER DEFAULT 0",
		"ALTER TABLE request_logs ADD COLUMN completion_tokens INTEGER DEFAULT 0",
		"ALTER TABLE request_logs ADD COLUMN total_tokens INTEGER DEFAULT 0",
		"ALTER TABLE request_logs ADD COLUMN cost_usd REAL DEFAULT 0",
		"ALTER TABLE request_logs ADD COLUMN is_stream BOOLEAN DEFAULT 0",
		"ALTER TABLE models_catalog ADD COLUMN reasoning_type TEXT",
		"ALTER TABLE models_catalog ADD COLUMN reasoning_levels TEXT",
		"ALTER TABLE models_catalog ADD COLUMN reasoning_output_limits TEXT",
		"ALTER TABLE provider_models ADD COLUMN reasoning_type TEXT",
		"ALTER TABLE provider_models ADD COLUMN reasoning_levels TEXT",
		"ALTER TABLE provider_models ADD COLUMN reasoning_output_limits TEXT",
		"ALTER TABLE provider_models ADD COLUMN cache_read_cost REAL",
		"ALTER TABLE provider_models ADD COLUMN cache_write_cost REAL",
		"ALTER TABLE provider_models ADD COLUMN structured_output BOOLEAN",
	}
	for _, stmt := range cols {
		execAlterIdempotent(db, stmt) // ignore error if exists
	}
}

func applyHardeningAlters(db *sql.DB) {
	cols := []string{
		"ALTER TABLE gateway_keys ADD COLUMN daily_token_limit INTEGER",
		"ALTER TABLE gateway_keys ADD COLUMN daily_cost_limit_cents INTEGER",
		"ALTER TABLE gateway_keys ADD COLUMN monthly_cost_limit_cents INTEGER",
		"ALTER TABLE system_config ADD COLUMN description TEXT",
	}
	for _, stmt := range cols {
		execAlterIdempotent(db, stmt)
	}
}

// applyOrgScaffold creates Phase 2.5 pre-enterprise tables and nullable org_id columns.
// Additive, idempotent, and nullable so existing rows keep org_id=NULL ("global").
// Documented as follow-up to 002_hardening.sql per ARCHITECTURE.md Phase 2.5.
func applyOrgScaffold(db *sql.DB) {
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS organizations(id TEXT PRIMARY KEY, name TEXT UNIQUE NOT NULL, created_at DATETIME NOT NULL)`)
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS memberships(id TEXT PRIMARY KEY, org_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE, user_id TEXT NOT NULL, role TEXT NOT NULL, created_at DATETIME NOT NULL)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_memberships_org ON memberships(org_id)`)
	_, _ = db.Exec(`ALTER TABLE providers ADD COLUMN org_id TEXT REFERENCES organizations(id)`)
	_, _ = db.Exec(`ALTER TABLE gateway_keys ADD COLUMN org_id TEXT REFERENCES organizations(id)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_providers_org ON providers(org_id)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_gateway_keys_org ON gateway_keys(org_id)`)
}

func applyTTFTAlters(db *sql.DB) {
	_, _ = db.Exec(`ALTER TABLE request_logs ADD COLUMN ttft_ms INTEGER DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE request_logs ADD COLUMN response_ms INTEGER DEFAULT 0`)
}
func applyErrorAlters(db *sql.DB) {
	_, _ = db.Exec(`ALTER TABLE request_logs ADD COLUMN error TEXT`)
	_, _ = db.Exec(`ALTER TABLE request_logs ADD COLUMN request_body TEXT`)
	_, _ = db.Exec(`ALTER TABLE request_logs ADD COLUMN response_body TEXT`)
}

// applyUsageLoggingAlters adds the per-request usage-metadata columns
// (finish reason, fallback chain, cache/reasoning token detail) idempotently,
// mirroring migrations/010_usage_logging.sql for legacy unversioned DBs.
func applyUsageLoggingAlters(db *sql.DB) {
	cols := []string{
		"ALTER TABLE request_logs ADD COLUMN finish_reason TEXT",
		"ALTER TABLE request_logs ADD COLUMN fallback_chain TEXT",
		"ALTER TABLE request_logs ADD COLUMN cache_read_tokens INTEGER DEFAULT 0",
		"ALTER TABLE request_logs ADD COLUMN cache_write_tokens INTEGER DEFAULT 0",
		"ALTER TABLE request_logs ADD COLUMN reasoning_tokens INTEGER DEFAULT 0",
	}
	for _, stmt := range cols {
		execAlterIdempotent(db, stmt)
	}
}

func applyGatewayKeyLimitsAlters(db *sql.DB) {
	_, _ = db.Exec(`ALTER TABLE gateway_keys ADD COLUMN allowed_models TEXT`)
	_, _ = db.Exec(`ALTER TABLE gateway_keys ADD COLUMN rate_limit_rph INTEGER DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE gateway_keys ADD COLUMN rate_limit_rpd INTEGER DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE gateway_keys ADD COLUMN rate_limit_tpm INTEGER DEFAULT 0`)
}

func isURI(s string) bool {
	return strings.HasPrefix(s, "file:")
}

func containsQuery(s string) bool {
	return strings.Contains(s, "?")
}
