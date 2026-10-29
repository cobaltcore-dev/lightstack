# Contract: `lightstack-migrate` backup & check commands

**Feature**: `030-sqlite-backup-restore` (cloud-storage#600) | **Date**: 2026-10-28

The CLI is the feature's interface. Both commands read the database from `database.url` in the config (or `LIGHTSTACK_DB_URL`), exactly like the existing subcommands. Exit code 0 = success, non-zero = failure (fail-fast, no retries).

---

## `lightstack-migrate backup [<file>]`

Take a consistent, standalone copy of the live SQLite database while Lightstack keeps serving requests.

### Inputs
| Input | Required | Meaning |
|---|---|---|
| `<file>` | no | Destination path for the backup. If omitted, defaults to `<state_path>/backups/lightstack-<UTC timestamp>.db`. |
| `-config <path>` | no | Config file (existing flag; default `config/lightstack.yaml`). |

### Behaviour
- **SQLite only.** If the URL selects PostgreSQL, exit **non-zero** with a message naming `pg_dump`; take no action. (FR-006)
- Resolve the target: explicit `<file>`, else `paths.SetBase(cfg.StatePath)` then `paths.BackupsDir()/lightstack-<UTC timestamp>.db`. (FR-002, R3)
- Ensure the default backups directory exists, mode `0750`. (FR-003a)
- Open a must-exist connection to the live database and run `VACUUM INTO <target>`. The live service continues serving writes; the copy is a consistent committed snapshot with no `-wal`/`-shm`. (FR-001, R2)
- If the target already exists, `VACUUM INTO` fails; exit **non-zero** with a message naming the target. (FR-003)
- On success, tighten the backup file to `0600` and print the final path. (FR-003a)

### Exit codes
| Code | Case |
|---|---|
| 0 | Backup written; path printed. |
| non-zero | PostgreSQL URL (names `pg_dump`); target exists; missing/unwritable database or directory; `VACUUM INTO` error. |

### Example
```
$ lightstack-migrate backup
✅ Backup written to /var/lib/lightstack/backups/lightstack-20261028T143000Z.db
$ lightstack-migrate backup /mnt/snap/ls.db
✅ Backup written to /mnt/snap/ls.db
$ lightstack-migrate backup /mnt/snap/ls.db         # again
Backup failed: backup target /mnt/snap/ls.db already exists; each backup needs a new file   (exit 1)
```

---

## `lightstack-migrate check`

Report the integrity of the SQLite database the URL names. Read-only; runs against a live, serving database with no downtime, and is the gate in the restore procedure. (FR-005, Clarifications)

### Behaviour
- **SQLite only.** PostgreSQL URL → exit **non-zero** naming `pg_dump`. (FR-006)
- Open a must-exist connection and run `PRAGMA integrity_check`.
  - Single row `ok` → then read the schema version from `schema_migrations`. None (no table, or no row) → exit **non-zero**: `<path> holds no Lightstack schema (no migration version): it is empty or not a Lightstack database` (FR-005a). Otherwise exit **0** printing `✅ Integrity check passed (schema version N)`.
  - One or more problem rows → print them; exit **non-zero**. (The bare `sqlite3` CLI exits 0 on problem rows — the exit code is this command's interpretation, not a child process's — R6.)
  - The statement itself errors (e.g. `SQLITE_CORRUPT`, "database disk image is malformed") → print it; exit **non-zero**.

### Exit codes
| Code | Case |
|---|---|
| 0 | `integrity_check` returned exactly `ok` and a schema version is recorded. |
| non-zero | PostgreSQL URL (names `pg_dump`); missing database; problem rows; a `SQLITE_CORRUPT`/other statement error; no Lightstack schema (empty or foreign file). |

### Example
```
$ lightstack-migrate check
✅ Integrity check passed (schema version 74)
$ lightstack-migrate check            # damaged database
Check failed: /var/lib/lightstack/db/lightstack.db: integrity check failed:
  Tree 2 page 8: btreeInitPage() returns error code 11
  ...                                 (exit 1)
```

---

## Newer-database refusal at startup (not a subcommand)

The server's startup migration and `lightstack-migrate up` (both `MigrateUp`) refuse a database whose
recorded version is above the newest migration this binary carries — a backup restored from a newer
Lightstack (FR-008, research R8):

```
the database is at migration version 75, but this Lightstack knows versions up to 74: it was written
by a newer Lightstack. Run that version, or restore a backup this one wrote
```

golang-migrate already stopped on such a database; the change is that the refusal now says why. An
older database is forward-migrated as before. Backend-neutral.

## Non-goals (negative contract)
- No overwrite of an existing backup target — ever (FR-003).
- No API/HTTP surface; these are CLI-only node-admin operations.
- No action on PostgreSQL beyond the `pg_dump` refusal.
- No scheduled/automatic backups, no pruning of old backups (operator-owned).
