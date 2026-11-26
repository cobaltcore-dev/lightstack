# Contract: the migrations folder

The rules a developer, a maintainer and CI hold the `migrations/` folder to after the squash. The
names come from [data-model.md](../data-model.md) §1 and §7.

## Grammar

```text
migrations/
  001_baseline.postgres.up.sql      001_baseline.postgres.down.sql
  001_baseline.sqlite.up.sql        001_baseline.sqlite.down.sql
  NNN_v<semver>.up.sql              NNN_v<semver>.down.sql                  # NNN ≥ 002, all fragments were plain
  NNN_v<semver>.postgres.up.sql     NNN_v<semver>.postgres.down.sql         # NNN ≥ 002, backend-specific
  NNN_v<semver>.sqlite.up.sql       NNN_v<semver>.sqlite.down.sql
  next/<YYYYMMDDhhmmss>-<ticket>-<purpose>[.postgres|.sqlite].(up|down).sql
  README.md
```

`<semver>` is `MAJOR.MINOR.PATCH`, optionally followed by `-<pre-release>`, for example `v0.7.0` or
`v0.7.0-rc.1`.

## Validation

`ValidateMigrations` checks the rules below. `lightstack-migrate` runs it before every command, and a unit test
runs it over the real folder in CI. Each failure names the file.

| Rule | Error text (prefix) |
|---|---|
| Version `001` is exactly the four baseline files | `version 001 must be the baseline pair of each backend:` |
| Every other numbered file is a release migration | `<file>: a numbered migration other than the baseline or a release migration; schema changes go into fragments (migrations/next/)` |
| Release versions run 002, 003, … without gaps | `release migrations must be numbered consecutively from 002: <file>` |
| One release per version, and no release twice | `version <N> names two releases:` / `release <vX> has two versions:` |
| Every up file has its down file, per variant and per view | `<backend> view: <file> has no down migration` (existing wording) |
| A fragment name matches the grammar | `<file>: a fragment name must be <UTC YYYYMMDDhhmmss>-<ticket>-<purpose>[.postgres\|.sqlite].(up\|down).sql` |
| A fragment name is plain or backend-specific, not both | `fragment <name> exists both plain and as .<backend>` |
| Every fragment variant has up and down | `fragment <file> has no down part` / `… no up part` |

Removed: "exactly one SQLite baseline (`NNN_baseline.sqlite.up.sql`)" and every rule that concerned versions below a
backend's baseline.

## Assembly: the open release migration

Assembly runs when `migrations/next/` holds at least one fragment.

| Item | Value |
|---|---|
| Version | one above the newest numbered version |
| Name in each backend's view | `NNN_open.up.sql`, `NNN_open.down.sql` |
| Up BODY of a backend | each plain or own-backend fragment in name order: `-- fragment: <name>`, then its up SQL |
| Up file | header (`-- open release migration NNN (unreleased): assembled from migrations/next/`), then `-- checksum: <hex>`, then the BODY, then the record `INSERT` |
| Down file | header, then the record `DELETE`, then the fragments' down SQL in reverse name order |
| Both backends | get the version whenever any fragment exists, even when one backend's BODY is empty |

## Freeze: `lightstack-migrate freeze [-migrations <dir>] <release>`

| Input | Result | Exit |
|---|---|---|
| Fragments present, every one plain | writes `NNN_<release>.up.sql` and `.down.sql`, then deletes `next/*.sql` | 0 |
| Fragments present, some backend-specific | writes `NNN_<release>.postgres.*` and `NNN_<release>.sqlite.*`, then deletes `next/*.sql` | 0 |
| No fragments | writes nothing: `no fragments in migrations/next/: release <release> adds no migration` | 0 |
| `<release>` not `v<semver>` | `release name must be v<semver>, e.g. v0.7.0 or v0.7.0-rc.1` | 1 |
| A migration already names `<release>` (checked first, so a second freeze of one release is refused even without fragments) | `release <release> already has migration <file>`, its up file | 1 |
| The fragments fail validation | the validation error | 1 |

The frozen files carry the BODY and the checksum the open migration served. Only the header's first line becomes
`-- release migration NNN (<release>): frozen from migrations/next/ by lightstack-migrate freeze`.

`make migrations-freeze RELEASE=<release>` runs `go run ./cmd/lightstack-migrate freeze -migrations migrations
<release>`.

## Creating a fragment: `make migration-fragment TICKET=<n> NAME=<purpose>`

The target creates `migrations/next/<UTC now>-<n>-<purpose>.up.sql` and `.down.sql`, both empty, and prints their
paths. For a backend-specific change, rename them to `.postgres.*` and `.sqlite.*` pairs.

## Generated baselines

| Generator | Writes | Refuses when |
|---|---|---|
| `TestGeneratePostgresBaseline -update` | `001_baseline.postgres.{up,down}.sql` | a column type, default, constraint or object kind has no rendering; the generators' squash point ≠ newest version in `testdata/pre-squash/` |
| `TestGenerateSQLiteBaseline -update` | `001_baseline.sqlite.{up,down}.sql` | the same, plus a type with no SQLite mapping (as today) |

Both read a scratch PostgreSQL database migrated with `internal/database/testdata/pre-squash/`. Both keep
install-time ids as `gen_random_uuid()` and install-time timestamps as `now()`. Both append `applied_migrations` and
the record row for version 1.
