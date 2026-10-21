# Database / Persistence Review — decision, image & video surfaces

Scope (per BRIEF): `internal/db/migrations/026_decision_models.sql`,
`027_image_models.sql`, `028_video_jobs.sql`, `029_video_models.sql`,
`internal/db/db.go` (`migrationsList`, `LatestMigrationVersion`, `BoolInt`,
`applyLegacyAlters`, `Migrate`), `internal/catalog/catalog.go`
(`SeedDecisionModels` / `SeedImageModels` / `SeedVideoModels`, sync),
`internal/db/pgsmoke/*.go`, and every statement touching `video_jobs` in
`internal/proxy/videos.go`.

Method: read all four migrations + the runner; grepped every write path for the
three flag columns; ran the SQLite test suite; ran ad-hoc schema probes
(`PRAGMA table_info`, `EXPLAIN QUERY PLAN`, timestamp round-trip, migration
replay, seed-clobber). **No Postgres instance was available in this environment**
(no `GATEWAY_PG_DSN`, `pg_isready` fails), so Postgres claims below are derived
from the driver contracts (`lib/pq` `NumInput`, `pgTranslateDDL`,
`splitStatements`) and from reading `pgsmoke`, not from a live run. That gap is
itself finding #P1-1. No source was modified.

---

## 1. Schema change inventory (what the four migrations actually do)

| Migration | Statements | Verdict |
|---|---|---|
| 026 | `ALTER TABLE models_catalog ADD COLUMN decision INTEGER DEFAULT 0`; same on `provider_models`; `CREATE INDEX IF NOT EXISTS idx_models_catalog_decision` | correct on both dialects |
| 027 | same shape with `image` | correct |
| 028 | `CREATE TABLE IF NOT EXISTS video_jobs (...)`; 3 indexes (`key_prefix`, `provider_id`, `created_at`) | correct DDL, **incomplete for its own use cases** (§4) |
| 029 | same shape with `video` | correct |

Dialect details I verified concretely on SQLite and reasoned through for PG:

- **INTEGER vs BOOLEAN.** `decision/image/video` are `INTEGER` on both dialects
  (028 also uses `INTEGER`). `pgTranslateDDL` (`internal/db/db.go:295-305`) only
  rewrites `DATETIME→TIMESTAMPTZ`, `BLOB→BYTEA` and `BOOLEAN … DEFAULT 0/1`, so
  `INTEGER` survives as `integer` — that is *why* `BoolInt` exists. Consistent.
- **DATETIME → TIMESTAMP.** `video_jobs.created_at/updated_at/completed_at/
  expires_at` are `DATETIME` (`028_video_jobs.sql:27-30`), rewritten to
  `TIMESTAMPTZ` by `pgTranslateDDL`. Verified on SQLite: `PRAGMA table_info`
  reports `DATETIME` and values round-trip as
  `"2026-10-17T17:37:52.856615826Z"` (RFC3339Nano text).
- **CREATE INDEX.** Every new index is `IF NOT EXISTS` → replay-safe.
- **ALTER TABLE ADD COLUMN idempotency.** None of 026/027/029 is idempotent at
  the SQL level (SQLite has no `ADD COLUMN IF NOT EXISTS`); they rely on the
  ledger. See §5 for why that is not enough on the legacy path.
- **Rebind coverage.** Every new query goes through `db.Q`/`db.Rebind`
  (`videos.go:267, 299, 699, 715, 722`; `catalog.go:745, 808, 931, 956`;
  `discovery.go:410, 415, 621, 627, 688, 775, 801`). I checked `Rebind`'s
  blind `?`-substitution for false positives (question marks inside string
  literals) and found **no** `db.Q`/`Rebind` call containing a `?` inside a
  quoted literal — the only `'%/'||?`-style LIKE concatenations
  (`proxy.go:3587`, `pgsmoke/smoke_test.go:124`) have their `?` outside quotes.
  So `Rebind` is safe today, but it is a plain string scanner
  (`db.go:173-190`) with no test guarding that invariant.
- **LIMIT/OFFSET.** Both dialects use `LIMIT ? OFFSET ?`
  (`catalog.go:182`, `videos.go:270` uses `LIMIT 100` literal) — valid in
  Postgres too, no rewriting needed.
- **RETURNING.** Not used anywhere in the new code (grep: no hits). Inserts read
  back by re-query (`videos.go:722`) — portable.

So: **(1) migrations are individually correct on both dialects.** The problems
are around them, not in their syntax.

---

