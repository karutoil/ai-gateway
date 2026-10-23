-- 035_budget_and_search_indexes.sql — index fixes for query shapes that sit on
-- the request path, plus the covering index for the monthly-budget sum.
--
-- Purely additive: CREATE INDEX IF NOT EXISTS only. No table rebuild and no
-- column change, so replaying the whole migration set against a migrated
-- database stays a no-op (the property db.Migrate's duplicate tolerance relies
-- on) and no existing data can be altered by this file.

-- 1. Monthly budget enforcement — the ONLY cost cap that actually binds.
--
-- apikey.MonthSpendUSDForPrefix runs, on EVERY request for any key with
-- monthly_budget_usd > 0 (i.e. on the auth path):
--     SELECT SUM(cost_usd) FROM request_logs WHERE key_id = ?     AND created_at >= ?
--     SELECT SUM(cost_usd) FROM request_logs WHERE key_prefix = ? AND created_at >= ?
--
-- The pre-existing indexes (idx_request_logs_key_id,
-- idx_request_logs_prefix_created) locate the rows but carry no cost_usd, so the
-- planner fetches every matching row from the table: the cost is O(requests this
-- month) per request — O(n²) over a month, growing monotonically, and slowest in
-- exactly the month a key finally reaches its cap.
--
-- Adding cost_usd as the third column makes both sums INDEX-ONLY: the scan stays
-- in the index leaf pages and never touches the table.
CREATE INDEX IF NOT EXISTS idx_request_logs_key_id_cost ON request_logs(key_id, created_at, cost_usd);
CREATE INDEX IF NOT EXISTS idx_request_logs_prefix_cost ON request_logs(key_prefix, created_at, cost_usd);

-- 2. Dashboard catalog search.
--
-- catalog.List sorts by (provider, id) with LIMIT/OFFSET and had no usable index
-- for that order, so every dashboard page sorted the whole catalog (temp B-tree
-- over 4k+ rows). The name search is an expression index on LOWER(name) so the
-- LOWER(col) LIKE LOWER(?) rewrite the callers now use is index-servable on both
-- dialects (Postgres LIKE is case-sensitive; SQLite's is ASCII-case-insensitive
-- by default, which is why the callers normalise explicitly).
CREATE INDEX IF NOT EXISTS idx_models_catalog_provider_id ON models_catalog(provider, id);
CREATE INDEX IF NOT EXISTS idx_models_catalog_name_lower ON models_catalog(LOWER(name));

-- 3. spend_counters retention.
--
-- The ledger accumulates two rows per key per day plus one per month, forever,
-- and its only index is the primary key (scope, period, start_utc), which a
-- "delete everything older than N days" sweep cannot use — it would be a full
-- scan on every run. This index makes that sweep cheap.
CREATE INDEX IF NOT EXISTS idx_spend_counters_start ON spend_counters(start_utc);
