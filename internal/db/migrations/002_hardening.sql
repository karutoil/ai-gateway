-- 002_hardening.sql — Phase 1.6 buffer (additive, nullable, idempotent)
-- Run after 001_initial on both fresh and existing DBs.
CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, dirty BOOLEAN NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS audit_logs (
  id TEXT PRIMARY KEY,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,
  target_type TEXT NOT NULL,
  target_id TEXT NOT NULL,
  meta TEXT,
  created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_logs_actor ON audit_logs(actor);
CREATE INDEX IF NOT EXISTS idx_audit_logs_created ON audit_logs(created_at);
-- budget prerequisites (nullable so no backfill needed)
-- Use IF NOT EXISTS guard via separate statements executed idempotently by db.Migrate harness:
-- harness will try ALTER and ignore "duplicate column" errors, mirroring existing pattern.
-- Documented here for versioned runner; actual ALTERs are executed by db.Migrate's idempotent loop:
-- ALTER TABLE gateway_keys ADD COLUMN daily_token_limit INTEGER;
-- ALTER TABLE gateway_keys ADD COLUMN daily_cost_limit_cents INTEGER;
-- ALTER TABLE gateway_keys ADD COLUMN monthly_cost_limit_cents INTEGER;
-- description col on system_config is optional (future use)
-- ALTER TABLE system_config ADD COLUMN description TEXT;
