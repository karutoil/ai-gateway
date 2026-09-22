-- backfill-cache-status.sql — classify legacy request rows for cache reporting
--
-- Rows written before cache_status existed (migration 021) show "cache ·—" in
-- the dashboard and are excluded from cache analytics. This one-time backfill
-- classifies them from what WAS recorded:
--
--   1. hit    Non-stream 2xx responses with zero tokens, zero cost AND
--             latency <= 250ms. The old cache-hit path logged exactly this
--             shape (served from memory, never billed). The latency bound is
--             the discriminator: verified cache hits measure ~0ms, while
--             real upstream responses that omit usage run 1.4s+. The
--             distribution is bimodal with nothing between 154ms and 1371ms.
--             cache_hit is set too so SUM-based stats count them.
--   2. bypass Streaming requests. Exact, not inferred: before CACHE_STREAMS
--             existed, streams were never cache-eligible.
--   3. miss   Remaining non-stream requests: cache-eligible at the time
--             (including slow zero-usage rows, which were real upstream
--             calls and correctly NOT hits).
--
-- Run once per database:
--   sqlite3  ./data/gateway.db < scripts/backfill-cache-status.sql
--   psql "$DATABASE_URL" -f scripts/backfill-cache-status.sql

-- 1. Recovered cache hits (high-confidence signature).
UPDATE request_logs
   SET cache_status = 'hit', cache_hit = 1
 WHERE cache_status IS NULL
   AND NOT is_stream
   AND status = 200
   AND COALESCE(total_tokens, 0) = 0
   AND COALESCE(cost_usd, 0) = 0
   AND latency_ms <= 250;

-- 2. Streams were never cache-eligible before CACHE_STREAMS.
UPDATE request_logs
   SET cache_status = 'bypass'
 WHERE cache_status IS NULL
   AND is_stream;

-- 3. Everything else non-stream was cache-eligible and consulted the cache.
UPDATE request_logs
   SET cache_status = 'miss'
 WHERE cache_status IS NULL
   AND NOT is_stream;
