-- 015_key_management_features.sql
-- Adds: key expiry surface (expires_at exists but unmanaged), per-key IP
-- allowlists, monthly spend caps, personal API tokens, and manageable
-- webhooks. All additive; existing rows keep working unchanged.

-- Key expiry is enforced by Verify already; nothing new needed on gateway_keys.

-- Per-key IP allowlist: empty/COLUMN NULL = allow all. Comma/space separated
-- CIDR ranges or exact IPs (e.g. "10.0.0.0/8,203.0.113.7").
ALTER TABLE gateway_keys ADD COLUMN ip_allowlist TEXT NOT NULL DEFAULT '';

-- Monthly spend cap in USD for the calendar month; 0/NULL = unlimited.
ALTER TABLE gateway_keys ADD COLUMN monthly_budget_usd REAL NOT NULL DEFAULT 0;

-- Personal access tokens for the dashboard API (automation/CI).
-- Token format: gwp_<32 hex>; only the SHA-256 hash is stored.
CREATE TABLE IF NOT EXISTS personal_access_tokens (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES dashboard_users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    hash        TEXT NOT NULL UNIQUE,
    prefix      TEXT NOT NULL,
    scopes      TEXT NOT NULL DEFAULT '',  -- comma-separated permission names; '' = inherit user's effective set
    last_used_at DATETIME,
    expires_at  DATETIME,
    created_at  DATETIME NOT NULL,
    revoked_at  DATETIME
);
CREATE INDEX IF NOT EXISTS idx_pats_user ON personal_access_tokens(user_id);
CREATE INDEX IF NOT EXISTS idx_pats_hash ON personal_access_tokens(hash);

-- Manageable webhooks (replaces env-only configuration).
-- events: comma-separated event types; '' = all events.
CREATE TABLE IF NOT EXISTS webhooks (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    url         TEXT NOT NULL,
    events      TEXT NOT NULL DEFAULT '',
    secret      TEXT NOT NULL DEFAULT '',
    org_id      TEXT,
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  DATETIME NOT NULL,
    updated_at  DATETIME NOT NULL,
    last_status TEXT,
    last_delivery DATETIME
);
