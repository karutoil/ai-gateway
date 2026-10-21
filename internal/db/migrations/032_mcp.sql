-- 032_mcp.sql — centralized MCP server registry, tool cache and per-key grants.
--
-- The gateway owns the upstream MCP connections (Streamable HTTP, legacy SSE
-- or a locally launched stdio process), discovers their tools, and exposes a
-- single authenticated MCP endpoint at /mcp. Credentials never leave the
-- server: they are AES-GCM encrypted with MASTER_KEY (the same key used for
-- provider credentials) and are never serialized back to the dashboard.
CREATE TABLE IF NOT EXISTS mcp_servers (
  id             TEXT PRIMARY KEY,
  name           TEXT NOT NULL,
  slug           TEXT NOT NULL UNIQUE,       -- namespacing prefix for tools
  transport      TEXT NOT NULL,              -- http | sse | stdio
  url            TEXT,                       -- http/sse endpoint
  command        TEXT,                       -- stdio executable
  args           TEXT,                       -- JSON array of strings
  env_enc        BLOB,                       -- AES-GCM JSON object (stdio env)
  headers_enc    BLOB,                       -- AES-GCM JSON object (static HTTP headers)
  auth_type      TEXT NOT NULL DEFAULT 'none', -- none | bearer | api_key | header
  auth_header    TEXT,                       -- header name for api_key/header
  auth_value_enc BLOB,                       -- AES-GCM secret for auth_type
  enabled        INTEGER NOT NULL DEFAULT 1,
  health_status  TEXT NOT NULL DEFAULT 'unknown', -- up | down | unknown
  last_error     TEXT,
  last_health_at DATETIME,
  tool_count     INTEGER NOT NULL DEFAULT 0,
  org_id         TEXT,
  created_at     DATETIME NOT NULL,
  updated_at     DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mcp_servers_enabled ON mcp_servers(enabled);

CREATE TABLE IF NOT EXISTS mcp_server_tools (
  server_id     TEXT NOT NULL,
  name          TEXT NOT NULL,
  description   TEXT,
  input_schema  TEXT,                        -- JSON schema object
  discovered_at DATETIME NOT NULL,
  PRIMARY KEY (server_id, name)
);

-- Explicit per-key grants. Absence of any row for a key means "all enabled
-- servers and all of their tools" (the historical, least-surprising default);
-- once a key has at least one row, only enabled rows grant access. An empty
-- allowed_tools array means "every tool on that server".
CREATE TABLE IF NOT EXISTS mcp_key_permissions (
  key_id        TEXT NOT NULL,
  server_id     TEXT NOT NULL,
  allowed_tools TEXT,                        -- JSON array; NULL/empty = all
  enabled       INTEGER NOT NULL DEFAULT 1,
  created_at    DATETIME NOT NULL,
  updated_at    DATETIME NOT NULL,
  PRIMARY KEY (key_id, server_id)
);
CREATE INDEX IF NOT EXISTS idx_mcp_key_permissions_key ON mcp_key_permissions(key_id);

-- Execution audit trail for tool calls (both /mcp and LLM auto-execution).
-- Arguments/results are deliberately NOT stored: they routinely contain
-- source code, tokens and user data.
CREATE TABLE IF NOT EXISTS mcp_tool_calls (
  id              TEXT PRIMARY KEY,
  server_id       TEXT NOT NULL,
  server_slug     TEXT NOT NULL,
  tool_name       TEXT NOT NULL,
  namespaced_name TEXT NOT NULL,
  key_prefix      TEXT,
  endpoint        TEXT,
  status          TEXT NOT NULL,             -- ok | error | denied
  error           TEXT,
  duration_ms     INTEGER NOT NULL DEFAULT 0,
  created_at      DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mcp_tool_calls_created ON mcp_tool_calls(created_at);
CREATE INDEX IF NOT EXISTS idx_mcp_tool_calls_key ON mcp_tool_calls(key_prefix);
