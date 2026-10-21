-- 030_video_retention_and_short_id.sql — retention indexes for video_jobs plus an
-- indexed short model id for the per-request catalog lookup.
--
-- Two independent production fixes. Both are purely additive: no destructive
-- rewrite, no column dropped, no lock beyond the DDL itself (Postgres takes
-- SHARE UPDATE EXCLUSIVE for CREATE INDEX IF NOT EXISTS / ACCESS EXCLUSIVE for
-- the single ADD COLUMN, both under a second on the row counts involved).
--
-- 1. video_jobs retention (P0-3).
--    Nothing ever deleted a row from video_jobs except the client's own
--    DELETE /v1/videos/{id}, so the fastest-growing table in the schema grew
--    with every create — failed and abandoned jobs included — and the
--    expires_at the provider supplies was written and never read. The sweep
--    itself lives in Go (db.PruneVideoJobs) because the retention window is
--    configuration; this file gives it the indexes it needs and gives
--    GET /v1/videos a sort order it can satisfy from one index:
--
--      idx_video_jobs_expires         — expiry sweep (expires_at IS NOT NULL)
--      idx_video_jobs_status_created  — the expires_at IS NULL retention sweep,
--                                       which filters on terminal status
--      idx_video_jobs_key_created     — (key_prefix, created_at DESC) so the
--                                       per-key list stops building a temp
--                                       b-tree over the key's whole history
--                                       before LIMIT 100. SQLite ignores the
--                                       DESC hint but still uses the two-column
--                                       index to satisfy ORDER BY created_at.
--
--    The superseded single-column idx_video_jobs_key is intentionally NOT
--    dropped: DROP INDEX takes a write-blocking lock on SQLite/older engines and
--    is only safe once every node runs this migration, so it waits for a later
--    file. Its current cost is one small btree write per job row.
--
-- 2. models_catalog.short_id (P0-6).
--    The lookup chain resolved a bare model id with
--    `WHERE LOWER(id) LIKE LOWER('%/<slug>')` — a leading-wildcard LIKE that no
--    B-tree can serve. EXPLAIN QUERY PLAN showed SCAN models_catalog + USE TEMP
--    B-TREE FOR ORDER BY at 6.16 ms/request on a 4k-row catalog, on every chat
--    request and up to 15x per image request. The suffix after the last '/'
--    (the "short id": "openai/gpt-4o" -> "gpt-4o") becomes a stored, indexed
--    column so the common case is an exact index lookup; the containment query
--    survives only as the last resort.
--
--    The column is nullable on purpose: legacy or operator-inserted rows that
--    never went through the sync must not break, and the Go lookup treats a
--    NULL short_id as "not indexed" and falls back to containment. Rows that
--    already exist are backfilled by db.Migrate (applyModelShortIDBackfill) —
--    the extraction is done in Go because last-slash substring extraction has
--    no portable SQL spelling (SQLite has instr() but not strpos(), Postgres
--    the opposite).
--
-- Applied by db.Migrate. The file is split on ';' by splitStatements on both
-- dialects, so no statement may contain ';' inside a literal.
ALTER TABLE models_catalog ADD COLUMN short_id TEXT;

CREATE INDEX IF NOT EXISTS idx_models_catalog_short_id ON models_catalog(short_id);

-- Case-insensitive exact match tier (`LOWER(id)=LOWER(?)`), which is the only
-- shape that can actually use this index; the containment LIKE cannot.
CREATE INDEX IF NOT EXISTS idx_models_catalog_id_lower ON models_catalog(LOWER(id));

CREATE INDEX IF NOT EXISTS idx_video_jobs_expires ON video_jobs(expires_at);
CREATE INDEX IF NOT EXISTS idx_video_jobs_status_created ON video_jobs(status, created_at);
CREATE INDEX IF NOT EXISTS idx_video_jobs_key_created ON video_jobs(key_prefix, created_at DESC);