## 2. The "never write a Go bool into an INTEGER column" trap

**Every current write path is clean.** Full inventory of writes to
`decision`/`image`/`video`:

| Path | Line | Uses `db.BoolInt` |
|---|---|---|
| catalog sync upsert | `internal/catalog/catalog.go:781` | yes ×3 |
| `SeedDecisionModels` | `catalog.go:820` | yes ×3 |
| `SeedImageModels` | `catalog.go:942` | yes ×3 |
| `SeedVideoModels` | `catalog.go:967` | yes ×3 |
| discovery `upsert` UPDATE | `internal/discovery/discovery.go:410-411` | yes ×3 |
| discovery `upsert` INSERT | `discovery.go:416` | yes ×3 |
| `Enrich` | `discovery.go:622` | yes ×3 |
| `UpdateManual` | `discovery.go:628` | yes ×3 |
| `AddManual` INSERT | `discovery.go:689` | yes ×3 |
| recycling-bin `Restore` | `discovery.go:776` | yes ×3 |

Reads scan `sql.NullInt64` and convert (`catalog.go:110/134-136`,
`discovery.go:556/587-589`, `discovery.go:211/225-227`, `proxy.go:3946`). No raw
bool write into these three columns remains — including admin handlers: there is
**no** direct `UPDATE models_catalog SET …` in `internal/handler` (only
`COUNT(*)` at `handler/admin.go:907`), and the `/provider-models` PUT goes
through `UpdateManual` (`handler/discovery.go:126`), which is covered.

Two residual risks, both real:

**P2-a — the guard is a convention, not a constraint.** Nothing in the schema
stops a future `SET image=?` with a Go bool. On SQLite it will silently succeed
(mixed `0`/`'false'` in the same column) and only Postgres will reject it at
runtime. Concrete hardening (additive, no downtime):

```sql
-- 030_capability_flag_domains.sql
ALTER TABLE models_catalog  ADD CONSTRAINT models_catalog_decision_chk CHECK (decision IN (0,1));
ALTER TABLE models_catalog  ADD CONSTRAINT models_catalog_image_chk    CHECK (image    IN (0,1));
ALTER TABLE models_catalog  ADD CONSTRAINT models_catalog_video_chk    CHECK (video    IN (0,1));
ALTER TABLE provider_models ADD CONSTRAINT provider_models_decision_chk CHECK (decision IN (0,1));
ALTER TABLE provider_models ADD CONSTRAINT provider_models_image_chk    CHECK (image    IN (0,1));
ALTER TABLE provider_models ADD CONSTRAINT provider_models_video_chk    CHECK (video    IN (0,1));
```
(Both dialects accept `CHECK`; on SQLite this needs a table rebuild, so ship it
as a *Postgres-only* guard executed from boot like `widenSpendCountersPostgres`,
or accept it as a schema-drift TODO. A cheaper, dialect-agnostic alternative that
actually catches the bug in CI: a `go/ast` test asserting that no `Exec`/`Query`
string containing `decision=?`, `image=?` or `video=?` has a bool-valued arg.)

**P2-b — `AddManual` silently drops `video`.** The request struct at
`internal/handler/discovery.go:68-86` declares `Decision` and `Image` but **not**
`Video`, so a manual video model added from the dashboard is stored with
`video=0` and is then rejected by `POST /v1/videos`
(`videos.go:341-357` → "is not a video model"). Field-level miss, same class of
bug as the bool/INTEGER trap:

```go
// internal/handler/discovery.go — add alongside Image
Video bool `json:"video"`
...
m := models.ProviderModel{ ..., Image: body.Image, Video: body.Video }
```

---

## 3. Legacy-ALTER mirroring: real, and it has already drifted

`applyLegacyAlters` (`db.go:854-880`) is the pre-1.6 path that must reproduce
every column a modern DB has, because `Migrate` runs it **after** the ledger
loop unconditionally (`db.go:603`) and legacy DBs skipped the versioned files.

Mirrored (added in this change): `decision` on both tables
(`db.go:874-875`).

**Not mirrored:** `image` (027) and `video` (029) — full grep for
`ADD COLUMN image|video` returns only `027_image_models.sql:12-13` and
`029_video_models.sql:9-10`, nothing in `db.go`. And `CREATE TABLE … video_jobs`
(028) has no counterpart in the `legacySchema` block (`db.go:489-596`) or in any
`apply*Alters`. This is a **latent drift bug**: the mitigation for it is that the
numbering gap (026-029 are new, so any existing ledger stops at ≤25) makes the
loop actually run those files; a legacy unversioned DB ends up with an empty
ledger, so it *does* run 001-029 and gets the columns. Today it works by
coincidence of ordering, not by construction. The moment someone replays
migrations on a modern DB (§5) or adds a 030 that touches `video_jobs`, the
divergence bites.

