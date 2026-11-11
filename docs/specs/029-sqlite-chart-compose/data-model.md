# Data Model: SQLite backend, part 2 — Helm chart and Docker Compose

**Feature**: `029-sqlite-chart-compose` · **Date**: 2026-10-28

This feature changes **no database schema** and adds **no migration** — the SQLite schema and the `reserved_by` columns are part 1 (SPEC-028). The "data model" here is the chart's configuration surface, the rendered artifacts it produces per backend, and the SQLite on-disk layout the chart provisions. The entities below are what the planning and tasks phases act on.

## Chart value surface (inputs the chart reads)

| Value | Type | Role in this feature |
|---|---|---|
| `database.url` | string | The one backend selector. `sqlite:///<abs path>` ⇒ SQLite; `postgres://`/`postgresql://` or empty ⇒ PostgreSQL. Scheme parsed by `lightstack.dbBackend` (R1). |
| `database.existingSecret` / `database.existingSecretKey` | string | PostgreSQL-only. Combined with a `sqlite://` `database.url` ⇒ render-time failure (FR-003a). |
| `secrets.existingSecret` | string | Pre-rendered full `lightstack.yaml`. Combined with a `sqlite://` `database.url` ⇒ render-time failure (FR-003a). |
| `realMode.hostStoragePath` | string (`^/.+`) | The node storage root. A SQLite path must be under it (FR-002). The db directory is derived from `database.url`, not this value. Also the `init-host-storage` base and the real-mode `host-storage` mount. |
| `realMode.enabled` | bool | Chooses whether the db directory is covered by the existing `host-storage` mount (real) or needs a narrow db-dir hostPath (stub) (R9). |
| `deploymentMode` | enum `single-node`/`multi-node` | SQLite requires `single-node` (FR-009). |
| `replicaCount` | int | SQLite requires `≤ 1` (FR-009). |
| `nodeSelector` / `affinity` | map / map | SQLite requires one of them to pin the pod (FR-008). Also the inputs to the shared `lightstack.scheduling` helper (R11). |
| `tolerations` | list | Part of the shared `lightstack.scheduling` helper. |
| `dataRetentionPolicy` | enum `retain`/`delete` | `delete` renders the purge Job (which now removes `db/` last); `retain` leaves `db/` on the node. |

## Derived values (computed in templates)

| Name | Definition | Used by |
|---|---|---|
| backend | `hasPrefix "sqlite:" database.url` ? `sqlite` : `postgres` | `LIGHTSTACK_DB_BACKEND` env; every SQLite gate; storage/strategy rendering |
| db file path | `trimPrefix "sqlite://" database.url` | the under-root validation (FR-002) |
| db directory | `dir <db file path>` | the hostPath volume/mount (FR-004) |

## Rendered artifacts per backend

| Artifact | PostgreSQL render | SQLite render |
|---|---|---|
| `LIGHTSTACK_DB_BACKEND` env (workload + purge) | `postgres` | `sqlite` |
| Deployment `strategy` | `Recreate` (real) / `RollingUpdate` (stub) | `Recreate` (both modes) |
| Workload hostPath volume (stub) | none | `hostPath: <db dir>`, `type: DirectoryOrCreate` (narrow — least privilege) |
| Purge-Job hostPath volume (stub) | none | `host-storage` at `<hostStoragePath>` (state root, so `RemoveAll(db/)` works — FR-011) |
| Database directory in real mode | n/a | inside the existing `host-storage` mount; `db` in the `init-host-storage` mkdir list |
| PVC | none | none |
| Purge-Job scheduling | `= Deployment` (new shared helper) | `= Deployment` (new shared helper) |
| Render-time refusals | none fire | path-outside-root / no-node-pin / non-single-node / replicas>1 / sqlite+Secret |

## SQLite on-disk layout (what the chart provisions and the teardown removes)

```
<realMode.hostStoragePath>/            # the node storage root (hostPath)
  db/                                  # the mounted database directory (paths.DBDir())
    lightstack.db                      # the database (binary creates it 0600)
    lightstack.db-wal                  # WAL — present while open / after a hard kill
    lightstack.db-shm                  # shared-memory index — same
  images/ volumes/ instances/ ...      # other node-local trees (real mode)
```

- **Creation**: real mode — `init-host-storage` pre-creates `db/`; stub mode — `DirectoryOrCreate` plus the binary's `MkdirAll` (SPEC-028 FR-011).
- **Lifecycle under `delete`**: the purge enumerates from the open database, the chart/Go close it, then `purgeLocalStorage` removes the trees with `db/` **last** (R12/FR-013).
- **Lifecycle under `retain`**: nothing runs; `db/` stays. A reinstall on the same node opens `lightstack.db` and replays any `-wal`/`-shm` (FR-014).
- **The three db files are a unit** — never moved or copied individually (FR-014).

## Non-entities (explicitly unchanged)

- No table, column, index, constraint or seed row changes.
- `pkg/paths/paths.go` is unchanged — `DBDir()` already exists.
- The binary's URL parsing, backend check, VXLAN refusal and must-exist open are unchanged (SPEC-028).
