# Tasks: Squash the migrations, restart at 001, one migration per release

**Feature**: `032-migration-squash-per-release` (cloud-storage#596)
**Input**: Design documents from `docs/specs/032-migration-squash-per-release/`
**Prerequisites**: plan.md, spec.md (required); research.md, data-model.md, contracts/, quickstart.md (present)

**Tests**: INCLUDED. The constitution makes test-first development non-negotiable, and the plan's delivery
sequence puts RED tests before code. Every behaviour task below has a failing test written first.

**Organization**: grouped by the spec's user stories, in priority order: US1 and US2 (P1), then US3, US4 and US5 (P2).
The stories build on each other (see Dependencies). Each phase ends at a checkpoint where the whole suite is green.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel: a different file, and no dependency on an incomplete task
- **[Story]**: US1–US5 for story-phase tasks; Setup, Foundational and Polish carry none
- Every task names exact file paths

## Path conventions

A single Go project at the repository root (`cmd/`, `internal/`, `migrations/`, `scripts/`, `test/`, `docs/`), as
plan.md § Project Structure lays out. The design references in tasks point at research.md (R1–R17),
data-model.md (§1–§7) and the three files in contracts/.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: an oracle for "nothing else changed", a fixed squash point, and a test strategy the user has approved.

- [X] T001 Record the baseline gates before any change. Run `make fmt`, `make lint`, `go build ./cmd/lightstack ./cmd/lightstack-migrate`, then `go test ./...` twice: once with `LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable'` (after `make db-down && make db-up`) and once with `LIGHTSTACK_TEST_DB_URL='sqlite:///tmp/ls-test/t.db'`. Keep the outputs, and list any pre-existing failure so later tasks are not blamed for it.
  - Done 2026-11-18: both binaries build; `gofmt -l` is clean; `make lint` reports 0 issues. The unit suite (contract packages excluded, `-p 1`) passed on PostgreSQL, a dedicated database in the `make db-up` container, and on SQLite: 19 packages ok, 0 failures, 5 without tests, on each backend. No pre-existing failures.
- [X] T002 Fix the squash point. Rebase `feature/596` onto `main` once every numbered migration meant to be folded in has merged. Record the newest version with `ls migrations | grep -oE '^[0-9]{3}' | sort -u | tail -1` (`082` today); T005 makes it `SquashPoint`. If `main` gains a numbered migration later, rebase and redo T005 and T015–T017 (spec Dependencies).
  - Done 2026-11-18: `SquashPoint` = 82, the newest version on this branch. `main` stops at `075` today, and this branch already holds `076`–`082`. Rebase onto `main` once they have landed there, then regenerate (T064).
  - Redone 2026-11-26 on the rebase onto `main` (`7202a5f`), which holds `076`–`083`: `squashPoint` = 83. `083_token_revocations` is the only schema it adds; `main`'s edits to `076`–`081` touch comments only.
- [X] T003 Present the test strategy to the user before writing any test, and wait for approval (constitution, Test-First step 2). Cover:
  - the failing tests of each phase and the file each lives in: T004 and T008 (Foundational), T010–T013 (US1), T023–T026 (US2), T036–T038 (US3), T045–T046 (US4), T050–T051 (US5);
  - which backend each runs on: PostgreSQL through `make db-up`, SQLite through temporary files, and both in CI's `unit-tests` job;
  - the bash test for the upgrade-script guards (T026);
  - the two CI checks verified by a dry run rather than a test: the immutability step (T054) and the tag job (T055);
  - what stays deliberately untested: golang-migrate's own dirty handling, and a real multi-node race (a baseline in flight defers to golang-migrate's lock, research R9).

  Record any change the user asks for in this file before continuing.
  - Done 2026-11-18: the user approved the strategy as presented, with no changes.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: pieces several stories share:
- the squash point constant;
- the migration body, checksum and record helpers (data-model §4);
- the `applied_migrations` DDL (data-model §2);
- PostgreSQL introspection as production code (research R2 and R10);
- a test helper that migrates a folder without the post-squash checks.

**⚠️ CRITICAL**: no user story work can begin until this phase is complete.

- [X] T004 [P] Write RED unit tests in `internal/database/migration_body_test.go` for the helpers T006 adds (pure functions, no database):
  - `checksumBody(body []byte) string` returns the lower-case SHA-256 hex.
  - `renderUp(header string, version uint64, body []byte) []byte` emits the header line, `-- checksum: <hex>`, the body, then `INSERT INTO applied_migrations (version, checksum) VALUES (<N>, '<hex>');`, exactly as data-model §4.
  - `renderDown(header string, version uint64, body []byte) []byte` emits the header line, then `DELETE FROM applied_migrations WHERE version = <N>;`, then the body.
  - `parseChecksumHeader(up []byte) (string, error)` returns the hex from the second line, and errors when that line is missing, not `-- checksum: `, or not 64 hex digits.
- [X] T005 [P] Create `internal/database/migrate_state.go` holding only `const SquashPoint = <T002's value>` (type `uint64`) and a doc comment from data-model §5.
  - Superseded in part by F11: production code keeps no squash-point constant; the baseline generators keep their own `squashPoint`.
- [X] T006 Implement T004's helpers in `internal/database/migration_body.go` (FR-019) (stdlib `crypto/sha256` and `encoding/hex`). Add `var appliedMigrationsDDL = map[Backend]string{Postgres: …, SQLite: …}` holding exactly the two `CREATE TABLE applied_migrations` statements of data-model §2. T004 green.
- [X] T007 Move PostgreSQL introspection into production code. Create `internal/database/introspect_postgres.go` and move `pgColumn`, `pgTable`, `introspectPostgres` and `dependencyOrder` out of `internal/database/schema_introspect_test.go`; `adminPostgresURL` and `scratchPostgres` stay in the test file.
  - Give `introspectPostgres` a `schema string` parameter that replaces the hard-coded `'public'` in all its queries: `information_schema.tables` and `.columns`, `pg_constraint` through `(quote_ident($schema) || '.' || quote_ident($1))::regclass`, and `pg_indexes.schemaname`.
  - Add new fields, leaving the existing ones and their contents unchanged so `postgresModel` (`schema_model_test.go`) and `renderSQLiteBaseline` keep working:
    - `constraints []pgConstraint{name, kind, def}`, from `conname`, `contype` and `pg_get_constraintdef`;
    - `pkName`;
    - `comment` on table and column (`obj_description`, `col_description`);
    - `sequences []pgSequence{name, column, dataType, lastValue, isCalled}` for the sequences its columns own (`pg_depend` with `deptype = 'a'`).
  - Update every caller to pass `"public"`. `go test ./internal/database -run TestSchemaParity` stays green on PostgreSQL.
  - Done 2026-11-18. Introspection reads through `pgQuerier` (a connection or a transaction), so re-stamp can introspect inside its transaction. Foreign-key parents are read as bare relation names, so a scratch schema orders the same as `public`. The "index backs a constraint" exclusion is scoped to the index's own schema. The `SELECT *` rows query uses `pgx.QueryExecModeDescribeExec`: a cached plan fails once the table changes. A sequence's "is called" state is folded into `lastValue`, which is nil until first use, as `pg_sequences` reports it. The catalog queries carry `//sqlcheck:postgres-only`. Regenerating `073_baseline.sqlite` with the moved code is byte-identical.
  - Superseded by F11: with re-stamp gone, the introspection is test code again (`introspect_postgres_test.go`). Only `pgQuerier` stays in production, for the state check.
- [X] T008 In `internal/database/introspect_postgres.go`, add `normalizeSchema(tables []pgTable, schema string) []pgTable`. It strips `<schema>.` qualifiers from index definitions, constraint definitions, defaults and `nextval(...)` arguments, so a model read from a scratch schema compares equal to one read from `public` (research R10).
  - Add `diffPostgresExact(live, ref []pgTable, exclude ...string) []string`. It lists each difference as `missing <object>`, `different <object>: <a> ≠ <b>` or `extra <object>`, covering tables, columns (type, length, nullability, default), constraints by name and definition, primary key, indexes, comments and sequences.
  - Prove both in `internal/database/introspect_postgres_test.go`, which skips without a `postgres://` URL. Create the same table in two schemas of a scratch database: the diff is empty. Change one default in one schema: the diff names it.
  - Done 2026-11-18. The diff also covers column order and comments. `TestNormalizeSchemaStripsOnlyTheSchemaQualifier` pins the whole-identifier match.
  - Superseded in part by F11: these functions moved into `introspect_postgres_test.go`.
- [X] T009 [P] Add the test helper `migrateFolderNoChecks(t *testing.T, dbURL string, src fs.FS, target uint64)` (target 0 means newest) in `internal/database/migrate_helpers_test.go`. It drives golang-migrate over `newMigrationView(src, backend)` with `iofs` and `newMigrator`, exactly as `withMigrator` does. It skips `ValidateMigrations` and any database classification, because the pre-squash folder fails both.
  - Add `dropBelow(src fs.FS, version uint64) fs.FS`, a filtered view that hides numbered files below a version. It reproduces the retired rule 3 for replaying the old SQLite history (research R4).
  - Done 2026-11-18, with `TestDropBelow`. Until T010 calls `migrateFolderNoChecks`, `make lint` reports it as unused; that is the only lint finding.

**Checkpoint**: helpers, DDL, introspection and the test harness are ready. The suite is still green on the old folder.

---

## Phase 3: User Story 1 — A new database starts from one baseline that equals the history (Priority: P1) 🎯 MVP

**Goal**: `migrations/` holds `001_baseline.{postgres,sqlite}.{up,down}.sql` and nothing older. A fresh database
applies one migration and matches the history: schema and seed rows equal, install-time values still generated
(FR-001–FR-008).

**Independent test**: `TestBaselineEquivalencePostgres`, `TestBaselineEquivalenceSQLite` and
`TestBaselineGeneratesInstallTimeIDs` pass. `TestMigrationsFolder` and `TestMigrationsRoundTrip` pass on both
backends (quickstart §1–§2).

### Tests for User Story 1 (write first, confirm RED)

