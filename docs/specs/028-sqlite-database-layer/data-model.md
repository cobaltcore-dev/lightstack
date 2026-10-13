# Data Model: SQLite backend, part 1

Phase 1 output for [spec.md](spec.md). Counts and the file:line references below were measured on `feature/598` @ `278d106` with the commands in [research.md](research.md); the line numbers are as of that commit and may have shifted at HEAD.

## 1. Schema change on both backends

One migration, a plain file that runs unchanged on both backends (FR-033, research R13):

```sql
-- migrations/074_reservation_node.up.sql
ALTER TABLE volumes   ADD COLUMN reserved_by TEXT;
ALTER TABLE snapshots ADD COLUMN reserved_by TEXT;

-- migrations/074_reservation_node.down.sql
ALTER TABLE snapshots DROP COLUMN reserved_by;
ALTER TABLE volumes   DROP COLUMN reserved_by;
```

| Column | Type | Null | Default | Meaning |
|---|---|---|---|---|
| `volumes.reserved_by` | `TEXT` | yes | none | Node name of the Lightstack node that reserved the row. Set when the row is inserted in `creating`; kept afterwards for diagnosis. `NULL` on every row written before 074 and on rows written by paths that never reserve (manage, backup restore, transfer accept). |
| `snapshots.reserved_by` | `TEXT` | yes | none | Same, for snapshots. |

Why this shape: no default and no `NOT NULL`, so the same `ADD COLUMN` is legal on SQLite (which refuses non-constant defaults and `NOT NULL` without a default on `ADD COLUMN`) and on PostgreSQL; `DROP COLUMN` works on SQLite ≥ 3.35 for a column with no index or constraint. No index: the startup sweep reads at most a handful of rows (`status = 'creating'`).

The SQLite baseline is written at **073** (the newest migration on `main`), so 074 is the first file both backends apply through the shared path (US5 AS 1–2).

## 2. Reservation lifecycle

Volume create, boot from a new volume and snapshot create (FR-032 – FR-036):

```text
                 tx1: quota lock + usage + INSERT (status=creating, reserved_by=<node>)
   (no row) ───────────────────────────────────────────────────────────────▶ creating
                                                                                │
         backing created outside any transaction (qemu-img / rbd / Ceph snapshot)
                                                                                │
           ┌────────────────────────────────┬───────────────────────────────────┤
           │ tx2: UPDATE … SET available     │ backing failed                     │ process stopped
           │ WHERE id=$1 AND status=creating │ DELETE … WHERE status=creating     │ (crash, kill, power loss)
           ▼                                 ▼ + reclaim partial backing           ▼
       available                          (no row)                  next start of the SAME node:
                                                                    creating ─▶ error, backing kept until deleted
```

| Transition | Where | Guard | Visible to clients |
|---|---|---|---|
| none → `creating` | first transaction, under the project's quota lock | usage incl. `creating` rows + request ≤ limit | listed and readable as `creating` while the backing is built |
| `creating` → `available` | second, single-statement write | `status = 'creating'` (a row removed meanwhile is not resurrected; its backing is reclaimed and the request fails) | the create's own response is unchanged (FR-036) |
| `creating` → removed | failure path | `status = 'creating'` | the create fails with today's error |
| `creating` → `error` | startup sweep (research R13) | `reserved_by = <this node's name>` | after a restart, as upstream's volume service does per host; the backing stays until the volume or snapshot is deleted |

While a row is `creating`, delete and extend of the volume, and delete of the snapshot, are refused with `400` as upstream refuses them (contract [block-storage-reservation.md](contracts/block-storage-reservation.md)). Attach already requires `available`.

Quota usage already counts every row whose status is not `deleted` (`internal/cinder/quotas.go:74,90,486-491`), so reservations count without a query change.

**Node name** (clarification 2026-10-12): the value of `LIGHTSTACK_NODE_NAME` when set, the host name otherwise; resolved once at startup and passed to Cinder's constructor. Not `compute.node_id`, whose `auto` value is a new UUID per start (`internal/compute/node_registry.go:29-31`).

## 3. Values on SQLite

The SQLite implementation converts at the boundary, so callers bind and scan the same Go types on both backends (FR-019 – FR-023). Column counts are from the existing migrations.

| PostgreSQL type (columns) | SQLite storage (STRICT) | Bound from Go | Scanned into Go |
|---|---|---|---|
| `UUID` (175), `UUID[]` elements | `TEXT`, canonical lower-case form, never rewritten | `string`, `uuid.UUID` (its `Value()`) | `string` |
| `TIMESTAMP` (125), `TIMESTAMPTZ` (2) | `TEXT`, UTC, `2006-01-02T15:04:05.000000Z` (fixed width, so string order = time order) | `time.Time`, `*time.Time` → formatted by the layer | `time.Time`, `*time.Time` ← parsed by the layer |
| `JSONB` (11) | `TEXT` holding JSON | map/struct → `json.Marshal`; `[]byte`/`json.RawMessage`/`string` → as text | `[]byte`, `string`; map/struct ← `json.Unmarshal` |
| `TEXT[]` / `UUID[]` (6) | `TEXT` holding a JSON array | `[]string` → JSON array (except in a list-membership position, where it expands) | `[]string` ← JSON array |
| `BOOLEAN` (28) | `INTEGER` 0/1 | `bool` | `bool` (database/sql converts 0/1) |
| `INTEGER`/`INT`/`BIGINT` | `INTEGER` | integers | integers |
| `VARCHAR(n)` (69 × 255, 43 × 50, 9 others) | `TEXT` + `CHECK (length(col) <= n)` | `string` | `string` |
| `TEXT` (35) | `TEXT` | `string` | `string` |
| `SERIAL` (1: `host_aggregates.id`) | `INTEGER PRIMARY KEY AUTOINCREMENT` (ids never reused, as with a sequence) | integers | integers |
| `NULL` | `NULL` | `nil`, nil pointers | pointers (unchanged rule) |