**Fix (P1-2, production impact: legacy/brownfield upgrades).**
1. Mirror both columns and the table in the legacy path:
```go
// db.go applyLegacyAlters cols[] — append
"ALTER TABLE models_catalog  ADD COLUMN image INTEGER DEFAULT 0",
"ALTER TABLE provider_models ADD COLUMN image INTEGER DEFAULT 0",
"ALTER TABLE models_catalog  ADD COLUMN video INTEGER DEFAULT 0",
"ALTER TABLE provider_models ADD COLUMN video INTEGER DEFAULT 0",
```
plus `execSchemaIdempotent(db, legacyVideoJobsDDL)` (the exact `CREATE TABLE IF
NOT EXISTS video_jobs …` + 3 indexes) in the `legacySchema` string.
2. **Better**: delete the duplication. `applyLegacyAlters` and friends are ~15
functions re-declaring migrations 002-024 by hand; every one of them is a drift
site. The runner already handles both fresh and legacy DBs (`legacySchema` +
replay). Replace the hand-written ALTER lists with a single
"replay all migrations with `duplicate column`/`already exists` tolerated" pass,
and add the missing tolerance to the ledger loop (§5). That removes the whole
class of bug instead of adding a fourth copy of it.

---

## 4. `video_jobs` — the table is fine, everything around it is missing

### 4a. Nothing ever deletes rows — unbounded growth (P1-1)

`grep -rn "video_jobs" --include=*.go` finds only five statements, all in
`internal/proxy/videos.go`:

- `INSERT` — `videos.go:699`
- `UPDATE` — `videos.go:715`
- `SELECT … WHERE id=?` — `videos.go:722`
- `SELECT … WHERE key_prefix=? ORDER BY created_at DESC LIMIT 100` — `videos.go:267`
- `DELETE FROM video_jobs WHERE id=?` — `videos.go:299` (**only** the explicit
  client `DELETE /v1/videos/{id}`)

There is **no** retention job, no `expires_at` sweep, no `created_at` pruning.
Compare the precedent that already exists in this codebase for exactly this
problem: `internal/proxy/response_store.go:151` prunes `response_turns` inline
on write ("Opportunistic expiry; indexed integer comparison"). Video jobs get
the opposite treatment: a row with `prompt`, `video_url`, `upstream_id` and
`error_message` is written per request and never removed, so the table grows
with *all* video traffic forever, including failed and abandoned jobs. On a busy
key that is unbounded — the classic silent-DB-growth incident.

Note the combination with §4b: `expires_at` is *stored* (`videos.go:712-716`,
populated from the provider's own expiry for the OpenAI dialect at
`videos.go:578-581`) but **never read for enforcement**. `VideoContent`
(`videos.go:225-243`) checks `videogen.IsTerminal(job.Status)` and then happily
re-polls / re-downloads, or serves the cached `video_url` long after the
provider's signed URL has expired — so a stale job returns a confusing 502
("could not resolve the generated video") instead of a clean 410. That is
finding P1-3 in §7.

**Fix.**
```sql
-- 030_video_jobs_retention.sql
CREATE INDEX IF NOT EXISTS idx_video_jobs_expires  ON video_jobs(expires_at);
CREATE INDEX IF NOT EXISTS idx_video_jobs_status   ON video_jobs(status, created_at);
```
```go
// opportunistic sweep, mirroring response_store.go:151 — call from VideoList
// and/or a 1h ticker in cmd/gateway/main.go
_, _ = h.DB.Exec(db.Q(`DELETE FROM video_jobs
    WHERE (expires_at IS NOT NULL AND expires_at < ?)
       OR (created_at < ? AND status IN ('completed','failed','cancelled'))`),
    now, now.Add(-videoJobRetention))   // e.g. 7*24h
```
Bind `time.Time` values, **not** strings (see §6). Index cost: two small
btree writes per job row, negligible versus the row itself.

### 4b. Missing indexes for the sweeps it needs (P1-1, same fix)

Verified with `EXPLAIN QUERY PLAN` on a migrated SQLite DB:

