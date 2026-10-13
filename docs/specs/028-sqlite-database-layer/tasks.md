# Tasks: SQLite backend, part 1 — database layer and migrations

**Input**: Design documents in `docs/specs/028-sqlite-database-layer/` — plan.md, spec.md, research.md (R1–R22), data-model.md, contracts/ (6), quickstart.md
**Prerequisites**: plan.md, spec.md (US1–US6)

**Tests**: Included and written first. The constitution makes test-first non-negotiable, and the spec requires the tests (FR-008, FR-017, FR-018, FR-028, FR-037 – FR-039, SC-001 – SC-011). Every test task names the behaviour that must be RED before its implementation task.

**Organization**: Grouped by user story. Story labels keep the spec's numbers. US2 (PostgreSQL unchanged) runs first after Phase 2 because it is the gate for the type conversion everything else builds on.

**Line references**: `file:line` citations in this document are against commit `278d106`; later commits have shifted them.

**Conventions for every task**:
- PostgreSQL leg: `LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable'` (`make db-up`).
- SQLite leg: `LIGHTSTACK_TEST_DB_URL='sqlite:///tmp/ls-test/lightstack-test.db'`.
- Always run `go test -count=1`: Go's test cache does not see the variable `TestMain` reads.
- No task commits. The branch owner commits.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: US1–US6 from spec.md

---

## Phase 1: Setup

**Purpose**: record the oracle, add the dependency.

- [X] T001 Record the pre-change gate baseline on PostgreSQL in `docs/specs/028-sqlite-database-layer/baseline-gates.md`. Record:
  - the passed/failed/skipped counts per package of `go test -count=1 ./...` (PostgreSQL leg);
  - `make lint`;
  - the contract suite (`docker compose -f deployments/docker-compose.yml up -d`, then `cd test/contract && go test -count=1 ./...`);
  - `./test/quick_test.sh`;
  - `make test-horizon-ui`.

  This is the oracle for SC-002.
- [X] T002 Add `modernc.org/sqlite` v1.60.1 to `go.mod`/`go.sum` (`go get modernc.org/sqlite@v1.60.1`), then `go mod tidy`. Confirm that `CGO_ENABLED=0 go build ./cmd/...` succeeds and that `golang-migrate` stays at v4.19.1 (research R22).

- [X] T003 Present the test strategy to the user before writing any test (constitution Test-First, step 2) — the RED tests of this file, the two test legs, the concurrency matrix and the contract tests — and record the approval in `docs/specs/028-sqlite-database-layer/spec.md` Clarifications.
---

## Phase 2: Foundational (blocking — the backend-neutral contract, PostgreSQL only)

**Purpose**: every component reaches the database through `internal/database` types, while PostgreSQL behaviour stays identical. The work is type-only: identifiers keep their names (`pool`, `svc.pool`, `testPool`) — research R1, [contracts/go-database-contract.md](contracts/go-database-contract.md).

**⚠️ CRITICAL**: No user-story work starts before T022 matches the T001 baseline.

- [X] T004 Write `internal/database/postgres_test.go` (PostgreSQL leg; skips without a URL). Cover:
  - `QueryRow` on no rows → `errors.Is(err, database.ErrNoRows)`;
  - `Exec(...).RowsAffected()`;
  - `Begin`/`Commit`/`Rollback`;
  - `WithTx` rolls back on error;
  - `IsUniqueViolation`, `IsForeignKeyViolation`, `IsNotNullViolation`, `IsCheckViolation` on real violations;
  - `HealthCheck`.

  RED: the package does not compile yet.
- [X] T005 Move `internal/common/pgerror_test.go`'s fixture cases (`pgconn.PgError` 23505 / 23503 / 22P02, wrapped errors) to `internal/database/errors_test.go`, targeting `database.IsUniqueViolation`. RED.
- [X] T006 Define the contract in `internal/database/db.go`:
  - types `Backend` (`Postgres`, `SQLite`), `Querier`, `DB`, `Tx`, `Rows`, `Row`, `Result`, `ErrNoRows`, `Options{Pool PoolConfig; MustExist bool}`;
  - `ParseURL(raw) (Backend, error)`, with scheme selection only: `postgres` / `postgresql` / `sqlite`. Empty, no scheme or another scheme → `database.url … accepted schemes: postgres, postgresql, sqlite`. The full validation comes in T055;
  - `Open(ctx, url, opts) (DB, error)`, dispatching by backend; the sqlite branch returns "not built yet" until T042;
  - `HealthCheck(ctx, db)` → `db.Ping`.

  Keep `PoolConfig` and `DefaultPoolConfig`. Delete `Connect`: no alias.
- [X] T007 Implement `internal/database/postgres.go`. It wraps `*pgxpool.Pool` using today's `ParseConfig` + `NewWithConfig` path (moved from `db.go:33-63`):
  - `pgDB`, `pgTx`, `pgRows`, `pgRow` (maps `pgx.ErrNoRows` → `database.ErrNoRows`), `pgResult`;
  - `Backend()` returns `Postgres`.

  Statements pass through untouched (R2).
- [X] T008 Implement `internal/database/errors.go` for PostgreSQL:
  - `IsUniqueViolation` (23505), `IsForeignKeyViolation` (23503), `IsNotNullViolation` (23502), `IsCheckViolation` (23514), `IsBusy` (40001 / 40P01 / 55P03);
  - all through `errors.As(*pgconn.PgError)`.

  Delete `internal/common/pgerror.go` and its test.
- [X] T009 Change `internal/database/tx.go` to `WithTx(ctx, db DB, fn func(tx Tx) error) error`; same commit/rollback semantics (`tx.go:13`).
- [X] T010 Create `internal/database/dbtest/dbtest.go`, the shared `TestMain` helper:
  - `Open(migrationsPath string) (database.DB, bool)` reads `LIGHTSTACK_TEST_DB_URL`. Unset → `false`, so callers skip as today. A `postgres://` URL → `database.Open` + `database.MigrateUp`;
  - `RequirePostgres(t)` skips a test unless the backend is PostgreSQL.
- [X] T011 [P] Convert `internal/keystone`:
  - every `*pgxpool.Pool` → `database.DB`, `pgx.Tx` → `database.Tx`, `pgx.ErrNoRows` → `database.ErrNoRows`;
  - `common.IsUniqueViolation` → `database.IsUniqueViolation` (`groups.go:103`, `handlers.go:610`);
  - `maindb_test.go` → `dbtest.Open`;
  - `pool_injection_test.go` → `database.Open` (`domain_resolve_test.go` reads the package `testPool`).