- [X] T010 [P] [US1] Write `internal/database/equivalence_test.go` (FR-003, FR-004, FR-005):
  - `TestBaselineEquivalencePostgres` skips without a `postgres://` URL (`adminPostgresURL`).
    - Create two `scratchPostgres` databases. Migrate one with `migrateFolderNoChecks(t, url, os.DirFS("testdata/pre-squash"), 0)` and the other with `MigrateUp(url, "../../migrations")`.
    - Introspect both with `introspectPostgres(ctx, conn, "public")`. Compare with `diffPostgresExact`, excluding `schema_migrations` and `applied_migrations`.
    - Compare seed rows through `normalizeRow`: generated ids and install-time timestamps are masked, and the generated-id source is the testdata history for one side and the baseline for the other.
    - Fail listing every difference. Assert that more than 50 tables were compared.
  - `TestBaselineEquivalenceSQLite` needs no environment variable; it uses temp files.
    - Migrate the old history with `migrateFolderNoChecks(t, "sqlite://"+a, dropBelow(os.DirFS("testdata/pre-squash"), 73), 0)`, and the new one with `MigrateUp("sqlite://"+b, "../../migrations")`.
    - Compare `sqliteModel` of both with `diffModels`, excluding `applied_migrations`. The five trigger-guarded `project_id` columns compare equal, because `sqliteModel` reads trigger guards as NOT NULL (FR-005).
  - `TestBaselineGeneratesInstallTimeIDs` uses the backend `LIGHTSTACK_TEST_DB_URL` selects. Two fresh databases migrated from `../../migrations` hold different values in every seed id column whose default is `gen_random_uuid()` and whose value no migration spells out (US1 scenario 3).
  - `TestBaselineProjectIDNotNull` uses the backend `LIGHTSTACK_TEST_DB_URL` selects (FR-005). Migrate a fresh database from `../../migrations`. For each of volumes, ports, networks, routers and floating_ips, build one `INSERT` from the table's catalog (`information_schema.columns` on PostgreSQL, `pragma_table_info` on SQLite):
    - every NOT NULL column without a default gets a dummy value of its own type;
    - `project_id` gets `NULL`.

    Each insert fails, and `IsNotNullViolation(err)` is true on both backends. Both backends check NOT NULL before CHECK and foreign keys, so the dummies cannot mask the violation.

  All four fail now: neither `testdata/pre-squash` nor the baselines exist.
  - Done 2026-11-18. RED confirmed: both equivalence tests (no history in testdata), and `TestBaselineProjectIDNotNull` on SQLite, where the old trigger raises error 1811, not a NOT NULL violation. `TestBaselineGeneratesInstallTimeIDs` on both backends, and the NOT NULL test on PostgreSQL, already hold on the history; they pin behaviour the squash must keep. The NOT NULL test also asserts that the violation names `project_id`, so a NULL in another column cannot make it pass.
- [X] T011 [P] [US1] In `internal/database/migrate_test.go` (FR-006, FR-026):
  - Add `TestMigrationsFolder`: `ValidateMigrations("../../migrations")` passes, version 001 is exactly the four `001_baseline.{postgres,sqlite}.{up,down}.sql` files, and every other numbered file matches `^\d{3}_v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\.(postgres|sqlite))?\.(up|down)\.sql$`.
  - Rewrite the fixtures of `TestValidateMigrations` (around lines 26–70) to the grammar in contracts/migrations-folder.md § Validation:
    - the baseline-pair rule;
    - a hand-numbered `002_add_x.up.sql`, which fails with `schema changes go into fragments (migrations/next/)`;
    - non-consecutive release versions;
    - one release on two versions.
  - Delete the below-baseline cases (around lines 160–175, "SQLite starts at its baseline 073; version 50 is below it"), because the rule is retired (FR-006).
  - Move the version-pinned cases at around line 149 (`force 74`) and around line 288 (`latest+1`) to post-squash numbers.

  Fails now.
- [X] T012 [P] [US1] Rewrite the fixtures in `internal/database/migrate_view_test.go` to the post-squash layout. Use `001_baseline.*` and `002_v0.7.0.*` in place of `073_baseline.sqlite.*` and `073_volume_transfer.*`, and delete the `baselineVersion` test (around line 97). Assert rule 1 (the other backend's files are dropped) and rule 2 (a backend's own file replaces the plain one), and that no version is skipped below a baseline. Fails now.
- [X] T013 [P] [US1] In `internal/database/backup_test.go` (around lines 495–550), drop `baselineVersion`. The oldest version a SQLite file can hold is now `1`. Keep each case's intent ("an older backup migrates forward", "a newer one is refused") using versions 1 and latest+1. Fails now: the old folder's SQLite view starts at 073.
  - Done 2026-11-18. Both tests now run on `migrationsWithProbeRelease` (in `migrate_helpers_test.go`): a copy of the folder plus one probe release past its newest version, so an older version exists whatever the real folder holds. The "version below the SQLite baseline" case is gone with the rule; `TestDatabaseState` covers the pre-squash case.

### Implementation for User Story 1

- [X] T014 [US1] Move the history: `git mv migrations/[0-9][0-9][0-9]_*.sql internal/database/testdata/pre-squash/` (FR-001, FR-007; research R1). That moves all 180 files, the `073_baseline.sqlite` pair included; `migrations/README.md` stays. Check that `ls internal/database/testdata/pre-squash | wc -l` prints 180 and `git status` shows renames.
  - Done 2026-11-18: 180 renames into `internal/database/testdata/pre-squash/` (`git mv`, staged).
- [X] T015 [US1] Retarget the SQLite generator in `internal/database/baseline_gen_test.go` (FR-001, FR-002, FR-003, FR-005; research R3):
  - Replace `baselineTarget` with `SquashPoint`, and fail if it is not the newest version in `testdata/pre-squash`.
  - Migrate the scratch PostgreSQL database with `migrateFolderNoChecks(…, os.DirFS("testdata/pre-squash"), SquashPoint)`. Give `migrationsSource` a directory parameter and read the testdata history.
  - Write `migrations/001_baseline.sqlite.up.sql` as `renderUp("-- baseline: the schema and seed data of the pre-squash migrations up to <SP>; generated by TestGenerateSQLiteBaseline, do not edit", 1, body)`, where `body` is today's rendering plus `appliedMigrationsDDL[SQLite]`.
  - Write `.down.sql` as `renderDown(<same header>, 1, <today's drop list> + "DROP TABLE \"applied_migrations\";")`.
  - Assert that `project_id` on volumes, ports, networks, routers and floating_ips renders `NOT NULL`, and that the output holds no `CREATE TRIGGER`.
  - Done 2026-11-18. The header comes from `baselineHeader(SQLite, …)`, and `requireSquashPointIsNewest` guards both generators. The SQLite renderer needed no new mapping: 75 tables, the 74 of the history plus `applied_migrations`.
- [X] T016 [US1] Add `TestGeneratePostgresBaseline` in `internal/database/baseline_gen_postgres_test.go`. It runs only with `-update` and a `postgres://` URL, and skips otherwise (FR-001, FR-002, FR-003; research R2).
  - Migrate a scratch database with `migrateFolderNoChecks(…, os.DirFS("testdata/pre-squash"), SquashPoint)`, then render from `introspectPostgres(…, "public")`:
    1. one `CREATE TABLE` per table, with columns typed from `udt` (`varchar(n)`, `_text`→`text[]`, `jsonb`, `uuid`, `timestamptz`, `bool`, `int4`, `int8`, …), `NOT NULL`, defaults, and named `PRIMARY KEY`, `UNIQUE` and `CHECK` constraints. An `int4` column whose default is `nextval('<t>_<c>_seq'::regclass)` and which owns that sequence renders as `SERIAL`;
    2. foreign keys as `ALTER TABLE … ADD CONSTRAINT <name> <def>`;
    3. indexes from `indexdef`, unqualified;
    4. `COMMENT ON TABLE` and `COMMENT ON COLUMN`;
    5. seed rows in `dependencyOrder`, with generated ids as `gen_random_uuid()` (`generatedID`) and timestamps as `now()`;
    6. `appliedMigrationsDDL[Postgres]`.
  - Wrap the result with `renderUp` and `renderDown` (header as in T015, naming this test). The down part drops every table in reverse dependency order, then `applied_migrations`.
  - Stop with an error on any `udt`, default or object kind it does not map.
  - Apply up, down, up to a second scratch database before writing `migrations/001_baseline.postgres.{up,down}.sql`.
  - Done 2026-11-18. One CHECK does not survive a round trip through PostgreSQL's own printed form: `port_forwardings_protocol_check`, written as `protocol IN ('tcp', 'udp')` on a varchar. `postgresCheckSource` renders it in its source form. The generator also checks itself: it introspects the applied baseline, diffs it against the history's catalog, and stops on any difference.
- [X] T017 [US1] Generate the baselines (FR-001) with `LIGHTSTACK_TEST_DB_URL=<postgres URL> go test -count=1 ./internal/database -run 'TestGenerate(Postgres|SQLite)Baseline' -update`. Run it a second time and confirm `git diff --exit-code migrations/`, which shows the output is byte-identical (FR-002).
  - Done 2026-11-18. A second run is byte-identical; checked with `sha256sum -c`, since the new files are untracked and `git diff` cannot show them.
- [X] T018 [US1] Retire view rule 3 and switch validation to the new grammar in `internal/database/migrate_view.go`, `internal/database/migrate.go` and `cmd/lightstack-migrate/main.go` (FR-006, FR-026; research R8, R15):
  - In `internal/database/migrate_view.go`: delete `baselineVersion`, the `baseline` parameter and the skip `backend == SQLite && f.version < baseline` in `selectFiles`, and the SQLite branch in `newMigrationView`.
  - In `internal/database/migrate.go`: delete `RefuseBelowBaseline` and its calls in `MigrateToVersion` and `ForceMigrationVersion`.
  - In `cmd/lightstack-migrate/main.go`: delete the `RefuseBelowBaseline` calls in `handleGoto` and `handleForce`.
  - Rewrite `ValidateMigrations` to the numbered-file rules of contracts/migrations-folder.md § Validation:
    - version 001 is exactly the four baseline files;
    - later files are `NNN_v<semver>[.postgres|.sqlite]`, consecutive from 002, one release per version, and no release twice;
    - anything else fails with `<file>: a numbered migration other than the baseline or a release migration; schema changes go into fragments (migrations/next/)`.

    Keep `validateView`'s up/down checks.

  T011, T012 and T013 green.
  - Done 2026-11-18. The grammar lives in `validateFolderNames` (`migrate_view.go`), which `ValidateMigrations` calls before the per-view checks. The test keeps its own pattern, `releaseFileNameSpec`, independent of the implementation's.