```
list    : SEARCH video_jobs USING INDEX idx_video_jobs_key (key_prefix=?)
          USE TEMP B-TREE FOR ORDER BY          <-- sort per list call
sweep   : SCAN video_jobs                        <-- full table scan
status  : SEARCH video_jobs USING INDEX idx_video_jobs_created (created_at<?)
```

Two concrete gaps:
- `idx_video_jobs_key(key_prefix)` does not cover `ORDER BY created_at DESC`, so
  every `GET /v1/videos` builds a temp b-tree over the key's whole history before
  `LIMIT 100`. With the retention fix the per-key row count stays small, but the
  index should be `(key_prefix, created_at DESC)` in one shot:
  ```sql
  DROP INDEX IF EXISTS idx_video_jobs_key;
  CREATE INDEX IF NOT EXISTS idx_video_jobs_key_created ON video_jobs(key_prefix, created_at DESC);
  ```
  (Postgres honours `DESC` and will do an index-only backward scan; SQLite
  ignores direction but still uses the two-column index to satisfy the ORDER BY.)
- `expires_at` has **no** index → the expiry sweep is a full scan of the fastest
  growing table in the schema (`028_video_jobs.sql:30,32-34`).

### 4c. `provider_id` is not a foreign key (P2-c)

`video_jobs.provider_id TEXT NOT NULL` (`028:14`) has no `REFERENCES
providers(id)`, unlike `provider_models` (`001_initial.sql:96`) and
`provider_model_exclusions` (`022:8`). Verified: `pragma_foreign_key_list('video_jobs')` → 0 rows.
Two consequences:
- `provider.Store.Delete` (`internal/provider/store.go:495`) cascades nothing,
  so deleting a provider leaves orphan `video_jobs` whose `VideoGet` path then
  fails with 502 "the provider that owns this video job no longer exists"
  (`videos.go:186-189`) — a permanent broken record.
- No referential integrity at all for the owning provider.

```sql
-- optional but recommended; ON DELETE SET NULL keeps the job readable
ALTER TABLE video_jobs ADD CONSTRAINT video_jobs_provider_fk
  FOREIGN KEY (provider_id) REFERENCES providers(id) ON DELETE CASCADE;
```
(Postgres-only; SQLite needs a table rebuild. Same staging as the CHECK
constraints in §2: add it in the `CREATE TABLE` for fresh DBs and mirror with a
boot-time guard for existing ones.)

### 4d. Primary key is a 32-hex TEXT — fine, but unvalidated (P3)

`id TEXT PRIMARY KEY` with `"video_" + strings.ReplaceAll(uuid.NewString(), "-", "")`
(`videos.go:169-170`) is correct for cross-dialect portability and matches every
other table (`001_initial.sql:39` etc. are all TEXT UUIDs). Providers do not
scope it (`upstream_id` carries the provider handle) — nothing to fix, but note
that `VideoDelete`/`VideoGet` take the id straight into a `WHERE id=?` with no
format check; `key_prefix` scoping (`videos.go:673-679`) is what actually
prevents cross-key access, and it is checked on GET, content and DELETE. Fine.

One ownership hole worth flagging to the backend reviewer: `videoJobVisible`
returns `true` when **either** side is empty (`videos.go:676-678`), so a job
created by a dashboard session (`key_prefix` captured from the header at
`videos.go:127`, which `middleware.attachGatewayKey` always sets —
`middleware.go:277`) is visible to *every* session, and a job whose
`key_prefix` was NULL is visible to every key. Not a schema bug, but the schema
allows `key_prefix NULL` (`028:15`) while no write path needs it — make it
`NOT NULL` with a backfill, or reject empty prefixes in `videoJobVisible`.

---

## 5. Migration numbering / versioning

**Good news:** the gap 026-029 is irrelevant to the runner. `Migrate` only ever
does `SELECT COUNT(*) … WHERE version=?` plus an ordered `for` over
`migrationsList()` (`db.go:411-467`); it never asserts contiguity and never
replays by "next integer". The gap in the *ledger* (a DB that stops at 025 gets
026..029 applied in order) is handled exactly as intended.

**`LatestMigrationVersion` (`db.go:150-156`)** is derived from the tail of
`migrationsList()`, so it cannot go stale by itself — but it is only *asserted*
in one place: `internal/db/pgsmoke/smoke_test.go:42-44`, `if want :=
db.LatestMigrationVersion(); n != want`. That test is skipped unless
`GATEWAY_PG_DSN` is set (`smoke_test.go:18-21`), and **CI never sets it**:
`.github/workflows/ci.yml:39-40` runs only `go test ./... -race -count=1` with no
`services: postgres`. Consequence: the only drift guard for the schema version is
**dead code in CI**, and so is the whole `TestCapabilityFlagWritesAgainstPostgres`
regression suite that this change specifically added. A developer can add
migration 030, forget the embed/`migrationsList` entry, and CI stays green while
every fresh production boot is at version 29 or fails.