- [X] T012 [P] Convert `internal/nova`: types, `pgx.ErrNoRows`, `flavors.go:60` (`IsUniqueViolation`), `DeleteServer`'s explicit `svc.pool.Begin` (`handlers.go:2044`); `maindb_test.go` → `dbtest.Open`.
- [X] T013 [P] Convert `internal/neutron`: types, `pgx.ErrNoRows`, and `router.go:819` `pgx.CollectRows(rows, pgx.RowTo[string])` → a scan loop; `network_db_test.go` → `dbtest.Open`.
- [X] T014 [P] Convert `internal/cinder`:
  - types, including the 23 `pgx.Tx` references in `quotas.go`, `volumes.go`, `transfers.go`, `manage.go` and `backups.go`;
  - `pgx.ErrNoRows`;
  - the inline SQLSTATE check `volumes.go:945-946` → `database.IsUniqueViolation`;
  - `volumes_db_test.go` → `dbtest.Open`.
- [X] T015 [P] Convert `internal/glance` (types, `pgx.ErrNoRows`; `images_db_test.go` → `dbtest.Open`).
- [X] T016 [P] Convert `internal/metadata` (types, `pgx.ErrNoRows`; `service_db_test.go` → `dbtest.Open`). Replace the `testPool.Stat().AcquiredConns()` assertion (`service_db_test.go:50`) with an assertion on the behaviour it guards.
- [X] T017 [P] Convert `internal/console` (`store.go` and the CLI; `maindb_test.go` → `dbtest.Open`), `internal/compute` (`node_registry.go`) and `internal/health` (`DatabaseCheck(db database.DB)`).
- [X] T018 [P] Convert `internal/common`: `project.go`, quota helpers, and any helper taking `*pgxpool.Pool`; `maindb_test.go` → `dbtest.Open`.
- [X] T019 Convert the entry points in `cmd/lightstack/main.go`, `internal/teardown/teardown.go` and `test/benchmark/database_bench_test.go`:
  - `cmd/lightstack/main.go` — server `:91` and console-tokens `:555` → `database.Open(ctx, url, database.Options{Pool: …})`;
  - `internal/teardown/teardown.go:85`;
  - `test/benchmark/database_bench_test.go`.
- [X] T020 Prove that no pgx type leaks outside `internal/database/`:
  - `grep -rnE 'jackc/pgx' --include='*.go' . | grep -v '^./internal/database/'` is empty;
  - `go build ./... && go vet ./...` pass;
  - `make lint` reports 0 issues.
- [X] T021 Run `internal/database/postgres_test.go` and `internal/database/errors_test.go` (T004/T005): GREEN on the PostgreSQL leg.
- [X] T022 Re-run every T001 gate on PostgreSQL. Results must equal `baseline-gates.md`. Any difference is fixed before Phase 3.

**Checkpoint**: one contract, PostgreSQL only, behaviour identical.

---

## Phase 3: User Story 2 — PostgreSQL deployments see no change (Priority: P1)

**Goal**: prove that the PostgreSQL path is untouched: URL forms, pool settings, row locks, migrations, API (FR-003, FR-025).

**Independent test**: the T001 gates match after every phase; the existing migration files are unchanged except the corrected down files of 010/019/028/037 (FR-025); the pool settings and row locks are verified.

