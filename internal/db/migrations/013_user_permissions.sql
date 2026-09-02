-- 013_user_permissions.sql — fine-grained RBAC
--
-- 1. user_permissions: per-user permission overrides on top of role defaults.
--    granted=1 → allow, granted=0 → deny. Rows absent → role default applies.
--    Resolution happens live per request (no re-login needed), mirroring how
--    roles already refresh from the DB.
--
-- 2. gateway_keys.created_by: the dashboard user that created a key, backing
--    the keys:read_own permission ("see only their own keys"). NULL for
--    legacy keys — unowned keys are visible only to keys:read holders.
--
-- Both purely additive; existing rows keep working unchanged.

CREATE TABLE IF NOT EXISTS user_permissions (
  user_id    TEXT NOT NULL REFERENCES dashboard_users(id) ON DELETE CASCADE,
  permission TEXT NOT NULL,
  granted    INTEGER NOT NULL DEFAULT 1,
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL,
  PRIMARY KEY (user_id, permission)
);
CREATE INDEX IF NOT EXISTS idx_user_permissions_user ON user_permissions(user_id);

ALTER TABLE gateway_keys ADD COLUMN created_by TEXT;
CREATE INDEX IF NOT EXISTS idx_gateway_keys_created_by ON gateway_keys(created_by);