**Fix (P1-4).** Two parts, both cheap:
1. A dialect-free test in `internal/db` that needs no database:
```go
func TestMigrationsListIsContiguousAndEmbedded(t *testing.T) {
    ms := migrationsList()
    for i, m := range ms {
        if m.version != i+1 { t.Fatalf("migration %d out of order at index %d", m.version, i) }
        if strings.TrimSpace(m.sql) == "" { t.Fatalf("migration %d is empty", m.version) }
    }
    // and: one file per version on disk
    files, _ := filepath.Glob("migrations/*.sql")
    if len(files) != len(ms) { t.Fatalf("%d .sql files but %d embedded migrations", len(files), len(ms)) }
}
```
   plus a `db.Open(":memory:")` assertion that `COUNT(*) FROM schema_migrations
   == LatestMigrationVersion()` (this needs no Postgres and would catch the
   missing-embed case immediately).
2. Add a `postgres:16` service to `ci.yml` and export `GATEWAY_PG_DSN`, so
   `pgsmoke` (including the new `capability_flags_test.go`) actually runs.
   Right now `internal/db/pgsmoke/capability_flags_test.go` — the explicit
   regression guard written for `pq: invalid input syntax for type integer:
   "false"` — is a no-op on every PR.

**Also (P1-2 adjacent): the ledger replay is not idempotent, and it fails
*dirty*.** I simulated a legacy unversioned DB (modern schema, emptied
`schema_migrations`) and re-ran `db.Migrate`. Result:

```
migration 4 failed: duplicate column name: last_login_at
```

`Migrate` then marks version 4 dirty (`db.go:456`) and returns, so `db.Open`
fails and **the gateway cannot boot** — and every subsequent boot short-circuits
with "migration 4 is marked dirty" (`db.go:425-426`) until an operator edits the
table by hand. The `legacySchema`/`applyLegacyAlters` fallbacks exist precisely
for this database shape, but they are unreachable because the loop aborts first.
The per-migration statement loop (`db.go:446-454`) should tolerate
duplicate-object errors the same way `execAlterIdempotent` does
(`db.go:798-812`) instead of aborting the whole boot. 026-029 are innocent here
(they are new) but they inherit the flaw: after such a failure, 026-029 never
run, so the DB ends up with an empty-ish ledger and no `video_jobs`.

---

## 6. Timestamp discipline on `video_jobs` (P2-d)

All writes bind `time.Time` (`videos.go:703-704, 717-718`), and SQLite stores
RFC3339Nano text; Postgres stores `TIMESTAMPTZ`. Reads scan `sql.NullTime`
(`videos.go:735-755`). That part is correct.

The trap is **string bounds**. Probes on a migrated SQLite DB:

```
created_at round-trip: "2026-10-17T17:37:52.856615826Z"   (typeof=text)
WHERE created_at > '2026-01-01'                    -> 1 row   (works: prefix compare)
WHERE created_at < '2026-01-01 00:00:00' (midnight row) -> 0 rows (MISSED)
WHERE created_at < ? (time.Time 2026-01-01T00:00:01Z)   -> 1 row   (correct)
```

So a retention/expiry sweep written with a SQL string literal is silently wrong
for the exact boundary row on SQLite (space < 'T'), and on Postgres it depends
on the session `DateStyle`. **Always bind `time.Time`** for any `created_at` /
`expires_at` comparison — same rule the existing
`response_store.go:151`/`167` code already follows. If any new query needs a
"day bucket" on `video_jobs`, use `db.DateBucketExpr`/`db.HourBucketExpr`
(`db.go:217-235`) rather than hand-rolled `substr`/`date()`.

---

## 7. Seed idempotency — and it clobbers operator edits (P0-2, correctness of billing)

Idempotency in the *row-count* sense holds: the three seeds are
`INSERT … ON CONFLICT(id) DO UPDATE SET` over every non-key column
(`catalog.go:808/820`, `931/942`, `956/967`), and the existing tests assert the
row count is unchanged on re-seed (`decision_seed_test.go:53-61`,
`image_seed_test.go:59+`). No duplicates, no PK churn.

But "idempotent" here means **the curated registry wins, forever, on every
boot**:

