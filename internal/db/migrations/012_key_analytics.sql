-- 012_key_analytics.sql — per-API-key analytics
--
-- Adds request_logs.key_id: the gateway_keys.id of the sk-gw-* credential that
-- authenticated the request (empty/NULL for dashboard-session "virtual keys").
--
-- Rationale for a denormalized column instead of joining on key_prefix:
--   * prefixes are display-only (8 chars), may be remapped/rotated, and are
--     NOT guaranteed unique on legacy databases (see applyHardeningV2Alters,
--     which refuses the UNIQUE index when duplicates exist);
--   * gateway_keys.id is the stable identifier used by every other API
--     surface (revoke, limits, org scoping).
--
-- Purely additive: existing rows keep key_id NULL and every legacy query is
-- untouched. Backfill from the prefix is best-effort at boot (migrations must
-- stay deterministic and fast; ambiguous prefixes on legacy DBs are skipped).
ALTER TABLE request_logs ADD COLUMN key_id TEXT;
CREATE INDEX IF NOT EXISTS idx_request_logs_key_id ON request_logs(key_id);
