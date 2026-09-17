-- 018_response_store.sql — server-side history for /v1/responses chaining.
-- OpenAI-compat upstreams (e.g. LiteLLM proxies) do not implement the
-- Responses API, so the gateway translates onto /chat/completions. Chained
-- turns reference history via previous_response_id, which is meaningless
-- without server-side state: each translated turn stores its full chat
-- message list (inputs + assistant outputs) under the response id it
-- issued, and the next turn expands the chain back into chat messages.
-- Rows are key-scoped (key_prefix) and short-lived (created_at_unix);
-- readers ignore anything older than 24h and writers purge expired rows
-- opportunistically. Additive table only — no existing schema touched.
CREATE TABLE IF NOT EXISTS response_turns (
  id TEXT PRIMARY KEY,
  key_prefix TEXT NOT NULL DEFAULT '',
  provider_id TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  prev_id TEXT NOT NULL DEFAULT '',
  messages TEXT NOT NULL DEFAULT '[]',
  created_at_unix INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_response_turns_expiry ON response_turns(created_at_unix);