- [X] T019 [US1] Make T010 (`internal/database/equivalence_test.go`) green (FR-004). Run `TestBaselineEquivalencePostgres`, `TestBaselineEquivalenceSQLite` and `TestBaselineGeneratesInstallTimeIDs`. Fix every difference they report in the generators (T015, T016) and regenerate (T017). Never edit a generated file by hand (FR-002), and never widen the comparison beyond `schema_migrations`, `applied_migrations` and the FR-005 exception.
  - Done 2026-11-18. SQLite was equal on the first run. PostgreSQL differed in exactly one constraint, fixed in the generator (T016) and never by editing a file. All four T010 tests pass on both backends.
- [X] T020 [US1] Retire the tests tied to one pre-squash migration (FR-007, R15): delete `internal/keystone/groups_domain_migration_test.go` (072), `internal/database/migration_080_sqlite_test.go` (080) and `test/endpoint_hostname_migration_test.sh` (069). Remove the `test-endpoint-migration` target from `Makefile`, together with its comment block and its `.PHONY` entry.
  - Done 2026-11-18 (`git rm`, staged). Nothing else references the removed files or target.
- [X] T021 [P] [US1] Adapt `test/migration_test.sh`: tests 3–5 count `migrations/*up.sql` and read the newest number with `ls … | tail -1`. Count top-level numbered files only, accept the `001_baseline.*` names and release names matching `NNN_v<digit>…`, and ignore `migrations/next/`.
  - Done 2026-11-18. The naming check accepts the baseline files and a release-candidate name, and rejects a hand-numbered file; `bash -n` is clean.
  - Superseded by F8: the script is removed.
- [X] T022 [US1] Get US1 green on both backends (`./internal/database`, `./cmd/lightstack`, the whole module):
  - Recreate the shared PostgreSQL first (`make db-down && make db-up`); the old one is pre-squash.
  - Run `go test ./...` with each `LIGHTSTACK_TEST_DB_URL`, covering `TestSchemaParity`, `TestMigrationsRoundTrip` (FR-008), the three T010 tests and `cmd/lightstack`.
  - Run `make lint`.
  - Done 2026-11-18. The full unit suite passes on SQLite (19 ok, 0 failures) and on PostgreSQL. `make lint` reports 0 issues. `TestSchemaParity` needed one documented exception, `maskBackendChecksums`: `applied_migrations.checksum` differs between backends by design, because each records its own body's checksum. The table's schema and other columns are still compared. My PostgreSQL legs used a dedicated database in the `make db-up` container, recreated because the old one was pre-squash; the shared database was not reset.

**Checkpoint**: a fresh database on either backend starts from `001` and matches the history (MVP).

---

## Phase 4: User Story 2 — A database from before the squash is refused with the way out (Priority: P1)

**Goal**:
- Every entry point recognises a pre-squash database and refuses it with the way out (FR-011, FR-012).
- `docs/UPGRADING.md` says v0.7.0 is a fresh install, bare metal included (FR-013).

**Independent test**: `TestDatabaseState` and the pre-squash cases of `TestStartupRefusals` pass
(quickstart §3).

### Tests for User Story 2 (write first, confirm RED)

- [X] T023 [P] [US2] Write `TestDatabaseState` in `internal/database/migrate_state_test.go` (FR-011, FR-012). It uses the backend `LIGHTSTACK_TEST_DB_URL` selects: fresh files, or `scratchPostgres` databases.
  - Build each state of data-model §3 directly with SQL:
    - fresh: empty;
    - fresh, SQLite only: a path whose file does not exist yet. `inspectDatabase` reports fresh and creates no file; `MigrateUp` then creates the file and migrates it from `001`;
    - pre-squash: `schema_migrations(version, dirty)` holding `(82, false)` and no `applied_migrations`;
    - pre-squash and dirty: `(82, true)`;
    - pre-squash at a reused low number: `(2, false)`, for FR-012;
    - baseline in flight: `(1, true)` and no `applied_migrations`;
    - current: migrated from `../../migrations`;
    - newer: current, with `schema_migrations` then set to latest+1.
  - For every pre-squash case, `MigrateUp`, `MigrateDown`, `MigrateToVersion`, `ForceMigrationVersion`, `GetMigrationStatus` and, on SQLite, `IntegrityCheck` must return an error for which `IsPreSquash(err)` is true and whose text contains "from before the migration squash". The text must never contain "written by a newer Lightstack" or "no migration found".
  - `MigrateReset` succeeds on a pre-squash database.
  - The baseline-in-flight case is not reported as pre-squash.
  - The newer case keeps the existing "newer Lightstack" message.

  Fails now.
- [X] T024 [P] [US2] Write `TestRestamp` in `internal/database/restamp_test.go` (FR-009, FR-010). It needs PostgreSQL and skips otherwise.
  - **Success, then the next release migration** (US2 scenario 1, SC-004):
    1. Copy `../../migrations` to a temporary directory. Add a release migration `002_v0.0.0-restamp-probe.up.sql` and `.down.sql`, written with `renderUp` and `renderDown`: the body is `ALTER TABLE volumes ADD COLUMN restamp_probe TEXT;` and the down body its `DROP COLUMN`.
    2. Start from a scratch database migrated with `migrateFolderNoChecks(…, os.DirFS("testdata/pre-squash"), SquashPoint)`. Record every table's row count.
    3. `Restamp(ctx, url, <copy>, true)` succeeds. `schema_migrations` is now `(1, false)`, `applied_migrations` holds exactly `(1, parseChecksumHeader(001_baseline.postgres.up.sql))`, and every row count is unchanged.
    4. `MigrateUp(url, <copy>)` applies `002`. The database records `2`, `volumes.restamp_probe` exists, and every table still holds all of its rows.
  - **Refusals**: each refusal leaves `schema_migrations` and the schema untouched, with its message from contracts/cli-and-startup.md:
    - version 59 (`migrateFolderNoChecks` to 59);
    - dirty at `SquashPoint`;
    - an added index (`extra index …`);
    - a dropped column (`missing column …`);
    - a changed default (`different …`);
    - an empty database;
    - a `sqlite://` URL ("restamp is for PostgreSQL only").
  - A post-squash database returns the "already post-squash" result without error.
  - `Restamp(…, false)` (dry run) changes nothing even on success.
  - **Atomicity**: set the test hook `restampBeforeCommit` to return an error. Afterwards the version is still `SquashPoint` and `applied_migrations` does not exist.

  Fails now.
  - Superseded by F11: re-stamp and its test are deleted.
- [X] T025 [P] [US2] Extend `TestStartupRefusals` in `cmd/lightstack/startup_refusal_test.go` (FR-011). Give the case table a `setup func(t *testing.T, workDir string)` field, which runs before the binaries start.
  - Done 2026-11-18 as a separate `TestStartupRefusesAPreSquashDatabase` beside the table. A pre-squash refusal needs a database file to exist first, which the table's "leaves nothing behind" assertion forbids; `sqliteWithVersion` writes that file. RED confirmed: both entry points reported "written by a newer Lightstack".
  - Add a pre-squash case: `setup` writes a SQLite file holding only `schema_migrations` with `(82, false)`, and `url` points at it.
  - For both entry points (the server, and `lightstack-migrate up`), expect exit 1 and stderr containing "from before the migration squash" and not "newer Lightstack".

  Fails now.
- [X] T026 [P] [US2] Write `test/upgrade_squash_guard_test.sh` (bash, no root, no systemd). It sources `scripts/upgrade-lightstack.sh`, which must not run `main` when sourced (see T033).
  - Build a temporary git repository with a pre-squash commit (`migrations/082_x.up.sql`) and a post-squash commit (`migrations/001_baseline.postgres.up.sql`).
  - Stub `run_target_migrate_status` to print `Status: PRE-SQUASH` and return 1, and later to return 0.
  - Stub `create_backup`, `stop_service` and `update_code` to record that they were called.
  - Assert:
    - the crossing upgrade on a pre-squash database exits 1 with "This upgrade crosses the migration squash" and "Nothing was changed", and calls none of the stubbed functions;
    - with status 0 (a fresh database), it continues;
    - `--version` from the post-squash commit to the pre-squash one exits 1 with "Refusing to roll back";
    - same-side upgrades pass both guards.

  Fails now.
  - Superseded by F9: the test is deleted with the script.

### Implementation for User Story 2

- [X] T027 [US2] Implement classification in `internal/database/migrate_state.go` (FR-011, FR-012; research R9, data-model §3):
  - Done 2026-11-18. `SquashPoint` (82) lives here too, and `CheckDatabaseState` (exported in T029) wraps the classification for the entry points.
  - `type DatabaseState`: fresh, pre-squash, baseline in flight, current, newer or dirty. Stale comes in US4.
  - `inspectDatabase(dbURL, migrationsPath string) (DatabaseState, uint64 /*version*/, bool /*dirty*/, error)` opens one short connection:
    - `pgx.Connect` on PostgreSQL;
    - on SQLite, it first runs `os.Stat` on the path. A missing file is fresh and is not created, because `MigrateUp` creates it. An existing file opens with `openForInspection` from `backup.go`, read-only, so inspection never changes the file.
    - It reads `schema_migrations`; a missing table means fresh.
    - It checks whether `applied_migrations` exists: `to_regclass('applied_migrations')` on PostgreSQL, `sqlite_schema` on SQLite.
    - Pre-squash means a recorded version, no `applied_migrations`, and not (version 1 and dirty).
  - Add the error type `PreSquashError{Backend Backend; Version uint64}`. Its `Error()` returns the contract text; on SQLite, the re-stamp clause reads "(SQLite databases cannot be re-stamped)".
  - Add the predicate `IsPreSquash(err error) bool`, built on `errors.As`, so callers never match message text.
  - Superseded in part by F11: `PreSquashError` lost its `Backend` field, and its message offers one way out on both backends: recreate the database.
