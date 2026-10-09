-- 024_request_attribution.sql — per-request identity beyond the gateway key
--
-- The gateway attributed usage only to the sk-gw-* key. One key can front many
-- end users and many applications, so "who used this and for what" was
-- unanswerable — the single biggest gap versus every surveyed router, all of
-- which carry a per-identity dimension.
--
--   end_user  the person/agent the call was made on behalf of. Sourced from
--             the OpenAI top-level "user" field, Anthropic metadata.user_id,
--             or an explicit X-End-User header.
--   app       the calling application. Sourced from X-App-Id, X-Title /
--             X-OpenRouter-Title, or the origin of HTTP-Referer (matching
--             OpenRouter's app-attribution headers).
--
-- Both are attacker-influenced and length-capped in code before storage.
--
-- Deliberately NOT exposed as Prometheus labels: end_user is unbounded by
-- construction, and the same reasoning that makes Langfuse forbid grouping by
-- userId/sessionId applies to a metric label. `app` IS safe to group by in SQL
-- because an operator bounds how many apps attribute to them.
--
-- Purely additive: existing rows keep NULL and every legacy query is
-- untouched.
ALTER TABLE request_logs ADD COLUMN end_user TEXT;
ALTER TABLE request_logs ADD COLUMN app TEXT;
CREATE INDEX IF NOT EXISTS idx_request_logs_end_user ON request_logs(end_user);
CREATE INDEX IF NOT EXISTS idx_request_logs_app ON request_logs(app);