- [X] T023 [P] [US2] In `internal/database/postgres_internal_test.go` (package-internal, to read pgxpool's own config), assert that `Options.Pool` reaches pgxpool unchanged (`MaxConns`, `MinConns`, `MaxConnLifetime`, `MaxConnIdleTime`, `HealthCheckPeriod`), and that `postgres://` and `postgresql://` URLs open the same way.
- [X] T024 [P] [US2] In `internal/database/postgres_test.go`, assert that statements reach PostgreSQL verbatim: a `FOR UPDATE` in one transaction blocks a second until the first commits, and a `::uuid[]` / `= ANY($1)` statement runs untranslated.
- [X] T025 [US2] Add a CI step to the `unit-tests` job in `.github/workflows/ci.yml` (`:56-99`) that fails when `git diff --diff-filter=MDR --name-only "origin/${{ github.base_ref || 'main' }}" -- migrations/` is non-empty — the pull request's base, `main` on pushes (SC-002).
  - Removed again 2026-10-13 at the user's request: a permanent gate would block a future squash of the migrations. SC-002 stays this feature's acceptance check (`git diff --diff-filter=MDR --name-only main -- migrations/` prints only the four corrected down files of FR-025).

**Checkpoint**: US2 holds; re-run T022 at the end of every later phase.

---

## Phase 4: User Story 1 — Run Lightstack on one SQLite file (Priority: P1) 🎯 MVP

**Goal**: `LIGHTSTACK_DB_URL=sqlite:///<path>` starts, migrates and serves every service, and the contract suite and `quick_test.sh` pass (FR-006 – FR-023, FR-037, FR-038).

**Independent test**: quickstart §1, §3 (SQLite leg) and §6.

### Tests for User Story 1 (write first, RED)

- [X] T026 [P] [US1] Write `internal/database/sqlite_translate_test.go`, table tests for research R5 / [contracts/sql-dialect.md](contracts/sql-dialect.md) §2:
  - casts: `::text` / `::varchar` → `CAST(… AS TEXT)`; `::int` / `::bigint` → `CAST(… AS INTEGER)`; `::jsonb`, `::uuid`, `::uuid[]`, `::text[]`, `::timestamp` dropped;
  - operand shapes: placeholder, literal, `a.b`, `f(…)`, `(…)`;
  - `= ANY($n)` and `= ANY($n::uuid[])` expansion with renumbering of later placeholders; an empty slice; a non-slice argument → error;
  - `$1` used twice;
  - `FOR UPDATE` removed;
  - `now() + make_interval(secs => $6)` → `ls_add_seconds(now(), $6)`;
  - `x ~ $n` → `x REGEXP $n`; `~*` and `!~` stay errors;
  - quoted strings, quoted identifiers and comments untouched; dollar quoting → error; an unknown cast type → error;
  - the cache reused across list lengths.
- [X] T027 [P] [US1] Write `internal/database/sqlite_values_test.go` (R7):
  - bind: `time.Time`, `*time.Time` (nil and set), `[]byte`, `json.RawMessage`, map, struct, `[]string` outside a list position, `bool`, `uuid.UUID`;
  - scan: into `time.Time`, `*time.Time`, `sql.NullTime`, map, struct, `[]string`, `bool`, `[]byte`, and `string` from an INTEGER `->>` result;
  - a malformed timestamp text → error.
- [X] T028 [P] [US1] Write `internal/database/sqlite_test.go` (R3, R4, R6, R7):
  - files: a missing directory created 0750 and a file 0600; an existing 0644 database, `-wal` and `-shm` tightened to 0600 with one log line each; `MustExist` on a missing file → error naming the path, nothing created;
  - settings read back from a pooled connection: `journal_mode=wal`, `synchronous=2`, `foreign_keys=1`, the busy wait;
  - registered functions: `now()` format, `gen_random_uuid()`, `ls_add_seconds`, `regexp` (through `x REGEXP y`), `pg_advisory_xact_lock(hashtext(…))` runs;
  - a read completes while another connection holds an open write transaction (WAL; FR-009);
  - error classes: unique, including PRIMARY KEY 1555; FK 787; NOT NULL 1299; CHECK 275; busy 5/517/6;
  - `ErrNoRows`, `RowsAffected`, commit and rollback.
- [X] T029 [P] [US1] Write `internal/neutron/router_snat_db_test.go`, running on both legs. The SNAT predicate (`router.go:730-731`) selects a router with a gateway and `enable_snat` missing or `true`, and excludes one with `enable_snat: false` (FR-017).
- [X] T030 [P] [US1] Write `internal/database/roundtrip_db_test.go` on both legs: a scratch table written and read back through the contract with timestamps (µs), a JSON document, a list, a bool, a NULL and an id. Values are equal on both backends (US1 AS6).
- [X] T031 [P] [US1] Write `internal/database/migrate_view_test.go` (R10, three rules, synthetic directories):
  - a plain file is used by both backends;
  - a `.postgres`/`.sqlite` pair replaces the plain file per backend;
  - SQLite hides versions below `NNN_baseline.sqlite.up.sql`;
  - down files follow the same rules;
  - `README.md` is ignored;
  - a collision after the rules → `ErrDuplicateMigration`.
- [X] T032 [P] [US1] Write `internal/database/describe_db_test.go` on both legs: `Describe` returns the backend, the location, the schema version and — on SQLite — the settings read back; the location never contains a user name or password (`postgres://u:p@db:5432/x` → `db:5432/x`) (FR-013a). RED.
- [X] T033 [P] [US1] Write `pkg/paths/paths_test.go` cases for `DBDir()` = `<state_path>/db`.

### Rewrites, the same for both backends (R8; each with its own test on both legs)

- [X] T034 [P] [US1] `internal/nova/tenant_usage.go:31,116`: drop `EXTRACT(EPOCH …)`; select each instance's `created_at` and sum hours in Go against one `time.Now()`. Test the same totals in `internal/nova/tenant_usage_db_test.go`.
- [X] T035 [P] [US1] `internal/neutron/vxlan_coordinator.go:210-212`: `generate_series` → `WITH RECURSIVE r(vni) AS (SELECT $1 UNION ALL SELECT vni+1 FROM r WHERE vni < $2) SELECT vni FROM r WHERE vni NOT IN (SELECT vni FROM network_vni_allocations) LIMIT 1`. Test the first free VNI with gaps.
- [X] T036 [P] [US1] Rewrite three JSON-containment and aggregation sites in `internal/metadata/service.go`, `internal/neutron/network.go` and `internal/nova/aggregates.go`:
  - `internal/metadata/service.go:116`: candidates `WHERE device_id IS NOT NULL AND fixed_ips::text LIKE '%' || $1 || '%'`, with the address quoted as `$1`, then an exact `ip_address` match in Go (R19). Test that `10.0.0.5` does not match a port holding `10.0.0.50`;
  - `internal/neutron/network.go:1901` (`routerPortHolding`): candidates from the `router_interfaces` join, exact match in Go;
  - `internal/nova/aggregates.go:463`: read (`FOR UPDATE`), merge in Go, write, in one `database.WithTx`. Test that two concurrent updates keep both keys on PostgreSQL.
- [X] T037 [P] [US1] `internal/neutron/reconciler.go:318`: `DISTINCT ON (network_id)` → `ROW_NUMBER() OVER (PARTITION BY network_id ORDER BY created_at, id) = 1`. Test the first subnet per network.
- [X] T038 [P] [US1] Three rewrites in `internal/nova/server_filter.go`, the `CURRENT_TIMESTAMP` sites and `internal/nova/handlers.go`:
  - `internal/nova/server_filter.go:244-246`: resolve `ip` in Go, as upstream does in Python (`nova/compute/api.py:3244`). Fetch the scope's ports — paired by project with their server; all projects for an admin with `all_tenants` — match each address with `regexp` (already validated at `:221`) and add `id = ANY($n::uuid[])`. `name` stays `name ~ $n` (`:209`): unchanged on PostgreSQL, as upstream's database filter there (`nova/db/main/api.py:1990-2003`), and translated to `REGEXP` on SQLite (T040). Tests:
    - `ip`: the same results on both legs; no match → empty list; an admin cross-project listing;
    - `name`: ordinary patterns give the same results on both legs; an escape the dialects read differently (`\b`) follows each backend's database, as upstream's does;
  - the 7 `CURRENT_TIMESTAMP` → `NOW()` (`nova/handlers.go:3295`, `keystone/handlers.go:1230,1370`, `cinder/volumes.go:2231,2233,2445,2447`);
  - `internal/nova/handlers.go:1714,1809` `ARRAY_AGG … FILTER` → one row per port × floating address, collected per port in Go. Test that a server without floating addresses gets an empty list and one with two gets both.

### Implementation for User Story 1

- [X] T039 [US1] Implement `internal/database/sqlite_funcs.go`, registering at package init through `sqlite.RegisterScalarFunction` / `RegisterDeterministicScalarFunction`:
  - `now()` (UTC, `2006-01-02T15:04:05.000000Z`);
  - `gen_random_uuid()`;
  - `ls_add_seconds(ts, secs)`;
  - `regexp(pattern, value)` → Go's `regexp.MatchString`, with a compiled-pattern cache, so `x REGEXP y` works;
  - `pg_advisory_xact_lock(x)` → NULL;
  - `hashtext(x)` → 0.

  (R6)
- [X] T040 [US1] Implement `internal/database/sqlite_translate.go` (R5): lexer, cast rule with operand detection, `= ANY` expansion and renumbering, `FOR UPDATE` removal, the `make_interval` rewrite, `~` → `REGEXP`, and a bounded template cache (4096 entries, cleared when full). Errors name the construct. T026 → GREEN.
- [X] T041 [US1] Implement `internal/database/sqlite_values.go` (R7): bind conversion (times → canonical text, `[]byte`/`RawMessage` → text, maps/structs/non-list slices → JSON, `driver.Valuer`) and scan-destination wrapping (times, JSON targets). T027 → GREEN.
- [X] T042 [US1] Implement `internal/database/sqlite.go` (R3, R4):
  - open sequence: the `db` directory 0750 with missing parents 0755, each set by an explicit `Chmod` (umask-independent, FR-011); `O_CREATE|O_EXCL` 0600 for create entry points; `stat` for `MustExist`; tighten modes with logs;
  - DSN `file:<escaped path>?mode=rw&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate` — `_txlock` is confirmed by T081;
  - `database/sql` pool from `Options.Pool`;
  - settings read-back, failing on mismatch;
  - wrappers `sqliteDB`, `sqliteTx`, `sqliteRows`, `sqliteRow`, `sqliteResult` that run the translator and value conversion; `Backend()` returns `SQLite`.

  Wire the sqlite branch of `database.Open`. T028 → GREEN.
- [X] T043 [US1] Extend `internal/database/errors.go` with the SQLite extended codes: UNIQUE 2067 + PRIMARYKEY 1555, FOREIGNKEY 787, NOTNULL 1299, CHECK 275, and BUSY/BUSY_SNAPSHOT/LOCKED by primary code 5/6. Map `sql.ErrNoRows` → `ErrNoRows`.
- [X] T044 [US1] Add `DBDir()` to `pkg/paths/paths.go`. T033 → GREEN. No purge change: that belongs to part 2.
- [X] T045 [US1] Implement `internal/database/migrate_view.go`, an `fs.ReadDirFS` view applying the three rules and serving kept files under their own names. T031 → GREEN.
- [X] T046 [US1] Make `internal/database/migrate.go` backend-aware (R10):
  - read the view through `source/iofs` (`iofs.New(view, ".")`);
  - PostgreSQL via `migrate.NewWithSourceInstance("iofs", src, dbURL)`;
  - SQLite via a dedicated `*sql.DB` from the T042 opener (create-if-missing), then `sqlite.WithInstance` + `migrate.NewWithInstance`;
  - `MigrateUp`, `MigrateDown`, `MigrateToVersion`, `MigrateReset`, `ForceMigrationVersion` and `GetMigrationStatus` keep their signatures.
- [X] T047 [US1] Write the SQLite baseline generator `internal/database/baseline_gen_test.go` (`TestGenerateSQLiteBaseline`, active only with `-update` and a PostgreSQL URL). It migrates a scratch PostgreSQL database to 073, introspects `information_schema`/`pg_catalog`, and writes `migrations/073_baseline.sqlite.up.sql` and `.down.sql`. Mappings (data-model §3, R11, R18):
  - `STRICT` tables and TEXT ids;
  - `VARCHAR(n)` → `TEXT CHECK (length(col) <= n)`;
  - `SERIAL` → `INTEGER PRIMARY KEY AUTOINCREMENT`;
  - defaults `(gen_random_uuid())` / `(now())` / `0|1` / `'{}'` / `'[]'`;
  - the 6 `CHECK`s;
  - FKs with `ON DELETE` actions;
  - 151 indexes with the 3 partial predicates; the GIN index on `images.properties` skipped;
  - seed rows as `INSERT`s.
- [X] T048 [US1] Generate and commit-ready `migrations/073_baseline.sqlite.up.sql` / `.down.sql` with T047, then review the output against data-model §3. A fresh SQLite file migrates to 073 and serves.
- [X] T049 [US1] Extend `internal/database/dbtest/dbtest.go` for `sqlite:///<path>` URLs: a fresh per-package file (the package name appended to the path, removed before migrating), so parallel `go test` processes never share a file. It also takes an option that forces `_txlock=deferred` or `immediate`, for T080. (Done as `dbtest.OpenWithTxLock` — a second handle per mode on the package's file — with `RunConcurrencyMatrix`, since one test compares both modes.)
- [X] T050 [P] [US1] Make the PostgreSQL-only test fixtures in `internal/console/`, `internal/nova/` and `internal/neutron/` backend-neutral:
  - `interval` literals → Go-computed times (`internal/console/helpers_test.go:59`, `internal/console/store_test.go:324`, `internal/console/cli_test.go:154`, `internal/nova/console_test.go:807`);
  - `generate_series` → Go loops (`internal/neutron/listing_page_size_test.go:38-50`);
  - `ARRAY['10.0.0.0/8']` → a bound `[]string` (`internal/neutron/update_statement_test.go:229`).
- [X] T051 [P] [US1] Guard the PostgreSQL-internals tests with `dbtest.RequirePostgres`: `internal/keystone/groups_domain_migration_test.go` (catalog queries) and the `DO $$` repair block in `internal/cinder/volumes_db_test.go:45-50` (run only on PostgreSQL).
- [X] T052 [US1] Wire the entry points in `cmd/lightstack/main.go` and `internal/teardown/teardown.go`:
  - `cmd/lightstack/main.go`: server create-if-missing; console-tokens `MustExist`, so a missing file exits 1 naming the path;
  - `internal/teardown/teardown.go:85`: `MustExist`, where a missing file takes the existing "database unreachable" path (`:84-92`) — warn naming the path, purge the host, exit 0 (FR-012).
- [X] T053 [US1] Add `database.Describe` (backend, redacted location, schema version, SQLite settings read back) and the `database ready` startup log line after `MigrateUp` in `cmd/lightstack/main.go` (FR-013a, [contracts/configuration-and-startup.md](contracts/configuration-and-startup.md) §6).
- [X] T054 [US1] Run all 8 database-backed packages (`internal/{cinder,common,console,glance,keystone,metadata,neutron,nova}`) on the SQLite leg and fix every failure. Then T026–T038 are GREEN on both legs (SC-004 for the unit tests).
  - Found beyond T034–T038 and fixed: `LIMIT $n`/`OFFSET $n` bound to NULL (9 list handlers) → a translator rule, contracts/sql-dialect.md §2; a nil `[]byte`/`json.RawMessage` bound as `''` instead of NULL; `DELETE FROM instance_actions a` → `AS a` (SQLite requires `AS` in `UPDATE`/`DELETE`); keystone's scope and S3-token lookups leaned on PostgreSQL's uuid parsing for dash-free ids → canonicalised in Go (spec Edge Cases, Values).
  - Test fixtures: `LOCK TABLE` failure injection (6 sites) → `dbtest.Unavailable` (PostgreSQL: the lock; SQLite: the table renamed away); `pg_stat_activity` wait in the transfer race test → a pause on SQLite; timestamp literals `'2026-01-01 00:00:00'` and `'{}'` array literals → bound Go values; a malformed-UUID failure injection → `dbtest.Unavailable`.
- [X] T055 [US1] Add a `Seed(t, sql, args...)` helper to `test/contract/helpers.go`: it runs one statement on `LIGHTSTACK_DB_URL`, opened through `database.Open(…, MustExist)`, and fails the test when it fails. Use it to replace the `docker exec lightstack-postgres psql` seedings in `test/contract/cinder/manage_test.go:155`, `test/contract/neutron/trunks_test.go:38,115,171` and `test/contract/neutron/l3_agents_test.go:153,163`. Set `LIGHTSTACK_DB_URL` to the Compose PostgreSQL in the existing `contract-tests` job (`.github/workflows/ci.yml:108`).
- [X] T056 [US1] Run the contract suite and `./test/quick_test.sh` against a stub-mode binary on SQLite (quickstart §1, §6) and fix every failure. Every test that passes on PostgreSQL passes, with the same skips (SC-001).
- [X] T057 [US1] CI (`.github/workflows/ci.yml`):
  - add a SQLite step to the `unit-tests` job with `LIGHTSTACK_TEST_DB_URL=sqlite:///${{ runner.temp }}/ls-test/t.db` and `-count=1`;
  - add a `contract-tests-sqlite` job — build; write a stub config with `database.url: sqlite:///${{ runner.temp }}/ls/db/lightstack.db`; start `bin/lightstack`; wait for `lightstack healthcheck`; run the contract suite and `test/quick_test.sh` (install the OpenStack CLI as the `integration-tests` job does at `:215-246`);
  - add the new job to `status-check`'s `needs` (`:389`).

**Checkpoint**: US1 is the MVP — Lightstack runs on SQLite, and PostgreSQL is still unchanged (re-run T022).

---

## Phase 5: User Story 3 — Misconfiguration is refused at startup (Priority: P2)

**Goal**: bad URLs, backend mismatches and SQLite with multi-node are refused within 1 s, naming the problem, before anything touches the disk (FR-001, FR-002, FR-004, FR-005, SC-003).

**Independent test**: quickstart §2.

- [X] T058 [P] [US3] Write `internal/database/url_test.go` covering the input matrices and exact messages of [contracts/configuration-and-startup.md](contracts/configuration-and-startup.md) §1–§2:
  - `ParseURL`: empty, keyword/value, `mysql://`, `sqlite3://`, `file:`, `sqlite://relative.db`, `sqlite://host/x.db`, `sqlite:///x.db?mode=ro`, `sqlite:x.db`, valid PostgreSQL forms, `sqlite:///abs/x.db`;
  - `RedactURL` never prints the password;
  - `CheckDeclaredBackend`: unset, `postgres`, `sqlite`, mismatches, invalid values.

  RED.
- [X] T059 [P] [US3] Write `cmd/lightstack/startup_refusal_test.go`. It builds the binary once in `TestMain`, then runs each refused configuration with a fresh temp `state_path`. Assert:
  - the exit code: server 1, `cleanup` 2, `console-tokens` 2;
  - stderr contains the contract message;
  - the elapsed time is under 1 s;
  - no file or directory appears under the temp path.

  Include `neutron.vxlan_enabled: true` + SQLite. Add the same for `cmd/lightstack-migrate` (exit 1). Also the missing-file cases (FR-012): `cleanup` with a SQLite URL whose file does not exist warns naming the path, exits 0 and creates no file; `console-tokens` exits 1 naming the path. RED.
- [X] T060 [US3] Implement the full validation in `internal/database/url.go`: `ParseURL` (SQLite absolute path, no host, query or fragment), `RedactURL`, `CheckDeclaredBackend`, with the messages as in the contract. `database.Open` re-validates. T058 → GREEN.
- [X] T061 [US3] Validate in every entry point (`cmd/lightstack/main.go`, `cmd/lightstack-migrate/main.go`) right after loading the configuration, before opening:
  - server in `cmd/lightstack/main.go`: `ParseURL` + `CheckDeclaredBackend(os.Getenv("LIGHTSTACK_DB_BACKEND"))` + the multi-node guard → `log.Fatalf`;
  - `runCleanup` (`:566-592`) and `runConsoleTokens` (`:514-566`) → exit 2;
  - `cmd/lightstack-migrate/main.go` → exit 1.

  T059 → GREEN.

**Checkpoint**: refusals verified; US1 and US2 unaffected.

---

## Phase 6: User Story 4 — No transaction holds the database while storage works; quotas hold under concurrency (Priority: P2)

**Goal**: FR-032 – FR-036, FR-035a, FR-036a, FR-039 (research R13, R14); [contracts/block-storage-reservation.md](contracts/block-storage-reservation.md) and [contracts/compute-quota.md](contracts/compute-quota.md).

**Independent test**: quickstart §5 and §8. The PostgreSQL parts need only Phase 2. The SQLite runs and the `_txlock` decision need US1.

### Tests for User Story 4 (write first, RED)

- [X] T062 [P] [US4] Rewrite `TestBootVolumeQuotaRefusalIs413` (`internal/nova/boot_volume_test.go:338-348`) to expect `403` with `Quota exceeded for gigabytes: Requested …, but already used … of … gigabytes`. RED.
- [X] T063 [P] [US4] Write `internal/nova/quota_refusal_db_test.go`: a server create over the instances, cores or RAM limit answers `403`, with body `{"forbidden": {"code": 403, "message": "Quota exceeded for instances: Requested 1, but already used N of N instances"}}` (cores/ram analogous). RED: 413 today.
- [X] T064 [P] [US4] Write `test/contract/nova/quota_refusal_test.go` (gophercloud; runs in both contract jobs): set the project's instances quota to its current usage; `servers.Create` fails with `gophercloud.ErrUnexpectedResponseCode`, status `403`, message `Quota exceeded for instances: Requested 1, but already used N of N instances`; restore the quota in cleanup. RED: `413` today (constitution Test-First: contract test first).
- [X] T065 [P] [US4] Add the race test to `internal/nova/quota_refusal_db_test.go` (`TestServerCreateQuotaHoldsUnderConcurrency`): two concurrent server creates with one instance left end as exactly one `202` and one `403`, in 20 of 20 runs. Run it once for a project without a `quotas` row and once with one, on both legs (SC-005). RED on PostgreSQL today: both get `202`.
- [X] T066 [P] [US4] Extend `internal/nova/boot_volume_test.go`:
  - a project already at its instance limit is refused before any volume is provisioned (US4 AS8);
  - a create refused by the recheck removes its provisioned boot volume (AS7).
- [X] T067 [P] [US4] Write `internal/cinder/reservation_db_test.go`, using a fake storage client:
  - during the backing call the row is `creating` with `reserved_by` set;
  - concurrent creates count reservations, so none exceeds the limit;
  - a backing failure removes the row and the partial backing, with today's error;
  - success → `available` with `bootable`/`source_image_id`;
  - a row deleted mid-way → backing reclaimed and the request fails;
  - snapshot create: `available` before the response, which is still `202` with `creating`.
- [X] T068 [P] [US4] Add `TestNoTransactionHeldDuringTheCopy` to `internal/cinder/reservation_db_test.go` (SC-006). The fake storage blocks 10 s in `CreateVolumeFromImage`; meanwhile another project's write completes in under 1 s. RED on the SQLite leg.
- [X] T069 [P] [US4] Write `internal/cinder/creating_refusals_db_test.go`: delete and `os-extend` of a `creating` volume, and delete of a `creating` snapshot, answer `400` with upstream's messages (contract table). Attach still answers today's `400`.
- [X] T070 [P] [US4] Write `internal/cinder/sweep_db_test.go`:
  - own `creating` rows → `error` with the backing reclaimed;
  - rows of another node name untouched (SC-010);
  - rows with NULL `reserved_by` untouched;
  - a reclaim failure is logged and the sweep continues.
- [X] T071 [P] [US4] Write `internal/common/nodename_test.go`: `LIGHTSTACK_NODE_NAME` wins; otherwise `os.Hostname()`.

### Implementation for User Story 4

- [X] T072 [US4] Add `migrations/074_reservation_node.up.sql` / `.down.sql`, plain files for both backends (data-model §1): `ALTER TABLE volumes ADD COLUMN reserved_by TEXT; ALTER TABLE snapshots ADD COLUMN reserved_by TEXT;` and the reverse `DROP COLUMN`.
- [X] T073 [US4] Implement `internal/common/nodename.go`: `NodeName() (string, error)` returns `LIGHTSTACK_NODE_NAME` when non-empty, else `os.Hostname()`. T071 → GREEN.
- [X] T074 [US4] Restructure `provisionVolume` / `writeVolume` (`internal/cinder/volumes.go:484-569`) into reserve → backing outside any transaction → finalise or clean up (R13). `cinder.NewService` gains `nodeName` (`volumes.go:41`; caller `cmd/lightstack/main.go:237`).
- [X] T075 [US4] Restructure `CreateSnapshot` (`internal/cinder/volumes.go:1542-1650`) the same way, with the Ceph snapshot outside the transaction. Finalise synchronously and delete the deferred status goroutine (`:1620-1635`). The response is unchanged. T067 → GREEN.
- [X] T076 [US4] Refuse requests against `creating` resources with upstream's messages through `common.NewBadRequestError`: `DeleteVolume` (`volumes.go:1055`), `os-extend` in `VolumeAction` (`:1249`), `DeleteSnapshot` (`:1808`). T069 → GREEN.
- [X] T077 [US4] Implement `SweepInterruptedReservations(ctx)` in `internal/cinder/sweep.go` (R13). Call it from `cmd/lightstack/main.go` before the listeners open, and add `node` to the `database ready` log line. T070 → GREEN.
- [X] T078 [US4] Add the binding recheck to the instance-insert transaction (`internal/nova/handlers.go:1037`):
  - a helper `checkQuotaLocked(ctx, tx, projectID, wants)` in `internal/nova/quotas.go` takes `SELECT pg_advisory_xact_lock(hashtext($1))` with `'nova_quota:'+projectID`, then checks instances, cores and RAM through `usageQuery` (`:402-433`) and the limit lookup;
  - a refusal rolls back, and the existing path removes the boot volume (`:1060-1061`).

  The early checks (`:778-800`) stay.
- [X] T079 [US4] Answer quota refusals with `403`: replace `common.NewQuotaExceededError` at `internal/nova/handlers.go:780,791,802,987` with `common.NewForbiddenError` carrying `Quota exceeded for <res>: Requested <r>, but already used <u> of <l> <res>`. The numbers come from `QuotaExceededError` (`internal/nova/quotas.go`) and from the Cinder error for `:987`. Cinder's own `413` stays (`internal/cinder/quotas.go:572`). T062–T066 → GREEN on PostgreSQL.
- [X] T080 [US4] Write the SQLite concurrency tests (FR-039) in `internal/cinder/concurrency_db_test.go`, `internal/nova/concurrency_db_test.go` and `internal/neutron/concurrency_db_test.go`. Each case runs with `_txlock=deferred` and with `immediate` (a `dbtest` option): quota helper, attach, detach, port allocation, default security group, server create/delete, volume/snapshot/server metadata replacement, transfer create/delete. Print a mode × case result table.
- [X] T081 [US4] Decide `_txlock` and the busy wait from T080: `immediate` unless `deferred` passes every case; the busy wait stays at most 5 s (constitution v1.3.0, Article III). Set the default in `internal/database/sqlite.go` and record the table and the decision in research.md R4. Then T065, T067 and T068 are GREEN on the SQLite leg.
- [X] T082 [US4] Audit every transaction body — the 13 inventory sites plus T074/T075/T078 — for libvirt, storage, network or HTTP calls with the R13 grep method. Record the result in `docs/specs/028-sqlite-database-layer/research.md` R13 (SC-008).

**Checkpoint**: reservations, sweep, recheck and 403 hold on both backends.

---

## Phase 7: User Story 5 — One migration sequence serves both backends (Priority: P2)

**Goal**: FR-024 – FR-031 beyond what US1 needed — validation, refusals, the file lock, tools, parity and conventions ([contracts/migrations.md](contracts/migrations.md)).

**Independent test**: quickstart §4 and §7, plus the two-migrator test.

- [X] T083 [P] [US5] Write `internal/database/migrate_test.go`:
  - `ValidateMigrations` fails for a missing down file in either view, for two SQLite baselines, and for a duplicate version after the rules;
  - SQLite `MigrateToVersion(50)` / `ForceMigrationVersion(50)` are refused, naming baseline 073;
  - `MigrateDown` / `MigrateReset` on SQLite reach an empty database through the baseline's down file.

  RED.
- [X] T084 [P] [US5] Write `internal/database/migrate_lock_test.go` (SC-007): two migrators as two processes (helper-process re-exec) against one SQLite file, 20 runs. Both exit 0; `PRAGMA integrity_check` returns `ok`; the version is the newest; `schema_migrations` holds one row. RED: no file lock yet.
- [X] T085 [P] [US5] Write `internal/database/migrate_roundtrip_db_test.go` on both legs (constitution v1.3.0 quality gate): migrate an empty database up to the newest version, all the way down (PostgreSQL through 001; SQLite through the baseline's down file) and up again; then step 074 down and up. After each up the schema equals the first up's (the parity introspection of T086). RED until T088.
  - The PostgreSQL round trip needed the down files of `010`, `019`, `028` and `037` corrected (each dropped what an earlier migration owned). First as `.postgres.down.sql` overrides; since 2026-10-13, at the user's request, the plain down files are corrected in place — below the baseline, SQLite never reads them.
- [X] T086 [P] [US5] Write `internal/database/parity_test.go` (`TestSchemaParity`; needs the PostgreSQL URL, SQLite in a temp file) per R11. It compares tables, columns, nullability, normalised defaults, primary/foreign/unique keys with delete actions, CHECKs, seed rows (by natural key where ids are random) and indexes (partial predicates included; the GIN index is the documented exception). RED until T048.
- [X] T087 [US5] Implement `internal/database/migrate_lock.go` (unix build constraint): `syscall.Flock` on `<db>.migrate-lock` (0600), polled every 100 ms up to 15 s, error naming the file. Every SQLite migration run takes it. T084 → GREEN.
- [X] T088 [US5] Extend `internal/database/migrate.go`: `ValidateMigrations` checks both views; the below-baseline refusal in `MigrateToVersion` / `ForceMigrationVersion`. T083 → GREEN.
- [X] T089 [US5] Make `cmd/lightstack-migrate/main.go` backend-aware: the backend comes from `database.url` / `LIGHTSTACK_DB_URL`, and `status`, `up`, `down`, `reset`, `goto` and `force` work on both.
- [X] T090 [US5] Change the `Makefile`: `migrate:` (`:195-197`) → `LIGHTSTACK_DB_URL="$(DB_URL)" go run ./cmd/lightstack-migrate up -migrations migrations`; `install-tools` drops golang-migrate's `migrate` CLI line (`:240`).
- [X] T091 [US5] Write `migrations/README.md`: the three rules, file naming, the baseline and its generator (T047), and the conventions of R20 — what fits one plain file; when a pair is needed (non-constant default or `NOT NULL` on `ADD COLUMN`, type or constraint changes, rebuilds only for tables nothing references); ids generated by the application.
- [X] T092 [US5] Run `TestSchemaParity` in the `unit-tests` job of `.github/workflows/ci.yml` (PostgreSQL available). T086 → GREEN. (Already the case: the PostgreSQL step runs `go test ./...` with a postgres URL, which runs `TestSchemaParity` and the PostgreSQL round trip; the SQLite step runs the SQLite round trip. No separate step.)

**Checkpoint**: both backends migrate from one folder, are proven equal, and are locked safely.

---

## Phase 8: User Story 6 — Contributors learn early when SQLite cannot run their SQL (Priority: P3)

**Goal**: FR-018 and SC-009 ([contracts/sql-dialect.md](contracts/sql-dialect.md) §3, §5; research R9).

**Independent test**: a seeded construct makes the check fail with file, line and construct; removing it makes it pass.

- [X] T093 [US6] Write the seeded cases in `internal/database/sqlcheck_test.go`: one synthetic Go file per construct class in contract §3 — `~*`, `!~`, `ILIKE`, `@>`, a `jsonb_*` function, `ARRAY_AGG`, `ARRAY[`, `DISTINCT ON`, `generate_series`, `EXTRACT`, `INTERVAL`, `CURRENT_TIMESTAMP`, `FOR SHARE`, `$$`, an unknown cast, `ANY(` outside `= ANY($n)`, an unlisted function. Each must be reported with file, line and construct. RED.
- [X] T094 [US6] Implement the scanner in `internal/database/sqlcheck_test.go` (`TestSQLConstructs`):
  - `go/parser` over `internal/**.go` and `test/**.go` (production, test and contract-suite files);
  - SQL detection by keyword and by `internal/common/sqlbuilder.go` method arguments;
  - the translator's lexer;
  - function allow-list and syntax deny-list (R9);
  - the `//sqlcheck:postgres-only` exemption.

  T093 → GREEN.
- [X] T095 [US6] Run `TestSQLConstructs` on the tree and fix anything it reports (expected: nothing left after T034–T038). Document the supported construct set in `internal/database/doc.go` (package documentation, FR-018).

**Checkpoint**: new PostgreSQL-only SQL fails CI before review.

---

## Phase 9: Polish & cross-cutting

- [X] T096 [P] Write `docs/adr/ADR-003-pluggable-database-backend.md`: the contract, scheme selection, pure-Go driver, translation, one migrations folder, and the reversal of ADR-001's rejection. Add an amendment note to `docs/adr/ADR-001-database-dependency-injection.md` (`:54-57`) and the index entry in `docs/adr/README.md`.
- [X] T097 [P] Update `docs/CONFIGURATION.md`: accepted `database.url` schemes and the SQLite form; `LIGHTSTACK_DB_BACKEND` and `LIGHTSTACK_NODE_NAME` in the environment table (`:141`); which pool settings apply to SQLite (`health_check_period` is PostgreSQL-only); file modes and the recommended path.
- [X] T098 [P] Update `docs/ARCHITECTURE.md`: the injection section (`:215-222`) now shows `database.Open` / `database.DB`; describe the two backends and the migration view.
- [X] T099 [P] Update `CLAUDE.md`:
  - the dependency-injection paragraph and the database-query idiom (`database.DB`, `database.ErrNoRows`);
  - the migration rules;
  - "PostgreSQL with 15 tables" in Database Schema;
  - the troubleshooting and real-mode notes that name PostgreSQL only.
- [X] T100 [P] Update `docs/UPGRADING.md`: `make migrate` now runs `lightstack-migrate`; golang-migrate's `migrate` CLI no longer reads the folder; a server create refused for quota answers `403` instead of `413` (FR-035a); deleting or extending a volume, or deleting a snapshot, while it is still `creating` answers `400`, as upstream (FR-036a); migration 074.
- [X] T101 [P] Update `docs/DOCKER_DEPLOYMENT.md` (`:389-392`, `:522-526`): `lightstack-migrate` works on both backends; SQLite paths.
- [X] T102 Verify constitution v1.3.0 in `.specify/memory/constitution.md` (amended 2026-10-12) is committed, and that the implementation meets its clauses:
  - Article II: every reservation follows the four limits (T074, T075, T077);
  - Article III: the SQLite busy wait is at most 5 s, and no code retries a failed statement or transaction (T081);
  - Article VI: the stripped binary is close to the stated ~50MB — update the figure in the same amendment if it is not. (Since 2026-10-13 Article VI says "small binary", no figure.)
  - Verified 2026-10-12: the four limits hold for volume and snapshot creates (reserve under the quota lock, counted by usage; backing outside any transaction — T082 audit; finalise or remove before answering, same response; one startup sweep before the listeners). The busy wait is 5 s and nothing retries a statement or transaction; the migration file lock waits up to golang-migrate's own 15 s, as PostgreSQL's migration lock does — it is not the database write lock. Stripped binary: 51.2 MB; Article VI, README.md and CLAUDE.md now say "small binary" instead of a figure (2026-10-13). The amendment is in the working tree; committing it is the user's.
- [X] T103 Rebase onto `main` before the final gates (`migrations/`, `docs/specs/028-sqlite-database-layer/`): if `074` is taken there, renumber `074_reservation_node`; if `main` gained migrations, regenerate the SQLite baseline with T047 at the newest one; re-run T086 (parity) and T085.
- [X] T104 Final gates on both backends: unit tests on both legs; `make lint`; the contract suite and `quick_test.sh` on both; `make test-horizon-ui` on both backends (FR-035a changes the quota error Horizon shows); the migration round trip (T085) on both legs. Compare the PostgreSQL results with `baseline-gates.md`. The only allowed differences are the new tests and the corrections of FR-035a and FR-036a.
- [ ] T105 Validate real mode on a host (quickstart §9), on SQLite and on PostgreSQL. Include a kill during an image-backed volume create, a restart, and a check that the volume is `error` and its file is gone. Generate the runbook with the host-testing workflow into `docs/specs/028-sqlite-database-layer/HOST_TESTING.md`.
  - Runbook written: `HOST_TESTING.md` (SQLite and PostgreSQL passes, the kill during an image-backed volume create, zsh-paste-safe, `zsh -n` clean). Since 2026-10-14 it runs Lightstack through `deployments/docker-compose-kvm.yml` (T107); since 2026-10-26 a third pass installs the chart on a one-node k3s cluster with a SQLite URL (§4). The host run itself is pending.
- [X] T107 Make `deployments/docker-compose-kvm.yml` run either backend (FR-042; decided 2026-10-14, before T105):
  - write `test/compose_kvm_state_test.sh` first — it renders the stack with `compose config`; RED: four subdirectory mounts, no override;
  - mount the whole state root at the same path with `${LIGHTSTACK_STATE_PATH:-/var/lib/lightstack}`, the config directory with `${LIGHTSTACK_CONFIG_DIR:-/opt/lightstack/config}`, and pass `LIGHTSTACK_DB_URL` through; GREEN, and `make test-compose-kvm`;
  - `deployments/README-KVM.md` (both backends, the three variables) and `docs/DOCKER_DEPLOYMENT.md`.
- [X] T108 Close the two chart gaps this feature opens (FR-043, FR-044; decided 2026-10-14):
  - helm-unittest first — `charts/lightstack/tests/node_name_test.yaml` (Deployment in stub and real mode, DaemonSet) and three purge-Job cases in `teardown_test.yaml`; RED: no node name, and the purge Job has no env;
  - `LIGHTSTACK_NODE_NAME` from `spec.nodeName` on the Lightstack container (`lightstack.podSpec`), and `LIGHTSTACK_DB_URL` from `database.existingSecret` on the purge Job through a helper shared with the workload (`lightstack.dbUrlEnv`); `extra_env_test.yaml` and `tls_test.yaml` asserted an absent env block and now assert the node name alone; GREEN, `make chart-test`;
  - `charts/lightstack/README.md`, `docs/CONFIGURATION.md`, `docs/UPGRADING.md`.
- [X] T106 Re-check `docs/specs/028-sqlite-database-layer/data-model.md`, the contracts and research.md against the final code: the `_txlock` and busy-wait values, messages and line references. Mark the spec Status "Implemented".

---

## Dependencies & Execution Order

### Phase dependencies

- **Setup (Phase 1)** → **Foundational (Phase 2)** blocks everything; T022 must match T001.
- **US2 (Phase 3)** needs only Phase 2. Its gate (T022) is re-run at the end of every later phase.
- **US1 (Phase 4)** needs Phase 2. It is the MVP.
- **US3 (Phase 5)** needs Phase 2. Its SQLite-form cases need T042 (US1) to exercise `Open`, but the validation itself does not.
- **US4 (Phase 6)**: its PostgreSQL parts (T062–T079, T082) need only Phase 2. T080/T081 and the SQLite legs of T065, T067 and T068 need US1.
- **US5 (Phase 7)** needs US1's view, migrator and baseline (T045–T048).
- **US6 (Phase 8)** needs US1's translator lexer (T040). T095 expects US1's rewrites (T034–T038).
- **Polish (Phase 9)** needs every story it documents. T103, T104 and T105 come last.

### Within each story

Tests first and RED; then implementation; then the story's checkpoint and a re-run of T022.

### Parallel opportunities

- Phase 2: T011–T018 touch different packages and run in parallel after T006–T010. The build is only green when all are done (T020).
- US1: the tests T026–T033 and the rewrites T034–T038 run in parallel; T039–T043 run in sequence (one package, shared types).
- US4: the tests T062–T071 run in parallel. The Cinder (T074–T077) and Nova (T078–T079) implementations run in parallel with each other.
- US5: T083–T086 run in parallel.
- Polish: T096–T101 run in parallel.

## Parallel Example: User Story 1

```bash
# Tests first, all at once:
Task: "Translator table tests in internal/database/sqlite_translate_test.go"           # T026
Task: "Value conversion tests in internal/database/sqlite_values_test.go"               # T027
Task: "Open/settings/functions/errors tests in internal/database/sqlite_test.go"        # T028
Task: "SNAT predicate test in internal/neutron/router_snat_db_test.go"                  # T029
Task: "Migration view tests in internal/database/migrate_view_test.go"                  # T031

# Rewrites, one file each:
Task: "Tenant-usage hours in Go in internal/nova/tenant_usage.go"                       # T034
Task: "VNI allocation WITH RECURSIVE in internal/neutron/vxlan_coordinator.go"          # T035
Task: "DISTINCT ON → ROW_NUMBER in internal/neutron/reconciler.go"                      # T037
```

## Implementation Strategy

### MVP first

1. Phase 1 + Phase 2 — one contract, PostgreSQL identical (T022).
2. Phase 3 (US2) — the PostgreSQL guarantees, pinned by tests.
3. Phase 4 (US1) — Lightstack on SQLite; the contract and quick suites green in CI. **Stop and validate**: quickstart §1, §3, §6.

### Incremental delivery

4. US3 — refusals; the chart in part 2 relies on `LIGHTSTACK_DB_BACKEND`.
5. US4 — reservations, sweep, recheck, 403, and the `_txlock` decision.
6. US5 — validation, file lock, tools, parity.
7. US6 — the static check.
8. Polish — docs, ADR-003, constitution proposal, final gates, real-mode host run.

Each step ends with T022 (PostgreSQL unchanged) and its own checkpoint. Commit points are the branch owner's call.