- `cmd/gateway/main.go:107` calls `catalogStore.SeedDecisionModels()` on **every
  boot**.
- `catalog.go:793-799` calls all three seeds after **every** models.dev sync,
  and the syncer runs on a `24 * time.Hour` ticker
  (`cmd/gateway/main.go:216-228`) plus on demand from `POST /catalog/sync`
  (`handler/catalog.go:122`).

I instrumented `SeedDecisionModels` against a real in-memory DB:

```
id             = cloudflare/clef
operator price = 2.4000   (input_cost×10, i.e. an operator price override)
after reseed   = 0.2400   (clobbered=true)
```

Concretely: the `DO UPDATE SET` list includes `input_cost`, `output_cost`,
`context_window`, `name`, `description`, `family`, `modalities` and the three
capability flags (`catalog.go:808` and the mirrored lists at 931/956). Any
operator edit to those fields on a curated row is reverted within 24 hours (or
immediately on a dashboard "Sync" click). That is a **spend-attribution bug, not
cosmetics**: `CostForSplit` (`catalog.go:906-923`) and
`/v1/models → image_cost`/`video_cost` all read these columns, so a gateway that
negotiated a different rate, or an operator correcting a wrong curated price,
silently reverts to the baked-in number. A wrong `input_cost` under-reports
spend and lets a key blow its budget. It also silently overwrites an operator's
capability correction (e.g. un-flagging a mis-classified row → the row is
re-flagged `decision=1` at the next boot and becomes routable to
`/v1/systemone`).

**Fix (P0-2).** Make the seeds *insert-only* and never fight the operator:
```sql
INSERT INTO models_catalog (id, provider, name, …)
VALUES (?,?,?, …)
ON CONFLICT(id) DO NOTHING;           -- curated rows are a seed, not a source of truth
```
and if refresh of curated rows is genuinely wanted, guard it behind an explicit
marker (`system_config('curated_seed_version')`) so a boot with an unchanged
registry writes nothing, and only *new* curated models are inserted. If a
price refresh is required, restrict `DO UPDATE SET` to columns the operator
cannot edit — never `input_cost`/`output_cost`, and never the capability flags.

Note the same `DO UPDATE`-everything pattern is on the models.dev sync
(`catalog.go:745`), but for that path "upstream is the source of truth" is
defensible. The curated seeds are the ones that should yield.

**Also (P3): inconsistent seed coverage.** `SeedImageModels` and
`SeedVideoModels` are only reachable via sync (`catalog.go:797-799`), while
`SeedDecisionModels` also runs at boot (`main.go:107`). A fresh gateway that has
never synced (offline / models.dev down) therefore lists decision models but not
image/video models, even though both registries are embedded in the binary. Make
all three boot-seeded (or none) — currently it's an accident of history.

**Also (P3): `SeedImageModels` writes `input_cost`/`output_cost` from
`im.InputCostPerM/OutputCostPerM` (`catalog.go:940`)** while the doc comment says
image models bill per image and their token costs are zero; probe shows
`openai/gpt-image-1 input_cost=0.0000`, so the registry simply carries zeros —
fine today, but the code intends "per-token columns are meaningless for image
rows", and a future registry entry with a non-zero `InputCostPerM` would silently
start mis-billing image jobs through `CostForSplit`. Either write literal `0, 0`
or add a comment/assertion in the registry loader.

---

## 8. pgsmoke review

- `main_test.go:33-60` — `resetDatabase` truncates every `public` table except
  `schema_migrations` with one `TRUNCATE … RESTART IDENTITY CASCADE`. Correct and
  fast; but note it **keeps the ledger**, so a schema change is only picked up
  when the test database is dropped. A developer with a long-lived `gwtest`
  database can be running tests against a schema one or more migrations behind
  (`000`-level drift) and get false greens. Add a `SELECT MAX(version) FROM
  schema_migrations` check against `db.LatestMigrationVersion()` inside
  `resetDatabase` and `DROP SCHEMA public CASCADE` when they differ.
- `smoke_test.go:42-44` — good: derived, not hardcoded (the comment about the
  stale "24" is exactly the right lesson). See §5 for why it never runs.
