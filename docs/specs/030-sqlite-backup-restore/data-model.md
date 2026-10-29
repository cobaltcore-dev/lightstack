# Data Model: SQLite backup, integrity check and restore

**Feature**: `030-sqlite-backup-restore` (cloud-storage#600) | **Date**: 2026-10-28

**No database schema change, no migration, no new tables or columns.** This feature adds no persistent application data. The "entities" here are node-local **files and directories** plus the two pre-existing migration facts the version-ceiling guard reads. Everything below is derived from existing state.

## File & directory artifacts

### Backup file
- **What**: a single standalone SQLite database file, a consistent snapshot of the live database at the moment `VACUUM INTO` ran.
- **Location**: `<file>` if the operator gave one; otherwise `paths.BackupsDir()/lightstack-<UTC timestamp>.db`.
- **Name (default)**: `lightstack-<YYYYMMDDThhmmssZ>.db`, UTC, e.g. `lightstack-20261028T143000Z.db`.
- **Permissions**: `0600`, owned by the process user (root in the chart) — the live database's mode (FR-003a).
- **Side files**: none. `VACUUM INTO` writes one file with no `-wal`/`-shm` (verified, research R2).
- **Lifecycle**: created by `lightstack-migrate backup`; never overwritten (`VACUUM INTO` refuses an existing target — FR-003); retained across teardown (FR-004); deleted only by the operator.
- **Invariants**: self-contained; passes `PRAGMA integrity_check`; opens read-back-equal to the source's committed data.

### Backups directory
- **What**: `paths.BackupsDir()` = `filepath.Join(paths.Base(), "backups")` — new helper, sibling of `paths.DBDir()` (`<base>/db`).
- **Base**: `paths.Base()` = `cfg.StatePath` = the chart's `realMode.hostStoragePath` (default `/var/lib/lightstack`). The `backup` handler must call `paths.SetBase(cfg.StatePath)` before resolving it (the migrate CLI does not set the base otherwise).
- **Permissions**: `0750`, created on first backup (FR-003a).
- **Placement**: **outside** `db/`, so `dataRetentionPolicy: delete` keeps it — it is absent from `purgeLocalStorage`'s list and stays absent (FR-004, locked by a teardown test). Real mode only; it lives inside the already-mounted `host-storage` volume. Stub mode is not backed up (FR-004a).

### Live database side files (`-wal`, `-shm`)
- **What**: the live database's write-ahead log and shared-memory index — `lightstack.db-wal`, `lightstack.db-shm` in `<base>/db/`.
- **Role in this feature**: the restore procedure **deletes** them before putting a backup in place, so a stale log is not replayed onto the restored snapshot (FR-007, research R10). They are not produced by a backup and are never part of one.

## State the version-ceiling guard reads (no new storage)

`RefuseVersionAhead` (FR-008, research R8) compares two existing facts; it writes nothing:

| Input | Source |
|---|---|
| Database schema version | `schema_migrations` table via `golang-migrate` `m.Version()` (same table both backends) |
| Highest version the binary knows | the migration source (`migrations/` via the per-backend view), walked with the source driver's `First`/`Next` |

- **Rule**: if *database version* > *highest known version* → refuse to start / to migrate, with a message naming both versions and the cause (a newer Lightstack created this database).
- **Non-trigger**: *database version* ≤ *highest known* → proceed; an under-version database is forward-migrated by the existing `MigrateUp` (FR-008 first half, research R9). The existing `RefuseBelowBaseline` continues to guard a below-baseline target.
- **Backend scope**: backend-neutral — the same arithmetic protects a PostgreSQL deployment downgraded under an ahead database.

## Relationships

```text
cfg.StatePath ── paths.SetBase ──▶ paths.Base()
                                     ├─ paths.DBDir()      = <base>/db        (live DB + -wal/-shm; purged by delete)
                                     └─ paths.BackupsDir() = <base>/backups   (backup files; KEPT by delete)   ← new

lightstack.db ──VACUUM INTO──▶ backup file (0600, single file)   [online, consistent]
backup file ──restore (stop; rm -wal/-shm; swap; check; start)──▶ lightstack.db
schema_migrations.version  vs  max(migration source)  ──▶ RefuseVersionAhead (start-time gate)
```

No other entities are involved. The backup is a byte-for-byte-consistent copy of the whole database; it is not decomposed into application entities, and no per-tenant scoping applies (it is a node-admin artifact, not an API resource).
