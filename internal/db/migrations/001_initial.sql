-- 001_initial.sql — idempotent baseline, mirrors current db.Migrate schema + indexes
-- This file documents the baseline that db.Migrate already ensures. Kept for versioned runner in Phase 1.6.
CREATE TABLE IF NOT EXISTS providers (
  id TEXT PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  type TEXT NOT NULL,
  base_url TEXT NOT NULL,
  api_key_enc BLOB NOT NULL,
  created_at DATETIME NOT NULL,
  last_health TEXT,
  health_status TEXT
);
CREATE TABLE IF NOT EXISTS gateway_keys (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  prefix TEXT NOT NULL,
  hash TEXT UNIQUE NOT NULL,
  last_used_at DATETIME,
  created_at DATETIME NOT NULL,
  revoked_at DATETIME,
  rate_limit_rpm INTEGER DEFAULT 60
);
CREATE TABLE IF NOT EXISTS request_logs (
  id TEXT PRIMARY KEY,
  key_prefix TEXT,
  provider_id TEXT,
  model TEXT,
  endpoint TEXT,
  status INTEGER,
  latency_ms INTEGER,
  created_at DATETIME NOT NULL,
  prompt_tokens INTEGER DEFAULT 0,
  completion_tokens INTEGER DEFAULT 0,
  total_tokens INTEGER DEFAULT 0,
  cost_usd REAL DEFAULT 0,
  is_stream BOOLEAN DEFAULT 0
);
CREATE TABLE IF NOT EXISTS models_catalog (
  id TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  name TEXT NOT NULL,
  description TEXT,
  family TEXT,
  context_window INTEGER,
  max_output INTEGER,
  input_cost REAL,
  output_cost REAL,
  cache_read_cost REAL,
  cache_write_cost REAL,
  reasoning BOOLEAN,
  tool_call BOOLEAN,
  structured_output BOOLEAN,
  attachment BOOLEAN,
  modalities TEXT,
  open_weights BOOLEAN,
  knowledge_cutoff TEXT,
  updated_at DATETIME,
  reasoning_type TEXT,
  reasoning_levels TEXT,
  reasoning_output_limits TEXT
);
CREATE TABLE IF NOT EXISTS model_aliases (
  alias TEXT PRIMARY KEY,
  target TEXT NOT NULL,
  created_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS system_config (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS provider_models (
  id TEXT PRIMARY KEY,
  provider_id TEXT NOT NULL,
  model_id TEXT NOT NULL,
  display_name TEXT,
  owned_by TEXT,
  context_window INTEGER,
  max_output INTEGER,
  input_cost REAL,
  output_cost REAL,
  cache_read_cost REAL,
  cache_write_cost REAL,
  reasoning BOOLEAN,
  tool_call BOOLEAN,
  structured_output BOOLEAN,
  attachment BOOLEAN,
  modalities TEXT,
  source TEXT,
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL,
  reasoning_type TEXT,
  reasoning_levels TEXT,
  reasoning_output_limits TEXT,
  FOREIGN KEY(provider_id) REFERENCES providers(id) ON DELETE CASCADE,
  UNIQUE(provider_id, model_id)
);
CREATE INDEX IF NOT EXISTS idx_gateway_keys_hash ON gateway_keys(hash);
CREATE INDEX IF NOT EXISTS idx_gateway_keys_prefix ON gateway_keys(prefix);
CREATE INDEX IF NOT EXISTS idx_models_catalog_provider ON models_catalog(provider);
CREATE INDEX IF NOT EXISTS idx_request_logs_model ON request_logs(model);
CREATE INDEX IF NOT EXISTS idx_request_logs_created ON request_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_provider_models_provider ON provider_models(provider_id);
CREATE INDEX IF NOT EXISTS idx_provider_models_model ON provider_models(model_id);