- [X] T028 [US2] Wire classification into `internal/database/migrate.go` (FR-011):
  - Done 2026-11-18. `GetMigrationStatus` returns the info and the `*PreSquashError` together, so `status` can print the state and the message.
  - `MigrateUp`, `MigrateDown`, `MigrateToVersion`, `ForceMigrationVersion` and `GetMigrationStatus` call `inspectDatabase` before `withMigrator` and return a `*PreSquashError` for a pre-squash database.
  - Baseline-in-flight and dirty databases defer to golang-migrate.
  - `MigrateReset` stays unchanged, and `refuseNewerVersion` keeps its message for post-squash databases above the newest version.
  - Extend `MigrationInfo` with `State DatabaseState`.
  - In `internal/database/backup.go`, `IntegrityCheck` returns a `*PreSquashError` for a pre-squash file before `requireCompleteSchema` runs.

  T023 and the `lightstack-migrate up` half of T025 green; the server half is green too, through `MigrateUp`.
- [X] T029 [US2] In `cmd/lightstack-migrate/main.go` (FR-011, FR-022):
  - Done 2026-11-18. `refuseDatabase` became the exported `CheckDatabaseState`; `refuseBeforePrompt` calls it, and the library entry points check again after the prompt. `TestMigrateCommandsOnAPreSquashDatabase` (in `cmd/lightstack/startup_refusal_test.go`, where the binaries are built) pins it. It was RED first: down, goto and force prompted and exited 0, and status printed no `PRE-SQUASH`.
  - `handleStatus` prints `Status: PRE-SQUASH`, a blank line and the error message, then exits 1, when `GetMigrationStatus` reports a pre-squash database.
  - `handleDown`, `handleGoto` and `handleForce` refuse a pre-squash database before they prompt, in the place `RefuseBelowBaseline` was called.

  T025 green.
- [X] T030 [US2] Implement `Restamp(ctx context.Context, dbURL, migrationsPath string, apply bool) (RestampResult, error)` in `internal/database/restamp.go` (FR-009, FR-010; research R10, contracts/cli-and-startup.md § restamp):
  - Done 2026-11-18. The tests found two defects in the plan, both fixed and recorded in research R10. (1) The lock key's first name is the URL path with its leading slash (`/lightstack`), which golang-migrate's driver uses when opened by URL; `current_database()` applies only when the path is empty. A new subtest holds golang-migrate's own driver lock and proves re-stamp waits for it. (2) Comparing sequence positions refused any database in use (`host_aggregates_id_seq`). Re-stamp now reads the live schema without rows (`introspectPostgresSchema`) and compares sequences by type and owner. Positions moved to `diffSequencePositions`, which the equivalence test and the generator self-check call. The success subtest runs on a database in use.
  1. A `sqlite://` URL is refused.
  2. Open a `pgx` connection and begin one transaction.
  3. Run `SELECT pg_advisory_xact_lock($1)` with the id from golang-migrate's `database.GenerateAdvisoryLockId(<database name>, <current_schema()>, "schema_migrations")`, the arguments its PostgreSQL driver uses.
  4. Read `schema_migrations`. Refuse an empty database, a dirty one, or a version other than `SquashPoint`. If `applied_migrations` exists, return the "already post-squash" result with no error.
  5. Introspect `public` with `introspectPostgres`, before touching `search_path`. Save the current value with `SHOW search_path`.
  6. Run `CREATE SCHEMA lightstack_restamp_ref` and `SET LOCAL search_path TO lightstack_restamp_ref`. Execute the bytes of `migrations/001_baseline.postgres.up.sql`, introspect `lightstack_restamp_ref`, then `DROP SCHEMA lightstack_restamp_ref CASCADE` and restore the saved `search_path` with `SET LOCAL`.
  7. Compare with `normalizeSchema` and `diffPostgresExact`, excluding `schema_migrations` and `applied_migrations`. On any difference, roll back and return the difference lines.
  8. If `apply` is false, roll back and return the "ready" result.
  9. Otherwise, in the same transaction:
     - execute `appliedMigrationsDDL[Postgres]`;
     - insert `(1, parseChecksumHeader(<baseline up>))`;
     - run `TRUNCATE schema_migrations`, then `INSERT INTO schema_migrations (version, dirty) VALUES (1, false)`;
     - call the test hook `var restampBeforeCommit func(pgx.Tx) error` if it is set;
     - commit.

  T024 green.
  - Superseded by F11: re-stamp is deleted.
- [X] T031 [US2] Add `restamp` to `cmd/lightstack-migrate/main.go` (FR-009, FR-010):
  - Done 2026-11-18. The "already post-squash" message reads its version through `GetMigrationStatus`, and so does a database re-stamped while the prompt waited. `TestRestampCommandOnAPostSquashDatabase` pins the exit-0 row on PostgreSQL; the SQLite refusal is a case of the T029 test. Checked by hand on a database built by psql: dry run, `no`, `yes`, `status`, then `restamp` again.
  - Add the command to the dispatch switch.
  - `handleRestamp` calls `Restamp(…, false)` first, so every check runs before the prompt. It then prompts with `⚠️  Re-stamp migration version <SP> to 001 (the baseline)? (yes/no): `.
  - On `yes`, it calls `Restamp(…, true)` and prints `✅ Re-stamped: the database now records 001 (baseline)`. On anything else it prints `Re-stamp cancelled`.
  - Messages and exit codes follow contracts/cli-and-startup.md: the "already post-squash" result exits 0.
  - Add the usage line to `printUsage`.
  - Superseded by F11: the `restamp` subcommand is deleted.
- [X] T032 [P] [US2] In `internal/database/dbtest/dbtest.go`, when `migrateOrExit` fails on a PostgreSQL URL and `database.IsPreSquash(err)` is true, append `(the test database: make db-down && make db-up)` to the printed error. Use the predicate from T027, never the message text. US4 extends this to the stale refusal in T048.
  - Done 2026-11-18. The message is built by `migrateFailure`, a pure function, so `TestMigrateFailure` can pin it: the remedy line comes only for a pre-squash refusal on PostgreSQL, recognised by type.
- [X] T033 [P] [US2] Add the guards to `scripts/upgrade-lightstack.sh` (research R13, contracts/ci-and-scripts.md):
  - Done 2026-11-18. `update_code` now uses the `TARGET` that `resolve_target` sets, and the `.git` check moved with it. Implementing this found three more things. (1) T026's fixture never produced a post-squash commit (`git rm` removed `migrations/`), and it inherited the caller's git config (signing). Both are fixed, and RED was re-checked against a copy of the script with the guard calls removed. (2) `run_migrations` called a `lightstack migrate` subcommand that does not exist, which started the server in the foreground, so the upgrade never finished once a guard passed. It now runs `bin/lightstack-migrate up`, which `build_binary` builds; two new cases in the bash test cover it, RED first (research R13 records the change of scope). (3) On SQLite, `status` fails on a missing file (pinned by `TestInspectCommandsDoNotCreateADatabase`), so the guard stops on a moved-away file. UPGRADING says to create the file with `lightstack-migrate up` first. `run_target_migrate_status` itself was checked end to end in a scratch clone: a pre-squash file gives 1 with PRE-SQUASH, an empty PostgreSQL database gives 0, and the worktree is removed both times.
  - Superseded by F9: the script is deleted.
  - `squash_tree <rev>` runs `git cat-file -e "<rev>:migrations/001_baseline.postgres.up.sql"`.
  - `resolve_target` runs `git fetch origin`, then resolves `origin/main` or `TARGET_VERSION` the way `update_code` does.
  - `run_target_migrate_status <rev>` creates a temporary directory with `mktemp -d`, runs `git worktree add --detach "$tmp" <rev>` and `(cd "$tmp" && go build -o "$tmp/lightstack-migrate" ./cmd/lightstack-migrate)`, then runs `"$tmp/lightstack-migrate" status -config "$CONFIG_FILE" -migrations "$tmp/migrations"`. It removes the worktree on every path and returns the status exit code.
  - `check_migration_squash`: when `HEAD` is pre-squash and the target is post-squash, it runs `run_target_migrate_status`. If that exits 1, it prints the contract messages and exits 1.
  - `check_rollback_across_squash`: when `HEAD` is post-squash and the target is pre-squash, it prints the contract refusal and exits 1.
  - Call both in `main`, after `get_current_version` and before `create_backup`.
  - Replace the final `main "$@"` with `if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi`.

  T026 green.
