-- 031_video_jobs_key_prefix_not_null.sql — close the empty-prefix fail-open.
--
-- video_jobs.key_prefix was declared nullable (migration 028) and never
-- backfilled or constrained. That column is the whole video tenancy check:
-- videoJobVisible() compares the caller's X-Gateway-Key-Prefix against
-- job.key_prefix. The first version of that check treated an empty prefix on
-- EITHER side as "visible", so a NULL/empty stored value made the ownership test
-- meaningless — a fail-open in the one function that decides who may read,
-- poll and DELETE a video job. The Go side now denies an empty prefix outright
-- (and routes dashboard sessions through an explicit admin branch instead of
-- relying on the empty-header coincidence); this migration removes the
-- possibility at the schema level so no future write path can reintroduce a row
-- that no key owns.
--
-- WHICH ROWS THIS CAN AFFECT: none that a caller could legitimately reach.
-- attachGatewayKey always sets a non-empty prefix for real keys
-- (apikey.Prefix, hex) and for dashboard sessions (sessionPrefix → "sess" +
-- alnum), so no legitimate create ever wrote NULL/empty. Any such row is
-- unowned by construction, i.e. already unreachable after the Go-side fix.
--
-- PORTABILITY: SQLite cannot add NOT NULL to an existing column and cannot alter
-- nullability at all (it needs a full table rebuild, which is not worth doing on
-- a live table for a set that should be empty). So the invariant is enforced in
-- two portable steps instead:
--
--   1. backfill any legacy NULL/empty rows to the literal 'unowned'. No real
--      prefix can equal it (prefixes are hex, or "sess" + alnum), so an orphan
--      becomes explicitly labelled rather than ambiguously "everyone's".
--   2. a partial index so the orphan set is cheap to find, both for operators
--      auditing and for the retention sweep.
--
-- The NOT NULL itself is asserted in Go (insertVideoJob always binds a value)
-- and by a test, which is the enforceable form on both dialects.
UPDATE video_jobs SET key_prefix = 'unowned' WHERE key_prefix IS NULL OR key_prefix = '';

CREATE INDEX IF NOT EXISTS idx_video_jobs_unowned
    ON video_jobs(key_prefix)
    WHERE key_prefix = 'unowned';
