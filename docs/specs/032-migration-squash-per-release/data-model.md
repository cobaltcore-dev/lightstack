# Data Model: Squash the migrations, restart at 001, one migration per release

**Feature**: [spec.md](spec.md) · **Research**: [research.md](research.md)

The feature adds one table and changes how migration files are laid out and named. The schema
otherwise stays as it is, with one exception on SQLite (§6).

## 1. The migrations folder after the squash

| Path | What it is | Written by | Changes after it lands on `main`? |
|---|---|---|---|
| `migrations/001_baseline.postgres.up.sql`, `.down.sql` | PostgreSQL baseline: the squash point's schema and seed data, plus `applied_migrations` and its row | `TestGeneratePostgresBaseline -update` | Never (FR-025) |
| `migrations/001_baseline.sqlite.up.sql`, `.down.sql` | SQLite baseline: the same schema in SQLite form, plus `applied_migrations` and its row | `TestGenerateSQLiteBaseline -update` | Never |
| `migrations/NNN_v<release>.up.sql`, `.down.sql` | Release migration for releases whose fragments were all plain | `lightstack-migrate freeze` | Never |
| `migrations/NNN_v<release>.{postgres,sqlite}.{up,down}.sql` | Release migration for releases with backend-specific fragments | `lightstack-migrate freeze` | Never |
| `migrations/next/<fragment>.sql` | Fragments of the open release migration | developers, through `make migration-fragment` | Until the release freezes them |
| `migrations/README.md` | The rules | by hand | yes |
| `internal/database/testdata/pre-squash/` | The 184 pre-squash files, moved with `git mv` | the squash change | Never: test input only |

Both backends start at `001`, and no file sits below it. Each backend's view serves:
- the baseline;
- the release migrations, each backend's own file replacing the plain one of its number (rule 2);
- when `next/` holds any fragment, a synthetic `NNN_open.up.sql` and `NNN_open.down.sql`, where `NNN` is one
  above the newest numbered version.

## 2. `applied_migrations` (new)

PostgreSQL, created by `001_baseline.postgres.up.sql`:

```sql
CREATE TABLE applied_migrations (
    version    BIGINT      PRIMARY KEY,
    checksum   TEXT        NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

SQLite, created by `001_baseline.sqlite.up.sql`, in the generator's style:

```sql
CREATE TABLE "applied_migrations" (
    "version"    INTEGER NOT NULL PRIMARY KEY,
    "checksum"   TEXT    NOT NULL,
    "applied_at" TEXT    NOT NULL DEFAULT (now())
) STRICT;
```

| Column | Meaning | Rule |
|---|---|---|
| `version` | the migration version this row describes | one row per applied post-squash version |
| `checksum` | SHA-256 (hex) of the migration's body as applied (§4) | equals the `-- checksum:` header of that version's up file |
| `applied_at` | when it was applied | informational; never compared |

- Every post-squash up file ends with `INSERT INTO applied_migrations (version, checksum) VALUES (<N>, '<hex>');`.
- Every down file begins with `DELETE FROM applied_migrations WHERE version = <N>;`.
- The row commits in the migration's own transaction: an implicit transaction on PostgreSQL, the driver's
  `Begin`/`Commit` on SQLite (research R5).

The equivalence check (FR-004) looks neither at this table nor at golang-migrate's `schema_migrations`.

## 3. Database states

Read before golang-migrate runs, from `schema_migrations (version, dirty)` and `applied_migrations` (research R9):

| State | `schema_migrations` | `applied_migrations` | Server start, `up` | `status` |
|---|---|---|---|---|
| **fresh** | absent, or no row; on SQLite also: no file yet (inspection creates none) | absent | migrates from `001`, creating the SQLite file | "no version", exit 0 |
| **pre-squash** | version V, clean; or dirty with V ≠ 1 | absent | refused: pre-squash (FR-011) | reports it, exit 1 |
| **baseline in flight** | version 1, dirty | absent | deferred to golang-migrate's lock | as golang-migrate reports |
| **current** | version V clean, V ≤ newest in the view | row V matches the expected checksum | migrates up | baseline, release or open; exit 0 |
| **stale** | version V clean | row V differs, or no row V | refused: stale (FR-020) | reports it, exit 1 |
| **newer** | version V > newest in the view | — | refused: newer Lightstack (existing) | reports it, exit 1 |
| **dirty** | version V, dirty | present | refused by golang-migrate (existing) | reports it, exit 1 |

**Expected checksum** for version V: the `-- checksum:` header of the up file the view serves for V, whether that
is the baseline, a frozen release migration or the synthetic open migration.

Transitions:

```text
fresh ──up──▶ current(001) ──up──▶ current(newest)
pre-squash ──reset / recreate──▶ fresh
current(open N) ──a fragment added, changed or removed──▶ stale ──recreate──▶ fresh
current(open N) ──freeze with the same fragments──▶ current(release N)   (same checksum; FR-021)
current(open N) ──freeze after more fragments merged──▶ stale
current(V) ──down──▶ current(V-1)      (the row for V is deleted)
current(001) ──down──▶ fresh           (the baseline's down drops every table)
```

## 4. Migration body and checksum

Every post-squash up file has this shape:

```text
-- <header line: baseline | release migration NNN (vX.Y.Z) | open release migration NNN (unreleased)>
-- checksum: <sha256 hex of BODY>
<BODY>
INSERT INTO applied_migrations (version, checksum) VALUES (<N>, '<sha256 hex of BODY>');
```

| Migration | BODY |
|---|---|
| baseline | the generated DDL, seed rows and `applied_migrations` DDL |
| open or release migration, each backend | for each fragment in name order that the backend takes (plain plus its own): `-- fragment: <name>`, a newline, the fragment's up SQL, a newline |

The down file starts with the same header, then the `DELETE` of its row, then each fragment's down SQL in reverse
name order, each preceded by `-- fragment: <name>`.

**Freeze invariant**: `freeze` writes the BODY the view served as the open migration, byte for byte. Only the
header's first line changes. A database that applied the open migration therefore holds the same checksum as one
migrated from the release migration (FR-021, FR-023).

## 5. The squash point

The squash point is the newest pre-squash version on `main` when the squash lands: `83` on this branch. The
baselines reproduce its schema and seed data, and their header line names it. Production code keeps no constant for
it: no refusal names a version to aim for, and `status` calls the baseline `baseline`. The generators, which are test
code, hold it and check that it equals the newest version in `testdata/pre-squash/`.

## 6. Schema changes made by this feature

| Change | Backend | Why |
|---|---|---|
| `applied_migrations` added | both | the post-squash marker and the applied-state record (FR-012, FR-019) |
| `project_id` on volumes, ports, networks, routers and floating_ips becomes a real NOT NULL column, and the 10 guard triggers of `079`/`080` go | SQLite | spec FR-005 exception (Clarifications) |

Nothing else changes. The equivalence check proves it (FR-004).

## 7. Fragment names

```text
<UTC YYYYMMDDhhmmss>-<ticket>-<purpose>[.postgres|.sqlite].(up|down).sql
```

| Part | Rule |
|---|---|
| time | 14 digits, the fragment's creation time in UTC; this is what orders fragments |
| ticket | decimal number (the cloud-storage issue) |
| purpose | lower-case words separated by `-` (`[a-z0-9]+(-[a-z0-9]+)*`) |
| variant | absent for plain SQL both backends treat alike, `.postgres` or `.sqlite` otherwise |

- Every variant's up file needs a down file of the same variant, and the reverse.
- One name is either plain or backend-specific, never both.
- Name order is the order of application. It sorts by time first, then ticket, then purpose.