- [X] T034 [P] [US2] Add a new first section, "The migration squash", to `docs/UPGRADING.md` (anchor `#the-migration-squash`), covering v0.7.0, the first post-squash release (FR-013):
  - Done 2026-11-18. The section covers recreating per backend, re-stamping (bare metal, Compose and a Helm one-off pod that mounts the release's config Secret), bringing a database to 082 (the parent of the commit that removed `082_server_group_rules.up.sql`), the warning, and both guards. The warning says the script's own backup holds no database.
  - Superseded in part by F9: the guards are gone with the script, and the section gives the bare-metal steps by hand.
  - what changed (migrations restart at `001`, one migration per release);
  - that 0.6.x databases and every SQLite database are recreated;
  - that a PostgreSQL database at exactly `<SP>` can instead be re-stamped with `lightstack-migrate restamp`. Give the bare-metal command and the Compose and Helm equivalents, each as a one-off container from the same image and configuration;
  - that a database below `<SP>` can first be brought there with the last pre-squash build;
  - a warning never to run a pre-squash Lightstack on a post-squash database: on PostgreSQL it inserts the default security group's two egress rules again and stops dirty at `003`, so a rollback across the squash means recreating the database;
  - the upgrade script's two guards, and how to use them from a pre-squash installation. The installed copy of `scripts/upgrade-lightstack.sh` predates the guards, and the script calls sibling scripts, so it has to run from a checkout of the target release:
    1. `git -C /opt/lightstack fetch origin && git -C /opt/lightstack worktree add --detach /tmp/lightstack-upgrade v0.7.0`
    2. `sudo /tmp/lightstack-upgrade/scripts/upgrade-lightstack.sh --version v0.7.0`
    3. `git -C /opt/lightstack worktree remove /tmp/lightstack-upgrade`
  - what happens with the old copy: its migration step fails because the new binary refuses the database, which stays untouched, and its own rollback restores the previous binary.
  - Superseded in part by F11: the section now says v0.7.0 is a fresh install; re-stamping and bringing a database to `082` are gone.
- [X] T035 [US2] Get US2 green: T023–T026 pass on both backends, `bash test/upgrade_squash_guard_test.sh` passes, and `make lint` is clean.
  - Done 2026-11-18. Both backends are green over every package except `test/contract`, which needs a running server, as in CI. Lint reports 0 issues, and the bash test passes 7/7. The suite found two defects, both fixed. `IntegrityCheck` called a foreign golang-migrate file pre-squash: SPEC-030 FR-005a's "no Lightstack schema" now comes first, via `requireLightstackTables`. The in-flight state test wrote to a SQLite file nothing had created. ST1005 removed the final period of every re-stamp refusal, and the contract now says error texts carry none.

**Checkpoint**: P1 complete. Fresh databases start at `001`, and old ones are refused with the way out. This is a
mergeable state if the per-release half (US3–US5) is split off.

---

## Phase 5: User Story 3 — Parallel changes add schema without picking numbers (Priority: P2)

**Goal**: fragments in `migrations/next/` assemble into one open release migration per backend, in creation order,
logged as unreleased when applied (FR-014–FR-018).

**Independent test**: `TestFragments*` pass on both backends (quickstart §4, first half).

### Tests for User Story 3 (write first, confirm RED)

- [X] T036 [P] [US3] Write `internal/database/migrate_fragments_test.go` (pure, over `fstest.MapFS`):
  - Done 2026-11-18, RED first (compile). Three refinements. (1) `assembleOpen` returns `*openMigration` (nil when `next/` holds none) instead of a six-value tuple. (2) Each section comment names the fragment's file, and fragments sort by `<time>-<ticket>-<purpose>` regardless of variant, so both backends see one order. (3) A literal duplicate name cannot exist in a directory, so "a duplicate name for one backend" only arises as a name both plain and backend-specific, which that rule refuses.
  - **Grammar** (data-model §7, contracts/migrations-folder.md):
    - valid and invalid names: a 14-digit time, a numeric ticket, a purpose matching `[a-z0-9]+(-[a-z0-9]+)*`, an optional `.postgres`/`.sqlite`, and `.up`/`.down`;
    - up without down, and down without up;
    - one name both plain and backend-specific;
    - a duplicate name for one backend;
    - dot-files such as `.gitkeep` ignored.

    Check the exact error texts.
  - **Assembly** over `001_baseline.*`, `002_v0.7.0.*` and `next/` fragments:
    - the version is 003;
    - the PostgreSQL BODY is the plain and `.postgres` fragments in name order, and the SQLite BODY the plain and `.sqlite` ones;
    - each section starts with `-- fragment: <name>`;
    - the down part is the record `DELETE`, then the down parts in reverse order;
    - two fragments with the same creation time sort by ticket, then purpose, as plain byte order of their names (`…-596-b` before `…-612-a`), and the order is the same for both backends;
    - both backends get version 003 even when only a `.postgres` fragment exists;
    - the header and the record `INSERT` carry `checksumBody(BODY)`.
  - **View**: for each backend, `ReadDir(".")` lists `003_open.up.sql` and `003_open.down.sql`, `Open` returns the assembled bytes, and the `next` directory itself is never listed.

  Fails now.
- [X] T037 [P] [US3] Write `TestFragmentsApplyInOrder` in `internal/database/migrate_fragments_apply_test.go`. It uses the backend `LIGHTSTACK_TEST_DB_URL` selects and a temporary copy of `../../migrations`.
  - Done 2026-11-18. Shared helpers live in `migrate_helpers_test.go`: `copyMigrationFiles`, `writeFiles`, `databaseModel` (which `TestMigrationsRoundTrip` now uses too), `execMigrationSQL`, `withoutFragments` and `replayFragments`.
  - Add fixture fragments `20990101000000-1-create-demo` (plain `CREATE TABLE demo (id TEXT)`) and `20990101000001-2-add-demo-col` (`ALTER TABLE demo ADD COLUMN note TEXT`), each with its down part. `MigrateUp` gives a fresh database both, and up, down, up returns the same schema.
  - Swap the creation times so the dependent fragment sorts first. A per-fragment replay then fails, naming `20990101000000-2-add-demo-col`. The replay applies the numbered migrations, then each fragment's up SQL alone in name order, and reports the first one that fails.
  - Run the same per-fragment replay over the real `migrations/next/`. It passes while the folder is empty; this is the CI check for US3 scenario 5.
  - Assert that `MigrateUp` logs `applied the open release migration 002 (unreleased)` (FR-018), capturing zerolog output.

  Fails now.
- [X] T038 [P] [US3] Write `TestFragmentsMergeInEitherOrder` (SC-002) in `internal/database/migrate_fragments_merge_test.go`. Skip only if `git` is not on `PATH`; CI has it.
  - Done 2026-11-18. Git runs without the caller's configuration (signing, hooks, default branch).
  - In a temporary git repository holding a copy of `migrations/`, branch A and branch B from the same commit, each adding one fragment pair with distinct names.
  - Merge A then B on one branch, and B then A on another. Neither merge conflicts, and the two trees are identical.
  - A fresh database migrated from the result has both changes, and so does a database first migrated at the base commit.

  Fails now.

### Implementation for User Story 3

- [X] T039 [US3] Implement `internal/database/migrate_fragments.go` (FR-014, FR-015, FR-016, FR-019):
  - Done 2026-11-18.
  - the fragment-name regex and `parseFragment`;
  - `readFragments(src fs.FS) ([]fragment, error)`, which reads `next/`, ignores dot-files, and validates per contracts/migrations-folder.md;
  - `assembleOpen(src fs.FS, backend Backend) (version uint64, up, down []byte, checksum string, ok bool, err error)`, built on `checksumBody`, `renderUp` and `renderDown`, with the header `-- open release migration NNN (unreleased): assembled from migrations/next/`.
- [X] T040 [US3] In `internal/database/migrate_view.go` (FR-015, FR-016, FR-017: every reader goes through the view):
  - Done 2026-11-18. The in-memory files are `memFile` and `memFileInfo`, and `ValidateMigrations` runs `readFragments` before it builds the views.
  - `newMigrationView` calls `assembleOpen` and serves the synthetic `NNN_open.up.sql` and `.down.sql` from memory. It adds them to `entries` and `kept`, using an in-memory `fs.File`, `fs.DirEntry` and `fs.FileInfo` defined in the same file.
  - `latestVersion` counts the open version.
  - Add `func (v *migrationView) expectedChecksum(version uint64) (string, error)`, which applies `parseChecksumHeader` to the up file for that version. US4 uses it.
  - `ValidateMigrations` (`migrate.go`) also runs `readFragments`.

  T036 green.
- [X] T041 [US3] In `MigrateUp` (`internal/database/migrate.go`): when the view holds an open migration and the version after `m.Up()` equals it, log a warning `applied the open release migration NNN (unreleased)` (FR-018).
  - Done 2026-11-18. `MigrateUp` assembles the open migration before golang-migrate runs, so an invalid `next/` fails first. Two mutations, applied with `-overlay`, are caught: dropping the warning, and reversing the fragment order.
- [X] T042 [P] [US3] Create an empty `migrations/next/.gitkeep`. In the `Makefile`, add `migration-fragment`:
  - Done 2026-11-18. `TICKET` must be numeric as well, because the grammar requires it, and `set -C` refuses to overwrite a fragment created in the same second. Exercised in a scratch directory.
  - `TICKET` and `NAME` are required, and a `NAME` that does not match `[a-z0-9]+(-[a-z0-9]+)*` is refused;
  - `ts=$$(date -u +%Y%m%d%H%M%S)`;
  - it creates empty `migrations/next/$$ts-$(TICKET)-$(NAME).up.sql` and `.down.sql` and prints their paths;
  - add it to `.PHONY` and `help`.
- [X] T043 [US3] `cmd/lightstack-migrate/main.go` `handleStatus` prints the `Migration:` line of contracts/cli-and-startup.md (FR-022):
  - Done 2026-11-18. `Kind` is empty for a version the view does not hold, either pre-squash or newer, and status then omits the line. `TestMigrationStatusKind` covers none, baseline, release and open, and was RED first (compile). `TestStatusNamesTheMigration` covers the printed line; its RED was not observed on its own, because `TestMain` builds the binary outside `-overlay`'s reach.
  - `baseline (pre-squash migrations up to <SP>)`;
  - `release vX.Y.Z`, taken from the numbered file's name;
  - `open release migration NNN (unreleased)`;
  - `none (empty database)`.

  `GetMigrationStatus` fills the new `MigrationInfo.Kind` field from the view.
- [X] T044 [US3] Get US3 green: T036–T038 (`internal/database/migrate_fragments*_test.go`) pass on both backends, `TestSchemaParity` and `TestMigrationsRoundTrip` still pass, and `make lint` is clean.
  - Done 2026-11-18. `internal/database/...` and `cmd/...` are green on both backends, `TestSchemaParity` and `TestMigrationsRoundTrip` included, and lint reports 0 issues after one QF1001 fix in a test.

**Checkpoint**: schema changes merge in any order with no number to pick, and every database gets them in creation
order.

---

## Phase 6: User Story 4 — A database that applied an earlier state of the open release migration is refused (Priority: P2)

**Goal**: the stale-database guard compares the record of the database's current version with the
`-- checksum:` header the view serves for that version (FR-019–FR-022).

**Independent test**: `TestStaleOpenMigration` and the stale case of `TestStartupRefusals` pass on both backends
(quickstart §4, second half).

### Tests for User Story 4 (write first, confirm RED)

- [X] T045 [P] [US4] Add `TestStaleOpenMigration` to `internal/database/migrate_state_test.go`. It uses the backend `LIGHTSTACK_TEST_DB_URL` selects, and a temporary copy of `migrations/` with one fixture fragment, migrated to the open version 002.
  - Done 2026-11-18, RED first (compile). Each scenario runs on a fixture of its own (`openMigrationFixture`), and `freezeAs` freezes by hand as the release step will. The contract row "restamp: stale → refused" gained a `TestRestamp` subtest, and `TestMigrateFailure` gained a stale case.
  1. Unchanged: `MigrateUp` again succeeds, with no false positive.
  2. Edit the fragment by appending a statement. `MigrateUp`, `GetMigrationStatus` (state stale), `MigrateDown` and `MigrateToVersion` refuse with "applied an earlier state of the open release migration 002". `IntegrityCheck` on SQLite fails the same way.
  3. Add a second fragment: refused the same way.
  4. Remove the only fragment: the existing "newer Lightstack" refusal.
  5. Hand-write `002_v0.7.0.up.sql` and `.down.sql` from the assembled bytes, changing only the header's first line (same BODY, same checksum), and delete the fragment. Start-up proceeds (FR-021).
  6. As in 5, but from a BODY with one more fragment: refused with "has since been frozen as release v0.7.0 with different content".
  7. Delete the database's `applied_migrations` row: refused with "holds no record of what it applied".
  8. Mark the version dirty: not classified stale; golang-migrate's dirty refusal applies.
  9. `ForceMigrationVersion` stays allowed on a stale database.

  Fails now.
- [X] T046 [P] [US4] Add a stale case to `TestStartupRefusals` in `cmd/lightstack/startup_refusal_test.go`:
  - Done 2026-11-18 as `TestStartupRefusesAStaleDatabase`, beside the table, for the reason T025 gives. RED first: the server served until the 10 s timeout, and `up` exited 0. The test also covers `status` (`STALE`), `down` and `goto` refusing before the prompt, and `force` still prompting; those were added after the implementation, so their RED was not observed.
  - `setup` copies `../../migrations` to the working directory and adds one fragment, migrates a SQLite file with `lightstack-migrate up`, then edits the fragment.
  - Both entry points run with the migrations argument pointing at that copy.
  - Expect exit 1, and stderr containing "applied an earlier state of the open release migration".

  Fails now.

### Implementation for User Story 4

- [X] T047 [US4] Add the stale state to `internal/database/migrate_state.go` (FR-019, FR-020). For a clean database that has `applied_migrations`, at version V ≤ newest in the view:
  - Done 2026-11-18. A mismatch at the baseline (001) gets its own wording ("an earlier state of the baseline, migration 001"), because the open-migration wording would be wrong for it, so `StaleError` has a `Baseline` field. `inspectDatabase` now returns an `inspection`, whose `refusal()` gives the pre-squash or stale error; the view gained `upFile` and `staleness`.
  - Read the `applied_migrations` row for V and compare it with `view.expectedChecksum(V)`.
  - No row: stale, with the "holds no record" message.
  - A mismatch: stale. When V's up file is a numbered `NNN_v<release>` file, use the "since been frozen as release <release>" message; otherwise use the "open release migration" message.
  - A dirty version is never stale.
  - Report it as the error type `StaleError{Version uint64; Release string; NoRecord bool}`, whose `Error()` returns the matching contract text, with the predicate `IsStale(err error) bool` built on `errors.As`.
- [X] T048 [US4] Wire the stale state into `internal/database/migrate.go`, `internal/database/backup.go`, `cmd/lightstack-migrate/main.go` and `internal/database/dbtest/dbtest.go` (FR-020, FR-022):
  - Done 2026-11-18. `refuseBeforePrompt` takes the refusals a command accepts: `force` accepts `database.IsStale`. `Restamp` refuses a stale post-squash database instead of reporting it already post-squash.
  - `MigrateUp`, `MigrateDown`, `MigrateToVersion`, `GetMigrationStatus` (`migrate.go`) and `IntegrityCheck` (`backup.go`) refuse a stale database, while `ForceMigrationVersion` allows it.
  - In `cmd/lightstack-migrate/main.go`, `handleStatus` prints `Status: STALE`, a blank line and the message, then exits 1. `handleDown` and `handleGoto` refuse a stale database before they prompt.

  - In `internal/database/dbtest/dbtest.go`, extend T032's remedy line to `database.IsStale(err)`.

  T045 and T046 green.
- [X] T049 [US4] Get US4 green: T045 (`internal/database/migrate_state_test.go`) and T046 (`cmd/lightstack/startup_refusal_test.go`) on both backends, and `make lint`.
  - Done 2026-11-18. The full unit suite is green on SQLite. On PostgreSQL, `internal/glance` `TestImageMarkerPagingDoesNotDropTiedRows` failed once: its paging marker's image was gone, deleted by another package running in parallel on the shared database. It passes alone, 3 of 3 runs and the whole package, and it was green in the previous full run. Lint reports 0 issues, and the bash test passes 7/7.

**Checkpoint**: a database cannot quietly miss a fragment.

---

## Phase 7: User Story 5 — A release freezes its migration, and CI keeps it frozen (Priority: P2)

**Goal**:
- `lightstack-migrate freeze <release>` writes `NNN_<release>` with the open migration's exact body and empties `next/` (FR-023, FR-024).
- CI keeps numbered files immutable on `main` (FR-025) and rejects hand-numbered files (FR-026).
- A `v*` tag cannot carry fragments (FR-027).

**Independent test**: `TestFreeze` and `TestFreezeNeedsNoConfig` pass, and the CI steps behave as quickstart §5–§6
describes.

### Tests for User Story 5 (write first, confirm RED)

- [X] T050 [P] [US5] Write `TestFreeze` in `internal/database/migrate_freeze_test.go` (FR-021, FR-023, FR-024), over temporary copies of `migrations/`:
  - Done 2026-11-18, RED first (compile). Scenario 6 is one database migrated before a second fragment and one after it, then the freeze. The first carries on (FR-021); the other is refused as stale.
  1. With plain fragments only, `Freeze(dir, "v0.7.0")` writes `002_v0.7.0.up.sql` and `.down.sql`. Apart from the first header line, these are byte-equal to the `002_open` files the view served before the freeze. Every `next/*.sql` is removed, and `next/.gitkeep` stays.
  2. With one `.postgres` fragment, it writes `002_v0.7.0.postgres.*` and `002_v0.7.0.sqlite.*`.
  3. With no fragments, it writes no file and the result says "adds no migration".
  4. `v0.7` and `0.7.0` fail with "release name must be v<semver>".
  5. A release that already has a migration fails, naming the file.
  6. A database migrated with the open state starts after the freeze without being recreated (FR-021). One migrated with an earlier state (one fragment fewer) is refused (FR-020 scenario 5).
  7. `ValidateMigrations` passes on the result.

  Fails now.
- [X] T051 [P] [US5] Add `TestFreezeNeedsNoConfig` to `cmd/lightstack-migrate/main_test.go`. It builds or runs the binary with `freeze -migrations <tmp copy> v0.7.0` in a directory with no `config/lightstack.yaml`: exit 0. With no fragments, the output says "adds no migration". Fails now: the command does not exist.
  - Done 2026-11-18, RED first: the command loaded a configuration and exited 1.

### Implementation for User Story 5

- [X] T052 [US5] Implement `Freeze(migrationsPath, release string) (FreezeResult, error)` in `internal/database/migrate_fragments.go` (FR-023, FR-024; contracts/migrations-folder.md § Freeze):
  - Done 2026-11-18. It re-renders from the open migration's BODY (`openMigration` keeps `upBody` and `downBody`) rather than editing text. It writes files exclusively (`O_EXCL`, 0644), and checks for an existing migration of the release before looking for fragments, so freezing a release twice is an error, not a silent no-op.
  - Validate the release against `^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`, and refuse a release that a numbered file already names.
  - Call `assembleOpen` for both backends.
  - Write one plain pair when no fragment is backend-specific, and backend pairs otherwise.
  - Use the header `-- release migration NNN (<release>): frozen from migrations/next/ by lightstack-migrate freeze`, and the same BODY and checksum as the open migration.
  - Write files with mode `0644`, then delete `next/*.sql`, keeping dot-files.

  T050 green.
- [X] T053 [US5] Wire up the freeze in `cmd/lightstack-migrate/main.go` and `Makefile` (FR-023):
  - Done 2026-11-18.
  - In `cmd/lightstack-migrate/main.go`, dispatch `freeze` before `common.LoadConfig`, reading `-migrations` from the flags and the release from the first argument. Messages and exit codes follow the contract; add the usage line.
  - In the `Makefile`, add `migrations-freeze`: `RELEASE` is required, and it runs `go run ./cmd/lightstack-migrate freeze -migrations migrations $(RELEASE)`. Add it to `.PHONY` and `help`.

  T051 green.
- [X] T054 [US5] Add the step "Numbered migrations are immutable" to the `build` job in `.github/workflows/ci.yml`, after "Build binary" (FR-025, research R12):
  - Done 2026-11-18. The check is `scripts/check-migrations-immutable.sh <base>`, which the CI step calls, rather than inline YAML, so it is testable: `test/migrations_immutable_test.sh` covers an edited baseline, a renamed and a deleted release migration, a fragment, a new release migration and the squash's move, 6/6, RED first. A new build step runs both bash tests (this one and `upgrade_squash_guard_test.sh`); they need no root.
  - On `pull_request`, run `git fetch --depth=1 origin "$GITHUB_BASE_REF"` and use `base=FETCH_HEAD`.
  - On `push`, run `git fetch --depth=1 origin "${{ github.event.before }}"` and use that commit as `base`. Skip the step when it is all zeros.
  - Compute `git diff --no-renames --name-status --diff-filter=DM "$base" HEAD -- 'migrations/001_baseline.*' 'migrations/[0-9][0-9][0-9]_v[0-9]*'`. With `--no-renames`, a rename shows as a deletion.
  - The second pattern needs a digit after `_v`. It matches every release migration (`002_v0.7.0…`), but not the pre-squash `044_volume_groups` and `073_volume_transfer_awaiting`, which the squash change moves. The squash change therefore passes without a special case.
  - For each path, print `::error file=<path>::numbered migrations are immutable once on main; put the change in a fragment (migrations/next/)` and exit 1.
- [X] T055 [P] [US5] Add a job `release-fragments` to `.github/workflows/security.yml`, with `if: startsWith(github.ref, 'refs/tags/v')` (the workflow's `on:` already includes `tags: ['v*']`) (FR-027):
  - Done 2026-11-18. The checkout action is pinned by SHA, as the workflow does. The step was dry-run with fragments (exit 1, annotated), with none, and with no `next/` (exit 0).
  - Check out the code and run `files=$(find migrations/next -name '*.sql' | sort)`.
  - If that is non-empty, print `::error::${GITHUB_REF_NAME} carries unfrozen fragments in migrations/next/: <files>. Run make migrations-freeze RELEASE=${GITHUB_REF_NAME} on main, then delete and re-cut the tag.` and exit 1.
- [X] T056 [US5] Get US5 green: T050 and T051 pass, `TestMigrationsFolder` passes, and `make lint` is clean. Dry-run T054's diff locally against `origin/main` after editing one line of `migrations/001_baseline.postgres.up.sql`: it must print the error. Then revert the edit.
  - Done 2026-11-18. The freeze and folder tests pass on both backends, both bash tests pass (6/6, 7/7), and lint reports 0 issues. The planned manual dry-run against `origin/main` could not have triggered: the baseline does not exist on `main` yet, so an edit shows as an addition. The bash test replaces it with a repository where the baseline and a release migration are already committed. On this branch, the diff from `origin/main` to the index is empty for the patterns, so the squash change passes its own check.

**Checkpoint**: one migration per release, frozen once on `main`, never shipped open.

---

## Phase 8: Polish & Cross-Cutting Concerns

- [X] T057 [P] Rewrite `migrations/README.md` (FR-028, research R16):
  - Done 2026-11-18. It also explains the one file shape (header, checksum, body, record) and where each check lives, and tells a branch rebased onto later fragments it depends on to rename its own with a fresh time.
  - the folder grammar;
  - the baseline: generated by the two `-update` tests from `internal/database/testdata/pre-squash/`, and frozen;
  - fragments: `make migration-fragment`, naming, creation-time order, and plain versus backend pairs. Keep the existing guidance on "Write one plain file pair only for…", the `.postgres`/`.sqlite` cases, and "Never rebuild a table other tables reference in a .sqlite file", now applied to fragments;
  - every fragment of a release running in one transaction (R17);
  - the open release migration and the stale-database guard (recreate);
  - the freeze (`make migrations-freeze`);
  - immutability and the three CI checks;
  - re-stamp, for PostgreSQL only.

  Drop rule 3 and the `073` baseline section. Rewrite the golang-migrate CLI note to the new reason: fragments and per-backend files.
  - Superseded in part by F11: the README no longer mentions re-stamp.
- [X] T058 [P] Update `CLAUDE.md`:
  - Done 2026-11-18. The baseline has 75 tables on each backend, `applied_migrations` among them.
  - Rewrite the migrations paragraph under "Database Schema": baseline `001` on both backends, fragments in `migrations/next/`, one migration per release, frozen numbered files, `applied_migrations`, and `restamp` for PostgreSQL only.
  - Correct the table count from the baseline: count the `CREATE TABLE` statements in `migrations/001_baseline.postgres.up.sql`.
  - Add `make migration-fragment` and `make migrations-freeze` to "Database Operations".
- [X] T059 [P] Append a dated "Amendment (SPEC-032)" note to `docs/adr/ADR-003-pluggable-database-backend.md`: the squash restarted both backends at `001`; rule 3 is retired; fragments assemble into the open release migration; the decision (one folder, per-backend views) is unchanged. Leave the original text as it is.
  - Done 2026-11-18.
- [X] T060 [P] Update the living documents under `docs/` and `internal/database/doc.go` (FR-028):
  - Done 2026-11-18. A delegated sweep handled the eight documents, with every fact checked against both baselines; `internal/database/doc.go` was done separately and now also describes the baseline, the fragments and the refusals. Two pre-existing errors were left alone: `docs/VXLAN_IMPLEMENTATION.md` names a `--migrate` flag that does not exist, and `docs/STUB_PLACEHOLDERS.md` credits 053 with a table 006 created.
  - Files: `docs/ARCHITECTURE.md`, `docs/blueprint.md`, `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`, `docs/compatibility/TERRAFORM_COMPATIBILITY_REPORT.md`, `docs/EBPF_STATUS.md`, `docs/L3_ROUTER_IMPLEMENTATION.md`, `docs/STUB_PLACEHOLDERS.md`, `docs/VXLAN_IMPLEMENTATION.md` and `internal/database/doc.go`.
  - Find the references with `grep -nE '(migration|Migration)s? \`?0[0-9]{2}|[0-9]{3}_[a-z_]+\.(postgres\.|sqlite\.)?(up|down)\.sql|\b073\b'`.
  - Replace each reference that sends a reader to a pre-squash migration file as the place something is defined with the schema fact, or with "the baseline (`001`)".
  - Leave `CHANGELOG.md` history, `docs/archive/`, `docs/specs/` (other features), `docs/review/` and past `docs/UPGRADING.md` sections untouched.
- [X] T061 [P] Rewrite the code comments that cite a migration number (FR-028). These are the 19 production comments: `internal/common/roles.go:10`, `internal/keystone/groups.go:93,114`, `internal/cinder/quotas.go:93`, `internal/keystone/auth.go:1074,1131,1151`, `internal/neutron/security_group_default.go:22`, `internal/database/backup.go:173`, `internal/neutron/router.go:1007`, `internal/nova/quotas.go:248`, `internal/glance/images.go:1024`, `internal/keystone/handlers.go:688`, `internal/neutron/reconciler.go:522`, `internal/nova/server_filter.go:253`, `internal/nova/state_reconciler.go:272` and `internal/neutron/network.go:478,1251,1762`. Also cover the test comments the same grep finds without the `_test.go` filter, for example `internal/cinder/volumes_db_test.go:63` and `test/contract/nova/security_group_actions_test.go:118`.
  - Done 2026-11-18, comments only: 34 Go files, checked mechanically. Three findings: "migration 075" meant 077 (renumbered in a merge); `network.go` called `networks.project_id` nullable, which it has not been since pre-squash 080; and a caveat in `server_detail_secgroups_test.go` no longer applied. The line references in `network.go` became function names. String literals that cite numbers (test messages) were left as they are.
  - State the schema fact instead.
  - Where the comment records history, keep it and mark it "(pre-squash migration NNN)". Superseded by F10: no comment outside `internal/database` cites a pre-squash migration any more.
  - Change comments only, no code.
- [X] T062 [P] Add a "⚠️ Breaking — database migrations squashed" entry to `CHANGELOG.md` under `[Unreleased]`. It says that migrations restart at `001`, and that pre-squash databases are recreated, or re-stamped with `lightstack-migrate restamp` if they are PostgreSQL databases at the squash point. It also covers one migration per release from fragments and the SQLite `project_id` NOT NULL change, and links `docs/UPGRADING.md#the-migration-squash`.
  - Done 2026-11-18. The entry also names the status and upgrade-script changes, including the migration step's fix. It states the SQLite `project_id` result without the unreleased triggers it replaced.
- [X] T063 Run the final gates (FR-029, SC-007), then walk `docs/specs/032-migration-squash-per-release/quickstart.md`:
  - Done 2026-11-18. Gates: gofmt is clean, lint reports 0 issues, and `go test -race` is green on both backends (on a fresh scratch PostgreSQL database rather than `make db-down && make db-up`, which would wipe the container other worktrees share). The contract suites ran as the SQLite CI job runs them, with the built binary, on both backends: PostgreSQL 326 passed, 0 failed, 11 skipped; SQLite 325 passed, 0 failed, 12 skipped. `quick_test.sh` passed 17/17 on each. The Compose stack was not used, because its PostgreSQL binds 5432, which the shared container holds. Both bash tests and `test/migration_test.sh` (15/15) pass. Quickstart: §1 and §7 via the suites; §2–§5 with the real binary on scratch databases and a scratch copy of the folder, all as described, except that §5's second freeze refuses (quickstart and contract updated, see T052); §6 via `TestMigrationsFolder`, `TestValidateMigrations` and `test/migrations_immutable_test.sh`. The pre-squash database of §3 was replayed with psql from `testdata/pre-squash` rather than a worktree of a squash commit that does not exist yet.
  - `make fmt` and `make lint`;
  - `go test -race ./...` on PostgreSQL (after `make db-down && make db-up`) and on SQLite;
  - both contract suites, as CI runs them (`contract-tests`, `contract-tests-sqlite`);
  - `test/quick_test.sh` against `docker compose -f deployments/docker-compose.yml up -d`;
  - the script tests that CI's `build` job runs: `bash test/migrations_immutable_test.sh` and `bash test/deploy_single_node_test.sh`.

  Then walk `quickstart.md` §1–§7 and record the outcome of each step.
- [ ] T064 Last step before merge: confirm `main` has gained no numbered migration since T002. If it has, return to T002. Re-run `go test -count=1 ./internal/database -run 'TestGenerate(Postgres|SQLite)Baseline' -update` and confirm `git diff --exit-code migrations/` (FR-002).
  - Open: this is the step before merge, and its precondition, a rebase onto `main` after 076–082 land there, is not met. The local `origin/main` stops at 075, and fetching failed in the agent shell, which has no credentials. The baseline regeneration was checked anyway on 2026-11-18: both generators ran and every baseline file is byte-identical (sha256).
  - 2026-11-26, on the rebase onto `main` `7202a5f`: `main` had gained `083`, so T002 was redone. Both generators ran with `-update`; the baselines gained `token_revocations` and its index, and a second run changed nothing. `testdata/pre-squash/` is blob-identical to `main`'s `migrations/` (184 files). Repeat if `main` gains a numbered migration before the merge.


### Follow-up: pre-existing problems (2026-11-18)

Fixed after implementation, at the user's request. Every code change went test-first.

- [X] F1 `scripts/deploy-single-node.sh`:
  - Its migration step called the nonexistent `lightstack migrate`, which started the server in the foreground. It now runs `bin/lightstack-migrate up`.
  - Its build step masked a failed build, because a filter's exit status decided, and it assumed `make`. It now installs `make` when missing, then checks the build's own status and both binaries.
  - `LIGHTSTACK_DIR` replaces 16 hard-coded paths, and sourcing the script no longer runs `main`.
  - `test/deploy_single_node_test.sh` (4 cases, RED first) runs in CI.
- [X] F2 `make build` builds `bin/lightstack-migrate` beside the server. The upgrade script relies on it and checks both binaries.
- [X] F3 Documentation:
  - `docs/SCALING.md`, `docs/UNIFIED_DEPLOYMENT.md` and `docs/VXLAN_IMPLEMENTATION.md` called the nonexistent `lightstack migrate` or `--migrate`; they now call `lightstack-migrate up`.
  - `docs/STUB_PLACEHOLDERS.md` now credits pre-squash 006 with the table (053's re-declaration did nothing) and shows the baseline's schema.
- [X] F4 `lightstack-migrate status` on a SQLite file that does not exist yet:
  - It is still an error and still creates nothing, as SPEC-028's must-exist contract requires.
  - It now reports `Status: NO DATABASE` (`MissingDatabaseError`, `IsMissingDatabase`), and the upgrade guard carries on for it, so UPGRADING no longer asks for `lightstack-migrate up` first.
  - Tests, RED first: `TestInspectCommandsDoNotCreateADatabase`, `TestStatusOnAMissingSQLiteFile`, and guard case 2b.
- [X] F5 Flaky paging tests:
  - `TestImageMarkerPagingDoesNotDropTiedRows` and its Cinder twin walked every row the shared seeded project could see, so another package's delete could remove the marker mid-walk.
  - They now see only their own rows, through a run-unique name and a project of the test's own, and fail on any other row.
  - Mutating either keyset back to `created_at <` still fails both.
- [X] F6 Eight test failure messages that named pre-squash migration numbers now name the baseline or the column.
- [X] F7 A second flake of the same family surfaced during the gates: neutron's `TestListingMarkersResolveOnlyWhatTheCallerMayRead` got a 500 from the seeded project's port listing.
  - The cause: fixtures in other packages inserted ports into the seeded project on a network but without a `name`, which product code always writes. The listing reads `name` and `mac_address` as plain strings, and one failed scan aborts the whole result. A port without a network never reaches it, because the listing joins ports to networks.
  - Fixed in the fixtures. Two could reach that listing: `nova/addresses_test.go` and `nova/selfheal_test.go`.
  - Four more got the same fix for consistency only, because their ports could not reach it. `metadata/identity_test.go` (now a `seedGuestPort` helper with a network of its own), `nova/server_list_filters_test.go` and `neutron/update_statement_test.go` inserted ports without a network. `nova/authz_test.go` uses a project of its own on an unshared network.
  - Proof: a full parallel suite on a scratch database with an audit trigger on `ports` recorded 7 malformed writes before the fix and none after.
- [X] F8 `test/migration_test.sh` is removed, after T021 had adapted it.
  - Nothing ran it: no CI job, Makefile target or script calls it. Only this spec mentioned it.
  - It called `./lightstack-migrate` in the current directory, but `make build` writes `bin/lightstack-migrate`. After a normal build, 10 of its 15 checks failed.
  - `TestValidateMigrations` and `TestMigrationsFolder`, which CI runs, already check the folder, the up/down pairs and the names. Of its other checks, five only searched the usage text for words such as `down`, and one rejected any `*test*.sql` file, fragments included.
  - Its status checks against a migrated database have no automated replacement. The host runbook checks the status output by hand (§2.2, §2.3, §2.5).
- [X] F9 `scripts/upgrade-lightstack.sh` is deleted, and `test/upgrade_squash_guard_test.sh` with it. This supersedes T026, T033 and the guard half of T034.
  - The reasons are in the spec (Clarifications, Session 2026-11-18, and FR-013) and in research R13. The script served only bare-metal checkout installs. It could not finish an upgrade, because its migration step started the server in the foreground. Its checkout stashed the node's configuration and never put it back, and its backup held no database.
  - FR-013, SC-003, the Edge Cases and Out of Scope are amended. `docs/UPGRADING.md` gives the bare-metal steps by hand, and the CI step no longer runs the guard test.
  - Also updated: `README.md`, `scripts/README.md`, `CHANGELOG.md`, `CLAUDE.md`, and five code comments that cited the script.
- [X] F10 No code comment outside `internal/database` cites a pre-squash migration number any more. T061 had kept 23 such lines, in 20 comments across 16 files, as history marked "(pre-squash migration NNN)".
  - The numbered files survive only as generator input in `internal/database/testdata/pre-squash/`, so the numbers led nowhere a reader could follow.
  - A comment that only dated a fact now states the fact. A comment whose history explains the code says what changed, without the number. Examples: the column "was then a VARCHAR with no foreign key"; the cinder `volume` alias "was once seeded with" a localhost URL; gateways "stored before networks had the `external` column".
  - `internal/database`, and the status texts and tests that describe the squash itself, keep their references. Comments only; gofmt, `go vet` and `make lint` are clean.
  - Docs that describe the system as it is carry no squash history either. `docs/VXLAN_IMPLEMENTATION.md` names neither the baseline nor a pre-squash migration. `docs/ARCHITECTURE.md`'s migrations paragraph now describes the current layout: the baseline, one migration per release, fragments, and checksums. Before, it described where the baseline came from. `docs/STUB_PLACEHOLDERS.md`, `docs/blueprint.md` and the Horizon and Terraform compatibility reports state their facts without migration numbers. The Horizon report's causes now give the order of the seeds without numbering them.
  - Dated documents keep their numbers, because they record what was true on their date: the reports, plans and reviews under `docs/testing/`, `docs/superpowers/` and `docs/review/`.
- [X] F11 Re-stamp is deleted, at the user's request (spec Clarifications, Session 2026-11-19; research R10). v0.6.1 shipped migrations only up to `059`, so no release database can be at `082`, and nothing deployed has to survive: v0.7.0 is a fresh install. This supersedes T024, T030 and T031, and parts of T005, T007, T008, T027, T034 and T057.
  - Code: `restamp.go`, `restamp_test.go` and the `restamp` subcommand are gone. The PostgreSQL introspection is test code again (`introspect_postgres_test.go`), and so is `appliedMigrationsDDL` (`baseline_gen_test.go`). Only `pgQuerier` and `sortedKeys` stay in production, where the state check and the fragments use them.
  - `SquashPoint` is gone from production code; the generators keep their own `squashPoint`. The status line for the baseline reads `baseline`. The pre-squash refusal reads `… from before the migration squash. Recreate the database. See docs/UPGRADING.md, "The migration squash"` on both backends, and `PreSquashError` lost its `Backend` field.
  - Kept on purpose: the pre-squash refusal itself. Without it, an old database at `082` would read as "written by a newer Lightstack", and once a few releases exist, an old database at a low version would pass as current.
  - `docs/UPGRADING.md` has one section per release, newest first. The v0.7.0 section says to install fresh: on a real-mode node, run the earlier build's `cleanup` while its database still exists, then empty the database and start v0.7.0. It does not mention the product's earlier name, at the user's request. The other sections are gone; `CHANGELOG.md` and git history keep them.
  - Pointers into the removed sections are fixed: two comments in `internal/nova/console.go`, the error text in `pkg/hypervisor/console_endpoint.go`, `.env.example`, `docs/INDEX.md` and `charts/lightstack/README.md`. `docs/CONFIGURATION.md`'s note on the renamed variables, which linked there, is gone. `CLAUDE.md`, `migrations/README.md` and `CHANGELOG.md` no longer mention re-stamp.
  - The host runbook builds no pre-squash commit any more. §1 writes a `schema_migrations` row at 59 into `old`, and §2.4 checks the refusal against it. The re-stamp steps are gone, and with them the OpenStack CLI, `tar` and the need for the full git history.
  - Gates: build, `go vet` and `make lint` are clean. `internal/database`, `dbtest`, `cmd/lightstack` and `cmd/lightstack-migrate` pass on PostgreSQL and on SQLite. The host runbook ran end to end on 2026-11-19, every block matching its Expect line, and teardown left the host clean.
- [ ] F12 After T064, delete the pre-squash history: `internal/database/testdata/pre-squash/` (180 files), both baseline generators and the equivalence tests. T064 needs them once more, after the rebase onto `main`. After that, both sides are frozen and the proof is done. The pre-squash refusal tests then stand in an old database with a `schema_migrations` row, as the host runbook does.
---

## Dependencies & Execution Order

### Phase dependencies

- **Setup (T001–T003)**: none. T002 waits for the in-flight numbered migrations to merge into `main`, and T003 waits for the user's approval of the test strategy.
- **Foundational (T004–T009)**: needs Setup and blocks every story.
- **US1 (T010–T022)**: needs Foundational. It produces the baselines and `applied_migrations`, which every other story builds on.
- **US2 (T023–T035)**: needs US1 (the baseline, `applied_migrations` and `SquashPoint`).
- **US3 (T036–T044)**: needs US1 (the post-squash folder and the record helpers). It does not need US2, but shares `migrate.go`, `migrate_view.go` and `cmd/lightstack-migrate/main.go` with it, so run it after US2, or merge carefully.
- **US4 (T045–T049)**: needs US2 (the classification) and US3 (the open migration and `expectedChecksum`).
- **US5 (T050–T056)**: needs US3 (`assembleOpen`). Its FR-021 test needs US4's guard.
- **Polish (T057–T064)**: needs every story. T064 is the very last task.

### Within each story

RED tests first (confirm they fail), then implementation in the listed order, then the green task at the end of
the phase. Never edit a generated baseline by hand; change the generator and regenerate (T017).

### Story completion order

```text
Setup → Foundational → US1 (MVP) → US2 ─┐
                             └→ US3 ────┼→ US4 → US5 → Polish
```

## Parallel opportunities

- **Foundational**: T004, T005 and T009 together. T006 after T004; T007, then T008.
- **US1 tests**: T010, T011, T012 and T013 together, since each is a different file. T021 alongside T014–T020.
- **US2 tests**: T023, T024, T025 and T026 together. Implementation: T032, T033 and T034 alongside T027–T031.
- **US3 tests**: T036, T037 and T038 together. T042 alongside T039–T041.
- **US4 tests**: T045 and T046 together.
- **US5 tests**: T050 and T051 together. T055 alongside T052–T054.
- **Polish**: T057–T062 together.

Example (US2 tests):

```text
T023 internal/database/migrate_state_test.go     TestDatabaseState
T024 internal/database/restamp_test.go           TestRestamp
T025 cmd/lightstack/startup_refusal_test.go      pre-squash startup case
T026 test/upgrade_squash_guard_test.sh           upgrade-script guards
```

## Implementation strategy

1. **MVP is US1 plus US2, both P1.** US1 alone leaves every existing database on golang-migrate's raw error, so
   merge it only together with US2. After US2's checkpoint the branch is a complete "squash" (the ticket's
   phase A).
2. **The per-release half (US3 → US4 → US5)** can follow in the same PR, or in a second one cut from the US2
   checkpoint. Until it lands, CI already refuses hand-numbered files (T011, T018), so nobody can reintroduce
   per-feature numbers between the halves. Schema changes made in that window must wait for US3.
3. **Regenerate late.** The baselines depend on `main`'s newest numbered migration. T064 confirms nothing moved
   before merge.

## Notes

- `[P]` marks a different file with no dependency on an incomplete task.
- The test-only helpers (`migrateFolderNoChecks`, `dropBelow`) are the only code that reads
  `testdata/pre-squash`. The generators and the equivalence check are its sole consumers.
- No chart, `values.*`, Compose file or Dockerfile changes (plan § Structure Decision).
- Commit after each green task or logical group. The user commits.
