# Implementation Plan: SQLite backend, part 1 — database layer and migrations

**Branch**: `feature/598` (feature id: `028-sqlite-database-layer`) | **Date**: 2026-10-12 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `docs/specs/028-sqlite-database-layer/spec.md` (cloud-storage#598, part 1 of #595)

## Summary

Lightstack gains a second database backend, chosen once at startup from the URL scheme: `postgres`/`postgresql` keep today's pgx pool unchanged; `sqlite:///<absolute path>` opens one SQLite file through the pure-Go `modernc.org/sqlite` driver (v1.60.1, SQLite 3.53.4).

**The database layer.** Every component moves from pgx types to a backend-neutral contract in `internal/database` whose method sets mirror pgx's. The 736 call sites keep their calls; only types change (R1).
- **PostgreSQL**: passes statements through untouched (R2).
- **SQLite**: translates PostgreSQL syntax on the way in — casts, `= ANY($n)` list expansion, `FOR UPDATE`, the console expiry arithmetic, `~` → `REGEXP`, `LIMIT`/`OFFSET` placeholders (R5). It registers `now()`, `gen_random_uuid()`, an add-seconds function, a `regexp` function and no-op advisory locks (R6), and converts timestamps, JSON, lists and `[]byte` at the boundary (R7).
- **Rewrites**: ten rewrite sites without a faithful translation are rewritten once for both backends (R8); the `name` filter stays a database match, as upstream's (clarification 2026-10-12). A static check keeps new PostgreSQL-only SQL out (R9).

**Migrations.** Both backends migrate from the one `migrations/` folder through a per-backend `fs.FS` view read by golang-migrate's `iofs` source (R10). SQLite starts from a generated baseline at 073 (R11), checked against PostgreSQL by a parity test, and migrates under a cross-process file lock (R12).

**Transaction boundaries.** Two transaction-boundary defects are fixed on both backends:
- **Cinder**: volume create, boot from a new volume and snapshot create reserve a `creating` row, build the backing outside any transaction, then finalise. A startup sweep resets each node's own interrupted reservations (R13).
- **Nova**: its quota check gains a binding recheck inside the instance insert, under a per-project advisory lock. A refused create now answers `403` with upstream's message instead of `413` — FR-035a (R14). With FR-036a's `400` for requests against a resource still `creating`, these are the feature's two API changes, both towards upstream.

**Startup.** Startup refuses bad URLs, backend mismatches and SQLite with multi-node, before touching the disk (R15). The database file is 0600, commits are fully synchronous, and one log line says which database and which settings are in effect.

**Tests and CI.** The eight database-backed packages run on both backends. A new CI job runs the contract and quick suites against a SQLite-backed binary (R16).

## Technical Context

**Language/Version**: Go 1.27.3 (`go.mod`); `modernc.org/sqlite` v1.60.1 requires ≥ 1.26.
**Primary Dependencies**:
- **Existing**: gin, `jackc/pgx/v5` + pgxpool (PostgreSQL implementation only), `golang-migrate/migrate/v4` v4.19.1 — whose `database/sqlite` and `source/iofs` packages are newly imported from the already-required module.
- **New module**: `modernc.org/sqlite` v1.60.1, bringing `modernc.org/libc`, `mathutil`, `memory`, `ncruces/go-strftime`, `remyoudompheng/bigfft` and `dustin/go-humanize` (and `modernc.org/fileutil` for the driver's tests only); `mattn/go-isatty` moves 0.0.20 → 0.0.24. Still `CGO_ENABLED=0`.

**Storage**:
- **PostgreSQL**: unchanged.
- **SQLite**: one file, recommended at `<state_path>/db/lightstack.db`, in WAL mode with `-wal`/`-shm` beside it.
- **One new migration on both backends**: `074_reservation_node` (`volumes.reserved_by`, `snapshots.reserved_by`, nullable `TEXT`).
- **SQLite baseline**: `073_baseline.sqlite.up.sql`/`.down.sql` (generated).
- **Existing migrations**: untouched, except the corrected down files of 010/019/028/037 (FR-025).

**Testing**:
- `go test` with `LIGHTSTACK_TEST_DB_URL` — a `postgres://` URL, a `sqlite:///` URL, or unset to skip — always with `-count=1`;
- the new parity test, static construct check and concurrency tests;
- the gophercloud contract suite (`test/contract`) and `test/quick_test.sh` against both backends;
- `golangci-lint` via `make lint`.

**Target Platform**: Linux in stub and real mode; macOS in stub mode. SQLite needs a local filesystem with POSIX locks (#595).
**Project Type**: single-binary Go web service (`cmd/lightstack`) plus the `lightstack-migrate` CLI.
**Performance Goals**:
- PostgreSQL: unchanged;
- SQLite: another project's write completes in < 1 s during a ≥ 10 s volume copy (SC-006);
- refusals within 1 s (SC-003).

**Constraints**:
- PostgreSQL receives every statement unchanged (FR-003);
- no API change other than FR-035a and FR-036a;
- existing migration files untouched, except four corrected down files (FR-025);
- no back-compat aliases for removed names;
- configuration gains environment variables only (`LIGHTSTACK_DB_BACKEND`, `LIGHTSTACK_NODE_NAME`), no YAML keys;
- the spec and this plan cover this branch's code only.

**Scale/Scope**: 736 SQL calls in 67 files; 165 `pgx.ErrNoRows`; 61 pool-type references in 28 files; 29 transaction-type references in 10 files; 52 `RowsAffected`; 13 transactions; 10 rewrite sites; 8 database-backed test packages; 10 test files that open a pool, plus the benchmark; 6 contract tests that seed through `psql`; 73 existing migrations; 151 indexes, 63 random-id and 123 timestamp defaults to mirror.

No `NEEDS CLARIFICATION` remains: research.md R1–R22 settle every open point, and the three planning decisions that changed the spec (`202`, `403`, index count) are recorded there (R21) and in the spec.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design (below the table).*

| Principle (`.specify/memory/constitution.md` v1.3.0) | Verdict | Why |
|---|---|---|
| I. OpenStack Flamingo API compatibility | PASS | Two responses change, both **towards** upstream: Nova's quota refusal becomes upstream's `403` with upstream's message (FR-035a, R14), and requests against a volume or snapshot still `creating` get upstream's `400` (FR-036a, R13). The `name` filter keeps upstream's database match (`~` on PostgreSQL, `REGEXP` on SQLite). The contract suite gates both backends (SC-001). |
| II. Synchronous operations | PASS (v1.3.0 exception: reservation around external work) | Every create still completes before it returns. The reservation meets the four limits: checks and reservation in the first transaction, no transaction across the storage call, finalised or removed before the response, and a one-shot startup correction by the node that made it (R13). |
| III. Fail-fast | PASS (v1.3.0 exception: bounded lock waits) | Refusals happen at startup in < 1 s (SC-003). SQLite's busy wait is 5 s, the article's maximum (R3), and the application never retries: immediate transactions prevent `SQLITE_BUSY_SNAPSHOT` (R4). |
| IV. Multi-mode | PASS | Both backends work in stub and real mode. `flock` and the driver work on macOS. Real-mode host validation on SQLite is a merge gate (quality gates). |
| V. Project isolation | PASS | Queries keep their project predicates. The regex filters keep the scope's candidates (R8), and the ports join keeps its project pairing. |
| VI. Single binary, shared pool | PASS (v1.3.0) | One small binary, the driver included and one shared pool per process — PostgreSQL, or one embedded SQLite file chosen by the URL. SQLite is refused in multi-node mode, as the article requires (FR-005). |
| Technology constraints | PASS (v1.3.0) | "PostgreSQL 14+, or SQLite embedded through `modernc.org/sqlite`" and Go 1.26+ are now the stated constraints. The new dependency is one pure-Go module tree. |
| Forbidden patterns | PASS | No queue, no worker, no state machine, no service discovery. |
| Test-first | PASS by plan | RED first: the 413 → 403 test, the two-creates race (both currently succeed), the SQLite settings read-back, the scheme refusals, the parity test, and the SC-006 blocking test, which fails on SQLite until R13. |
| Backwards compatibility | PASS | Migrations stay reversible (074 has a down; the baseline has a down). Removed names have no aliases. `docs/UPGRADING.md` covers `make migrate`, golang-migrate's `migrate` CLI and the 403 change. |
| Security | PASS | The SQLite file holds recoverable EC2 secrets and console tokens, so it is 0600, enforced (FR-011a). Startup logs never print credentials. |
| Simplicity / anti-abstraction (CLAUDE.md Articles VII and VIII; ADR-001) | **DEVIATION — justified** | ADR-001 rejected interface injection as premature. A second backend is the case it deferred to, so ADR-003 records the reversal. See Complexity Tracking. |
| Quality gates before merge | PASS by plan | CLI (`quick_test.sh`) and contract suite on both backends; Horizon probe on both backends (FR-035a changes the quota error Horizon shows); stub and real mode (SQLite real-mode host run); migrations up and down on both backends; no secrets. |

**Post-design re-check (after Phase 1)**: unchanged. The design adds no queue or background loop beyond the one-shot startup sweep, and no API change beyond FR-035a. The one remaining deviation is the abstraction rule (CLAUDE.md Article VIII, ADR-001), justified in Complexity Tracking; Articles II, III and VI are met through constitution v1.3.0 (amended 2026-10-12).

## Project Structure

### Documentation (this feature)

```text
docs/specs/028-sqlite-database-layer/
├── spec.md
├── plan.md                 # this file
├── research.md             # Phase 0: R1–R22
├── data-model.md           # Phase 1: migration 074, reservation lifecycle, value mapping, files
├── quickstart.md           # Phase 1: run and verify on both backends
├── HOST_TESTING.md         # real-mode host runbook, both backends
├── baseline-gates.md       # recorded baseline gate results (delivery step 1)
├── contracts/
│   ├── configuration-and-startup.md   # URLs, refusals, exit codes, env vars, log line
│   ├── go-database-contract.md        # internal/database API and call-site changes
│   ├── sql-dialect.md                 # supported constructs, translation, static check
│   ├── migrations.md                  # folder rules, baseline, CLI, lock, conventions
│   ├── block-storage-reservation.md   # volume/snapshot create, refusals, sweep
│   └── compute-quota.md               # check and recheck, 403
├── checklists/requirements.md
└── tasks.md                # Phase 2 (/cl.tasks — not created here)
```

### Source Code (repository root)

```text
internal/database/
├── db.go                 # Backend, DB/Querier/Tx/Rows/Row/Result, ErrNoRows, Options, Open, HealthCheck
├── url.go                # ParseURL, CheckDeclaredBackend, RedactURL
├── describe.go           # Describe
├── errors.go             # IsUniqueViolation, IsForeignKeyViolation, IsNotNullViolation, IsCheckViolation, IsBusy
├── tx.go                 # WithTx(ctx, db DB, fn func(Tx) error)
├── postgres.go           # pgxpool wrappers (only file importing pgx outside tests)
├── sqlite.go             # open sequence (dir 0750, file 0600, must-exist, read-back), sql.DB wrappers
├── sqlite_translate.go   # lexer + rules + bounded cache
├── sqlite_values.go      # bind/scan conversions
├── sqlite_funcs.go       # now, gen_random_uuid, ls_add_seconds, regexp, pg_advisory_xact_lock, hashtext
├── migrate.go            # backend-aware migrator, view, baseline refusal, ValidateMigrations
├── migrate_view.go       # fs.ReadDirFS view (three rules)
├── migrate_lock.go       # flock-based SQLite migration lock (unix)
├── sqlcheck_test.go      # static construct check (FR-018, SC-009)
├── doc.go                # package documentation: the supported construct set (FR-018)
├── baseline_gen_test.go  # SQLite baseline generator (-update)
├── parity_test.go        # schema parity test (FR-028)
└── dbtest/dbtest.go      # shared TestMain helper; per-package SQLite file; RequirePostgres
internal/common/
├── pgerror.go            # deleted (moves to database.IsUniqueViolation)
├── nodename.go           # LIGHTSTACK_NODE_NAME → else os.Hostname()
└── project.go, quota helpers   # *pgxpool.Pool → database.DB
internal/{keystone,nova,neutron,cinder,glance,metadata,console,compute,health,teardown}/  # type conversion
internal/cinder/volumes.go      # reservation flow, snapshot flow, refusals while creating, sweep
internal/cinder/quotas.go       # unchanged locking; Tx types
internal/nova/handlers.go, quotas.go   # binding recheck + advisory lock; 403 with upstream message
internal/nova/{tenant_usage,aggregates,server_filter}.go, internal/neutron/{vxlan_coordinator,reconciler,network,router}.go,
internal/metadata/service.go     # R8 rewrites; 7 CURRENT_TIMESTAMP → NOW()
cmd/lightstack/main.go          # URL/backend/vxlan checks, node name, Open, log line, sweep, cleanup/console-tokens exit codes
cmd/lightstack-migrate/main.go  # backend-aware commands
pkg/paths/paths.go              # DBDir()
migrations/
├── 073_baseline.sqlite.up.sql / .down.sql   # generated
├── 074_reservation_node.up.sql / .down.sql  # plain, both backends
└── README.md                                # three rules + conventions (R20)
test/contract/helpers.go        # Seed(t, sql, args...) through database.Open; 3 test files stop shelling out to psql
deployments/docker-compose-kvm.yml  # whole state root at the same path; LIGHTSTACK_STATE_PATH / LIGHTSTACK_CONFIG_DIR; LIGHTSTACK_DB_URL passed through (FR-042)
test/compose_kvm_state_test.sh  # render test for it (make test-compose-kvm)
charts/lightstack/templates/_helpers.tpl, teardown-purge-job.yaml  # LIGHTSTACK_NODE_NAME from spec.nodeName (FR-043); purge Job gets LIGHTSTACK_DB_URL from database.existingSecret (FR-044)
charts/lightstack/tests/{node_name,teardown,extra_env,tls}_test.yaml  # helm-unittest cases
.github/workflows/ci.yml        # SQLite unit-test step; contract-tests-sqlite job; status-check needs
Makefile                        # migrate → lightstack-migrate; install-tools drops golang-migrate's `migrate` CLI
docs/adr/ADR-003-pluggable-database-backend.md; docs/adr/ADR-001-…md (amendment note)
docs/CONFIGURATION.md, docs/ARCHITECTURE.md, docs/UPGRADING.md, docs/DOCKER_DEPLOYMENT.md, deployments/README-KVM.md, charts/lightstack/README.md, CLAUDE.md
.specify/memory/constitution.md # v1.3.0 (amended 2026-10-12)
```

**Structure Decision**: one Go module, as today. The new code lives in `internal/database`, the only package that knows both drivers. Services change types, not structure.

## Delivery sequence

Each step builds and passes its gates before the next starts. The first is behaviour-neutral, and its oracle is the gate results recorded beforehand.

1. **Baseline gates** — record unit tests (PostgreSQL leg), the contract suite, `quick_test.sh` and lint on the untouched branch.
2. **Contract + PostgreSQL implementation + call-site conversion** — types only; PostgreSQL runtime identical; gates must match step 1.
3. **Rewrites for both backends** (R8) — behaviour-equivalent on PostgreSQL; covered by existing and new DB-backed tests.
4. **Nova recheck + 403** (R14) and **Cinder reservations + sweep + refusals** (R13), with migration 074 — test-first: 403 test, race test and blocking test RED, then GREEN on PostgreSQL.
5. **SQLite implementation** — open sequence, translator, functions, values, error classes (R3–R7) — with its unit tests (translator table tests, value round-trips, settings read-back, file modes).
6. **Migrations** — view, iofs, golang-migrate wiring, file lock, baseline generator + generated baseline, parity test, `lightstack-migrate`, Makefile (R10–R12).
7. **Startup** — URL/backend/vxlan checks, node name, log line, sweep call, exit codes (R15).
8. **Tests and CI** — `dbtest` helper, backend-neutral fixtures, PostgreSQL-only guards, the static check, concurrency tests (deciding `_txlock`), contract seeding helper, CI jobs (R9, R16).
9. **Docs, ADR-003, constitution proposal** (R17).
10. **Validation** — gates green on both backends; real-mode host run on SQLite and PostgreSQL; record the `_txlock` decision and the busy wait in research.md.

## Complexity Tracking

| Violation | Why Needed | Simpler Alternative Rejected Because |
|---|---|---|
| Interface-based injection (`database.DB` instead of `*pgxpool.Pool`), reversing ADR-001's rejection (CLAUDE.md Article VIII) | Two backends behind one call-site syntax; ADR-001 deferred the interface until there was a second implementation, and this is it | `pgx/v5/stdlib` + `database/sql` for both changes PostgreSQL's pool and types (FR-003); a repository or ORM rewrites 736 statements |
| A SQL translation layer in `internal/database` | Callers write SQL once; SQLite cannot run PostgreSQL's casts (120 sites), `= ANY($n)` (8), `FOR UPDATE` (4), a `NULL` `LIMIT`/`OFFSET` placeholder or the interval arithmetic (1) | per-backend statement files double every statement; a full SQL parser is far beyond the grammar used (R5) |
| A per-backend view over one migrations folder | golang-migrate rejects two files with one version; existing files may not be renamed (FR-025) | two folders drift (rejected in #595); renumbering the existing migrations breaks every deployed database |
