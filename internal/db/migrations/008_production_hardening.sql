-- 008: production-hardening wave
-- Safe idempotent DDL only; column ALTERs live in Go applyHardeningV2Alters()
-- because SQLite cannot express conditional ADD COLUMN in raw SQL.

-- Atomic spend ledger for budgets/billing (integer micro-USD; 1e-6 USD per unit).
--   scope     = 'key:<prefix>' | 'org:<org_id>'
--   period    = 'day' | 'month'
--   start_utc = ISO date/time of window start (UTC)
CREATE TABLE IF NOT EXISTS spend_counters (
	scope TEXT NOT NULL,
	period TEXT NOT NULL,
	start_utc TEXT NOT NULL,
	tokens INTEGER NOT NULL DEFAULT 0,
	cost_micros INTEGER NOT NULL DEFAULT 0,
	updated_at DATETIME NOT NULL,
	PRIMARY KEY(scope, period, start_utc)
);

-- Hot-path query support (budget checks, top-key stats, org-scoped listings).
CREATE INDEX IF NOT EXISTS idx_request_logs_prefix_created ON request_logs(key_prefix, created_at);
CREATE INDEX IF NOT EXISTS idx_request_logs_provider_created ON request_logs(provider_id, created_at);
