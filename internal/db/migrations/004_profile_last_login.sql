-- 004_profile_last_login.sql — profile enhancements
ALTER TABLE dashboard_users ADD COLUMN last_login_at DATETIME;
ALTER TABLE dashboard_users ADD COLUMN login_count INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_dashboard_users_last_login ON dashboard_users(last_login_at);
-- Ensure audit_logs has actor index already (from 002) — add index for profile activity queries
CREATE INDEX IF NOT EXISTS idx_audit_logs_target ON audit_logs(target_type, target_id);
