-- 007_gateway_key_limits.sql — per-key model allowlist + extended rate limits
CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, dirty BOOLEAN NOT NULL DEFAULT 0);
-- Additive, nullable / default 0 so existing rows unaffected (no backfill).
-- Actual ALTERs are executed idempotently by db.Migrate harness as well.
-- Documented here for versioned runner:
-- ALTER TABLE gateway_keys ADD COLUMN allowed_models TEXT;
-- ALTER TABLE gateway_keys ADD COLUMN rate_limit_rph INTEGER DEFAULT 0;
-- ALTER TABLE gateway_keys ADD COLUMN rate_limit_rpd INTEGER DEFAULT 0;
-- ALTER TABLE gateway_keys ADD COLUMN rate_limit_tpm INTEGER DEFAULT 0;