- `capability_flags_test.go` — the right test for the right bug, and it covers
  seed → sync → AddManual → UpdateManual → Enrich → Delete/Restore. Two gaps:
  (i) it never exercises `video_jobs` against Postgres, so the `DATETIME →
  TIMESTAMPTZ` + `sql.NullTime` scan path for `completed_at`/`expires_at` is
  unverified on PG (this is the one place I could not test locally);
  (ii) it asserts flags but not the `key_prefix`-scoped list query. Add a small
  `TestVideoJobsLifecyclePostgres` doing insert → select-by-id → select-by-key
  ordered by `created_at DESC` → update → delete, which would simultaneously
  catch a missing `TIMESTAMPTZ` translation and a `Rebind` regression.
- `stores_test.go:107-153` — the `LOWER(...) LIKE LOWER(...)` umans matching test
  is solid; unrelated to this change but confirms the PG suite is meaningful
  when it runs.

---

## 9. Ordered remediation plan (safe on a live database)

Additive first, backfill, then tighten — nothing destructive without a staged
plan:

1. **030_video_jobs_retention.sql** (additive, seconds of lock, no write block)
   ```sql
   CREATE INDEX IF NOT EXISTS idx_video_jobs_expires      ON video_jobs(expires_at);
   CREATE INDEX IF NOT EXISTS idx_video_jobs_status_created ON video_jobs(status, created_at);
   CREATE INDEX IF NOT EXISTS idx_video_jobs_key_created  ON video_jobs(key_prefix, created_at DESC);
   ```
   then `DROP INDEX idx_video_jobs_key` in 031 *after* 030 is live on all nodes.
2. **031_video_jobs_fk.sql** — `ADD CONSTRAINT … FOREIGN KEY(provider_id)
   REFERENCES providers(id) ON DELETE CASCADE NOT VALID`, then
   `VALIDATE CONSTRAINT` in a later window (PG; the `NOT VALID`+`VALIDATE` split
   takes only a `SHARE ROW EXCLUSIVE` lock, so writes proceed).
3. **Backfill** `key_prefix` NULLs (`UPDATE video_jobs SET key_prefix='' WHERE
   key_prefix IS NULL`) *before* any `SET NOT NULL`.
4. **Retention sweep** in code (indexed, `time.Time`-bound) + a boot/hourly
   ticker; expose the retention window in config.
5. Seed fix (code, no DDL): `DO NOTHING` for curated rows.
6. Mirror `image`/`video` + `video_jobs` into the legacy path — or, better,
   delete the hand-maintained ALTER lists.
7. CI: Postgres service + `GATEWAY_PG_DSN`; dialect-free migration-contiguity
   test.

Rollback for 1-3 is `DROP INDEX` / `DROP CONSTRAINT` (instant, no data loss).
Nothing in this plan rewrites or deletes rows except step 4, which deletes only
rows the retention policy is designed to delete.

---

## Verdict

**NEEDS WORK**

The four migrations are individually correct on SQLite and Postgres: correct
column types (`INTEGER` flags, `DATETIME`→`TIMESTAMPTZ`), `IF NOT EXISTS`
indexes, portable `LIMIT/OFFSET`, no `RETURNING`, and `Rebind` covers every new
query with no `?`-in-literal hazards. `BoolInt` is now used on **every** write
path to `decision`/`image`/`video`, including seeds, discovery upserts, manual
add/update and the recycling-bin restore — the trap is closed in code. What is
*not* production ready is everything surrounding the schema: the curated seeds
silently overwrite operator pricing on every boot and every 24h sync, `video_jobs`
has no retention and no `expires_at`/status index despite the expiry sweeps it
needs, the legacy-ALTER path has already drifted for `image`/`video`, and the
Postgres regression suite that guards all of this never runs in CI.

Blocking, in severity order:

1. **P0 — `video_jobs` grows without bound; nothing ever deletes rows.**
   Only statement that removes a row is the client's explicit
   `DELETE /v1/videos/{id}` (`videos.go:299`). No expiry sweep, no retention,
   no pruning — unlike `response_turns`, which prunes inline
   (`response_store.go:151`). `expires_at` is written (`videos.go:716`) and then
   never read for enforcement, so expired jobs also return 502s from
   `VideoContent` instead of a clean 410. Fix: indexed sweep
   (`idx_video_jobs_expires`) bound with `time.Time`, called on a ticker +
   opportunistically in `VideoList`.
2. **P0 — curated seeds clobber operator edits and pricing.**
   `cmd/gateway/main.go:107` and `catalog.go:793-799` re-run
   `SeedDecisionModels`/`SeedImageModels`/`SeedVideoModels` on every boot and
   after every (daily) sync, and their `ON CONFLICT DO UPDATE` lists include
   `input_cost`, `output_cost`, `context_window` and the three capability flags
   (`catalog.go:808, 931, 956`). Proven: an operator `input_cost` override on
   `cloudflare/clef` reverted from 2.4000 to 0.2400 on re-seed. This corrupts
   spend attribution and budget enforcement. Fix: `ON CONFLICT(id) DO NOTHING`
   (insert-only seed), or gate refresh behind an explicit seed-version marker.
