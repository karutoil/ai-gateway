-- 014_key_rotation.sql
--
-- Key rotation: gateway keys can be regenerated in place (same id, name,
-- owner, limits, analytics continuity). Columns:
--
--   previous_hash — hash of the immediately-prior secret. During the grace
--     window the old secret still authenticates (so apps don't break the
--     moment rotation happens); it is NULL/'' for never-rotated keys and
--     cleared when the grace window lapses.
--   rotated_at    — when the current secret was created (drives the grace
--     window and shows "last rotated" in the UI).

ALTER TABLE gateway_keys ADD COLUMN previous_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE gateway_keys ADD COLUMN rotated_at DATETIME;
CREATE INDEX IF NOT EXISTS idx_gateway_keys_prev_hash ON gateway_keys(previous_hash);
