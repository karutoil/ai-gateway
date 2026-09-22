-- 019_cache_hit.sql — gateway response-cache visibility in reporting
-- Rows served from the exact-match response cache (the X-Cache: HIT path)
-- are stamped cache_hit=1 so reports can separate cached responses from
-- fresh upstream calls. The token counters from migration 010
-- (cache_read_tokens / cache_write_tokens) measure the UPSTREAM prompt
-- cache and are untouched; until now a gateway cache hit was logged as an
-- ordinary zero-token request and was invisible in the dashboard.
ALTER TABLE request_logs ADD COLUMN cache_hit INTEGER DEFAULT 0;