3. **P1 — legacy-ALTER mirroring has drifted (latent boot-breaker).**
   `applyLegacyAlters` mirrors `decision` (`db.go:874-875`) but not `image`
   (027) or `video` (029), and `video_jobs` (028) has no counterpart in
   `legacySchema` (`db.go:489-596`). Relatedly, replaying migrations on an
   already-migrated DB aborts dirty at migration 4 (`duplicate column name:
   last_login_at`) and then blocks every subsequent boot
   (`db.go:425-426, 456`) — verified by simulation. Fix: mirror the columns/table
   *and* make the per-migration statement loop tolerate duplicate-object errors,
   or delete the hand-maintained ALTER lists entirely.
4. **P1 — missing indexes for the queries the table needs.**
   `EXPLAIN QUERY PLAN` shows `GET /v1/videos` does a temp b-tree sort
   (`idx_video_jobs_key` doesn't cover `ORDER BY created_at DESC`), the expiry
   sweep is a full table scan (no `expires_at` index), and there is no
   `status` index for stale-job reconciliation. Fix: 030 above.
5. **P1 — the only schema-version and Postgres-bool guards never run in CI.**
   `smoke_test.go:42-44` (`LatestMigrationVersion`) and the entire
   `pgsmoke/capability_flags_test.go` suite skip unless `GATEWAY_PG_DSN` is set
   (`smoke_test.go:18-21`), and `.github/workflows/ci.yml:39-40` runs no
   Postgres service. A missing `go:embed`/`migrationsList` entry for a future
   030 would pass CI and break every fresh boot. Fix: add a `postgres:16`
   service + DSN to CI, and a dialect-free contiguity/embed test in
   `internal/db`.
6. **P2 — `video_jobs.provider_id` is not a foreign key.**
   `pragma_foreign_key_list('video_jobs')` → 0 rows, unlike
   `provider_models`/`provider_model_exclusions`. Deleting a provider orphans
   jobs that then 502 forever (`videos.go:186-189`). Fix: FK with
   `ON DELETE CASCADE`, staged as `NOT VALID` → `VALIDATE`.
7. **P2 — `AddManual` silently drops the `video` flag.**
   Struct at `handler/discovery.go:68-86` has `Decision` and `Image` but no
   `Video`, so dashboard-added video models land with `video=0` and are then
   rejected by `POST /v1/videos`. Fix: add the field and pass it through.
8. **P2 — capability flags have no DB-level domain constraint.**
   `INTEGER` flags accept any value; on SQLite a Go-bool write succeeds silently
   and only Postgres rejects it. Fix: `CHECK (decision IN (0,1))` etc.
   (Postgres-side guard), or an AST test asserting no bool-valued `?` is bound to
   these columns.
9. **P2 — timestamp bounds must never be string literals.** Proved on a migrated
   SQLite DB that `created_at < '2026-01-01 00:00:00'` misses the midnight row
   (`'T'` vs space), while a bound `time.Time` matches it. Any retention/expiry
   query must bind `time.Time` and use `db.DateBucketExpr`/`HourBucketExpr` for
   bucketing.
10. **P3 — `key_prefix` is nullable but semantically required**, and
    `videoJobVisible` returns `true` when either side is empty
    (`videos.go:676-678`), so dashboard-session jobs are visible to every
    session. Backfill `''` then `SET NOT NULL`; reject empty prefixes.
11. **P3 — inconsistent boot-seeding**: only `SeedDecisionModels` runs at boot
    (`main.go:107`); image/video seeds depend on a successful models.dev sync, so
    an offline first boot lists decision models but not image/video models.
12. **P3 — `pgsmoke.resetDatabase` keeps the ledger**, so a long-lived test
    database can silently test an older schema than the binary expects; it
    should drop/recreate the schema when
    `MAX(schema_migrations.version) != db.LatestMigrationVersion()`.
13. **P3 — `SeedImageModels` writes `im.InputCostPerM/OutputCostPerM`** into
    token-cost columns the image path never uses; harmless today (registry
    values are 0) but a future non-zero entry would mis-bill through
    `CostForSplit`. Write literal `0, 0`.
