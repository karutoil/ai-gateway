-- 003_dashboard_rbac_passkey.sql — dashboard RBAC + passkey + recovery
CREATE TABLE IF NOT EXISTS dashboard_users (
  id TEXT PRIMARY KEY,
  username TEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'admin',
  display_name TEXT,
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL,
  passkey_enabled INTEGER NOT NULL DEFAULT 0,
  recovery_code_hash TEXT,
  recovery_generated_at DATETIME,
  disabled INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_dashboard_users_username ON dashboard_users(username);
CREATE INDEX IF NOT EXISTS idx_dashboard_users_role ON dashboard_users(role);

CREATE TABLE IF NOT EXISTS webauthn_credentials (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES dashboard_users(id) ON DELETE CASCADE,
  credential_id TEXT UNIQUE NOT NULL,
  public_key BLOB NOT NULL,
  attestation_type TEXT,
  transports TEXT,
  flags INTEGER,
  counter INTEGER NOT NULL DEFAULT 0,
  cloned INTEGER NOT NULL DEFAULT 0,
  backed_up INTEGER NOT NULL DEFAULT 0,
  created_at DATETIME NOT NULL,
  last_used_at DATETIME
);
CREATE INDEX IF NOT EXISTS idx_webauthn_credentials_user ON webauthn_credentials(user_id);
CREATE INDEX IF NOT EXISTS idx_webauthn_credentials_cred ON webauthn_credentials(credential_id);

CREATE TABLE IF NOT EXISTS recovery_codes (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES dashboard_users(id) ON DELETE CASCADE,
  code_hash TEXT NOT NULL,
  used INTEGER NOT NULL DEFAULT 0,
  created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_recovery_codes_user ON recovery_codes(user_id);
