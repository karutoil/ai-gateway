-- 017_oauth.sql — OAuth-backed providers (Antigravity first, generic registry).
-- Additive and nullable-compatible so existing api-key providers are untouched.
-- oauth_refresh_enc / oauth_access_enc are AES-GCM blobs under MASTER_KEY.
-- oauth_def_id keys the internal/oauth registry ("antigravity", ...).
ALTER TABLE providers ADD COLUMN oauth_def_id TEXT NOT NULL DEFAULT '';
ALTER TABLE providers ADD COLUMN oauth_refresh_enc BLOB;
ALTER TABLE providers ADD COLUMN oauth_access_enc BLOB;
ALTER TABLE providers ADD COLUMN oauth_expires_at BIGINT;
ALTER TABLE providers ADD COLUMN oauth_email TEXT NOT NULL DEFAULT '';
ALTER TABLE providers ADD COLUMN oauth_project_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_providers_oauth_def ON providers(oauth_def_id);
