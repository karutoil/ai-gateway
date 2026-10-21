# Migrations (Phase 1.6 — versioned)

- `001_initial.sql` = baseline matching current `db.Migrate` idempotent schema.
- `002_hardening.sql` = Phase 1.6 buffer: `schema_migrations` + `audit_logs` + budget columns (nullable).
- `003+` will be Phase 2/2.5/3 (usage_rollups, organizations, memberships, etc.).

Runner (not yet wired, stub in 1.6): ordered apply + row in `schema_migrations`. Existing `db.Migrate` already handles idempotent ALTERs so both fresh and migrated DBs stay green whether runner or legacy path is used.

Postgres note: TEXT PK stays TEXT (UUID strings), DATETIME → TIMESTAMPTZ, BOOLEAN → BOOL, BLOB → BYTEA.

## `schema_migrations` is not sufficient — columns are reconciled at boot

A version stamped in `schema_migrations` only proves the migration transaction
committed, not that its DDL is present. `request_logs` is the column that makes
this dangerous: `internal/proxy` names its columns explicitly in the INSERT, so
one missing column fails **every** request log write (one error per request, row
dropped) instead of degrading a single field. The dashboard, `/api/logs` and the
spend rollups then read as if no traffic ever happened while the proxy is still
serving 200s.

How a stamped version can lack its column: migration `020` rebuilds `lb_rules`
via `CREATE lb_rules_v2 → INSERT SELECT → DROP TABLE lb_rules → RENAME`, so DDL
replayed or restored around that window can leave a table missing columns a
later version considers applied. Restoring from a file copy taken mid-migration
does the same.

`repairRequestLogsColumns` (called from `Migrate`) therefore introspects
`request_logs` at boot and issues an additive `ALTER TABLE ... ADD COLUMN` for
each column in `requestLogColumns` that the live table lacks — keyed off the
actual columns, not `schema_migrations`. It is additive, a no-op on healthy
databases, and safe to run every boot.

**Adding a `request_logs` column:** add the migration file as usual, then add
the same column to `requestLogColumns` in `internal/db/db.go`. The proxy INSERT
and the repair set must agree — `TestRepairRequestLogsColumnsCoversInsertColumnSet`
fails if they drift.