Column defaults in the SQLite baseline:

| PostgreSQL default (count) | SQLite baseline |
|---|---|
| `gen_random_uuid()` (59) | `(gen_random_uuid())` — registered function |
| `NOW()` / `CURRENT_TIMESTAMP` (113) | `(now())` — registered function, returns the format above |
| `true` / `false` (32) | `1` / `0` |
| JSONB `'{}'` / `'{}'::jsonb` (6) | `'{}'` |
| array `'{}'` / `ARRAY[]::TEXT[]` (2) | `'[]'` |

Constraints: primary, unique and foreign keys mirrored with their actions (73 `ON DELETE CASCADE`, 6 `ON DELETE SET NULL`); the 6 `CHECK` constraints mirrored; foreign keys enforced on every connection (FR-008). Indexes: all 151 distinct indexes the PostgreSQL schema has at 073 (161 `CREATE INDEX` statements, some repeated with `IF NOT EXISTS`) mirrored by name and columns, the 4 partial predicates included; the GIN index on `images.properties` (`migrations/051_glance_image_properties.up.sql:7`) has no SQLite equivalent and is the one documented exception (FR-028).

## 4. Database contract (Go)

The entities every component uses instead of pgx types (signatures in [contracts/go-database-contract.md](contracts/go-database-contract.md)):

| Entity | Role | PostgreSQL implementation | SQLite implementation |
|---|---|---|---|
| `DB` | the process's one database: query, query one row, execute, begin, ping, close, backend | wraps `*pgxpool.Pool` | wraps `*sql.DB` (modernc) |
| `Querier` | what `DB` and `Tx` share: query, query one row, execute | — | — |
| `Tx` | a transaction: `Querier` + commit, rollback | wraps `pgx.Tx` | wraps `*sql.Tx` |
| `Rows` | next, scan, err, close | wraps `pgx.Rows` | wraps `*sql.Rows`, converts on scan |
| `Row` | scan | wraps `pgx.Row` | wraps the row, converts on scan |
| `Result` | rows affected | `pgconn.CommandTag` | `sql.Result` |
| `ErrNoRows` | one "no rows" condition | from `pgx.ErrNoRows` | from `sql.ErrNoRows` |
| error classes | unique, foreign key, not null, check, busy/serialization | SQLSTATE 23505, 23503, 23502, 23514, 40001/40P01/55P03 | SQLite extended codes (research R7) |
| `Backend` | `postgres` or `sqlite`, chosen once from the URL scheme | | |

## 5. Migration view

One backend's reading of `migrations/` (FR-024):

| Rule | Effect |
|---|---|
| 1. A `.postgres` or `.sqlite` file belongs to that backend only | the PostgreSQL view never sees `NNN_baseline.sqlite.up.sql` |
| 2. A backend's own file for a version replaces the plain file of that version | a `.postgres`/`.sqlite` pair under one number never collides |
| 3. SQLite skips every version below its baseline | the 72 PostgreSQL files below 073 never reach SQLite; 073 itself is replaced under rule 2 |

The baseline version is the number of the file named `NNN_baseline.sqlite.up.sql`. File names are served unchanged, so `golang-migrate` parses them as before; down files follow the same rules; up/down pairing is validated per view.

## 6. Files on disk (SQLite)

| Path (recommended layout) | Mode | Created by |
|---|---|---|
| `<state_path>/db/` | 0750 when Lightstack creates it | the server or `lightstack-migrate` (FR-011) |
| `<state_path>/db/lightstack.db` | 0600, enforced at every open (FR-011a) | the server or `lightstack-migrate` (never `cleanup`/`console-tokens`) |
| `lightstack.db-wal`, `lightstack.db-shm` | 0600 (SQLite gives them the database file's mode; tightened if wider) | SQLite |
| `lightstack.db.migrate-lock` | 0600 | the migrator, held exclusively for a whole run (FR-030) |

## 7. Configuration inputs

| Input | Values | Effect |
|---|---|---|
| `database.url` / `LIGHTSTACK_DB_URL` | `postgres://…`, `postgresql://…`, `sqlite:///<absolute path>` | selects the backend (FR-001/002) |
| `LIGHTSTACK_DB_BACKEND` | unset, `postgres`, `sqlite` | when set, must agree with the URL (FR-004) |
| `LIGHTSTACK_NODE_NAME` | unset or a name | the node name of reservations (FR-033); host name when unset |
| `neutron.vxlan_enabled` | bool | `true` + SQLite → refused (FR-005) |
| `database.max_connections`, `min_connections`, `max_conn_lifetime`, `max_conn_idle_time` | as today | applied to both pools; `health_check_period` is PostgreSQL-only (research R4) |
