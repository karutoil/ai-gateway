# Migrations (Phase 1.6 — versioned)

- `001_initial.sql` = baseline matching current `db.Migrate` idempotent schema.
- `002_hardening.sql` = Phase 1.6 buffer: `schema_migrations` + `audit_logs` + budget columns (nullable).
- `003+` will be Phase 2/2.5/3 (usage_rollups, organizations, memberships, etc.).

Runner (not yet wired, stub in 1.6): ordered apply + row in `schema_migrations`. Existing `db.Migrate` already handles idempotent ALTERs so both fresh and migrated DBs stay green whether runner or legacy path is used.

Postgres note: TEXT PK stays TEXT (UUID strings), DATETIME → TIMESTAMPTZ, BOOLEAN → BOOL, BLOB → BYTEA.
