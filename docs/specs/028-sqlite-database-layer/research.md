# Research: SQLite backend, part 1

Phase 0 output for [spec.md](spec.md). Code facts were measured on `feature/598` @ `278d106`. Driver facts come from the module sources of `modernc.org/sqlite` **v1.60.1** and `github.com/golang-migrate/migrate/v4` **v4.19.1**, and from a probe program run against v1.60.1 (appendix A). Upstream OpenStack facts come from the `stable/2025.2` (Flamingo) sources of Cinder and Nova.

## R1 — The database contract

- **Decision**: one package-level contract in `internal/database` — `DB`, `Querier`, `Tx`, `Rows`, `Row`, `Result`, `ErrNoRows`, error classes, `Backend` ([contracts/go-database-contract.md](contracts/go-database-contract.md)) — whose method sets **mirror pgx's**: context first, `Exec` returns a result with `RowsAffected() int64`, `Rows.Close()` returns nothing, `QueryRow` returns a row whose `Scan` reports `ErrNoRows`. Identifiers stay as they are (`pool`, `svc.pool`, `testPool`); only their types change, so the 736 call sites need no edit and the compiler finds every type to change.
- **Rationale**: the conversion stays mechanical and reviewable, and the PostgreSQL path keeps its exact runtime (FR-003).
- **Alternatives considered**:
  - *`database/sql` for both backends, PostgreSQL through `pgx/v5/stdlib`*: one implementation, but it swaps pgxpool for the `database/sql` pool and changes pgx's type mapping (arrays, JSONB, UUIDs) on the default backend. Rejected (FR-003).
  - *An ORM or query builder*: rewrites all 736 statements. Rejected (#595).
  - *A repository per service*: the same rewrite, spread over six packages. Rejected (#595, ADR-001).

## R2 — PostgreSQL implementation

- **Decision**: thin wrappers over today's `*pgxpool.Pool` (`postgres.go`), built with today's `pgxpool.ParseConfig` + `NewWithConfig` path and `PoolConfig`. Statements pass through untouched. Only two things are translated on the way out: `pgx.ErrNoRows` becomes `database.ErrNoRows`, and the error classes read `*pgconn.PgError` codes (23505 unique, 23503 foreign key, 23502 not null, 23514 check, 40001/40P01/55P03 busy).
- **Rationale**: FR-003 — same settings, pool, row locks and migration locking.
- **Alternatives considered**: keeping `pgx.ErrNoRows` visible beside `database.ErrNoRows` — rejected as a back-compat alias; the old name is gone, and the compiler finds all 165 checks.

## R3 — SQLite driver and opening

- **Decision**: `modernc.org/sqlite` **v1.60.1**, the newest release (`go list -m -versions`). It needs Go ≥ 1.26 (its `go.mod`), bundles **SQLite 3.53.4** (`lib/sqlite.go:4556`) and is pure Go, so `CGO_ENABLED=0` keeps working (`build/package/Dockerfile:22-27`). One DSN for every entry point:

  ```text
  file:<absolute path, URI-escaped>?mode=rw&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)
       &_pragma=busy_timeout(<ms>)&_pragma=foreign_keys(1)&_txlock=<deferred|immediate>
  ```

  Opening sequence:
  1. Create-if-missing entry points (`lightstack`, `lightstack-migrate`): `MkdirAll` the missing parents 0755 and `Mkdir(dir, 0750)` — a plain `MkdirAll(dir, 0750)` would create a missing state root 0750, which QEMU cannot traverse to its disks — then create the file with `O_CREATE|O_EXCL`, mode 0600. Must-exist entry points (`cleanup`, `console-tokens`): `stat`, fail naming the path.
  2. Tighten the database, `-wal` and `-shm` files to 0600 when they are wider, logging each change (FR-011a).
  3. `sql.Open("sqlite", dsn)`. **`mode=rw` is always set**: because Lightstack creates the file itself in step 1, the driver never creates one. The mode is therefore always Lightstack's, and must-exist needs no second DSN.
  4. Read `journal_mode`, `synchronous`, `foreign_keys` and `busy_timeout` back from a pooled connection, and fail if any differs from what was requested (FR-008).
- **Rationale** (verified):
  - The driver opens with `SQLITE_OPEN_READWRITE|SQLITE_OPEN_CREATE|SQLITE_OPEN_FULLMUTEX|SQLITE_OPEN_URI` (`conn.go:102-104`), so a `file:` URI is honoured.
  - `mode=rw` on a missing file fails with SQLITE_CANTOPEN (14) and creates nothing (probe).
  - The driver applies `_pragma` on every new connection (`sqlite.go:443-…`); the probe read back `wal`, `2`, `1` and `2000`.
  - v1.60.1 also accepts mattn-style shorthand keys and validates them (`sqlite.go:316-360`). v1.46.1 silently ignored them, so the `_pragma` form is used because it works in both, and the read-back is the guard.
  - SQLite gives `-wal`/`-shm` the database file's mode (probe: all three 0600). The driver's own default is 0644 (`SQLITE_DEFAULT_FILE_PERMISSIONS = 420`, `lib/sqlite_g_000000000001ffff.go:137`).
  - `synchronous` already defaults to FULL in this build (`SQLITE_DEFAULT_SYNCHRONOUS = 2`, `SQLITE_DEFAULT_WAL_SYNCHRONOUS = 2`, `lib/sqlite.go:3424,3428`). It is still set explicitly and read back, to pin the clarified durability against a future default.
  - `trusted_schema` is 1 (probe), so registered functions may appear in `DEFAULT` clauses (R6).
- **Busy wait**: **5000 ms** to start with. Write transactions no longer span storage calls (R13), so they last milliseconds; the concurrency tests (R16) confirm the value or lower it, and the result is recorded with them. Constitution v1.3.0 (Article III) caps it at 5 s, so it may not be raised.
- **Alternatives considered**:
  - *A plain path*: needs a second DSN for must-exist, and lets the driver create the file 0644.
  - *`mattn/go-sqlite3`*: requires cgo. Rejected (#595).

## R4 — Pool and transaction mode

- **Decision**:
  - Pool: `database/sql` with `SetMaxOpenConns(max_connections)`, `SetMaxIdleConns(min_connections)` (at least 1), `SetConnMaxLifetime`, `SetConnMaxIdleTime` from today's settings. `health_check_period` is PostgreSQL-only and is documented as such.
  - Transaction mode: run every concurrency test with `_txlock=deferred` and with `_txlock=immediate` (FR-039). **Expected outcome: `immediate`.**
- **Rationale**: the probe reproduces the failure FR-010 anticipates. A deferred transaction that reads, then writes under contention, fails with **SQLITE_BUSY at once, or SQLITE_BUSY_SNAPSHOT when the other writer had already committed; the busy wait applies to neither**. Deferred failed the 5 of the 13 inventory transactions that read before they write; the other 8 start with a write. Under `immediate`, `BEGIN IMMEDIATE` (`tx.go:23-24`) takes the write lock up front and waits up to the busy wait.
- **Alternatives considered**:
  - *Retrying on 517*: a retry loop, which Article III forbids.
  - *A single connection*: readiness and reads would queue behind writes (FR-009).
- **Result of the concurrency tests (T080, 2026-10-12)** — `TestCinderConcurrencyMatrix`, `TestNovaConcurrencyMatrix`, `TestNeutronConcurrencyMatrix`, 8 concurrent requests per case, SQLite leg:

  | Transaction | deferred | immediate |
  |---|---|---|
  | Cinder quota helper (volume create, limit 3) | FAIL: 3×2xx, 1×413, 4×500 | pass |
  | Volume attach | FAIL: 1×2xx, 6×400, 1×500 | pass |
  | Volume detach | FAIL: 3×2xx, 5×500 | pass |
  | Volume metadata replacement | pass | pass |
  | Snapshot metadata replacement | pass | pass |
  | Transfer create | pass | pass |
  | Transfer delete | pass | pass |
  | Nova quota check and server create (limit 3) | FAIL: 3×2xx, 2×403, 3×500 | pass |
  | Server delete | pass | pass |
  | Server metadata replacement | pass | pass |
  | Aggregate metadata merge (added by T036) | FAIL: 4×2xx, 4×500 | pass |
  | Default security group | pass | pass |
  | Port allocation for a server | pass | pass |

  The deferred failures are the read-then-write transactions, each a SQLITE_BUSY (`database is locked (5)`) at once — or SQLITE_BUSY_SNAPSHOT when the other writer had already committed, the busy wait applying to neither — surfacing as 500; the ones that pass begin with a write (or read outside the transaction), so their write lock is taken under the busy wait. **Decision: `immediate`**, the default in `internal/database/sqlite.go`; the busy wait stays at 5 s (constitution v1.3.0, Article III). The SQLite runs of T065 (two creates at the instance limit), T067 (reservations) and T068 (no transaction during the copy) are green with it.

## R5 — Translation for SQLite

- **Decision**: `sqlite_translate.go`, applied to every `Query`, `QueryRow` and `Exec` of the SQLite implementation (DB and Tx) and never to PostgreSQL.
  - **Lexer**: one pass. Single-quoted strings (`''` escape), double-quoted identifiers and `--`/`/* */` comments are copied verbatim. Dollar quoting is an error.
  - **Rules**:
    1. **Casts** `::type`:
       - `text`, `varchar`, `char` → `CAST(<operand> AS TEXT)`;
       - `int`, `integer`, `bigint`, `smallint` → `CAST(<operand> AS INTEGER)`;
       - every other target (`jsonb`, `json`, `uuid`, `uuid[]`, `text[]`, `timestamp`, `timestamptz`, `boolean`) → the cast is dropped.
       - The operand is the preceding primary: a placeholder, a literal, an identifier chain (`a.b`), or a parenthesised group with an optional function name. Any other shape is a translation error.
    2. **List membership**: `= ANY ( $n [::type[]] )` → `IN (…)`. The template marks the position. Per call, `args[n-1]` must be a slice; it expands to one placeholder per element, and every placeholder is renumbered with the argument slice rebuilt to match. An empty slice gives `IN ()`, which matches nothing (probe). A non-slice argument is an error.
    3. **Row locks**: `FOR UPDATE` is removed. `FOR SHARE`, `SKIP LOCKED` and `NOWAIT` are unsupported (R9).
    4. **Console expiry**: `now() + make_interval(secs => $n)` → `ls_add_seconds(now(), $n)` (`internal/console/store.go:123`, and one test fixture).
    5. **Regular expressions**: `x ~ y` → `x REGEXP y`, which SQLite evaluates as `regexp(y, x)` through the registered function (R6). `~*` and `!~` are unsupported (R9).
    6. **Limit and offset**: `LIMIT $n` → `LIMIT COALESCE($n, -1)` and `OFFSET $n` → `OFFSET COALESCE($n, 0)`, so a `NULL` placeholder means no limit, as PostgreSQL's `LIMIT NULL` does.
  - **Placeholders stay `$N`**: the driver binds `$N`/`?N` by the number N, not by position of appearance (`conn.go:590-615`; probe: an insert binding `$2, $1` out of order stored the right values).
  - **Cache**: translated templates are cached per statement text in a bounded map (4096 entries, cleared when full); only list expansion runs per call. Dynamic statements from `internal/common/sqlbuilder.go` add bounded variety, one template per filter combination.
  - **Failure mode**: any translation error is returned from the call and names the construct; nothing passes through untranslated.
- **Rationale**: FR-014, with the narrowest grammar the 736 statements need.
- **Changed from the ticket**: `SELECT pg_advisory_xact_lock(hashtext($1))` is not rewritten. `pg_advisory_xact_lock` and `hashtext` are registered as no-op functions (R6), so the statement runs unchanged. A rewritten no-op would have to keep the placeholder, because `database/sql` checks the argument count against the statement.
- **Alternatives considered**:
  - *Dropping every cast*: loses the integer casts' semantics.
  - *A full SQL parser*: far more than the 736 statements need.
  - *Per-backend statement files*: duplicates every statement.

## R6 — Functions registered on every SQLite connection

`sqlite.RegisterScalarFunction` / `RegisterDeterministicScalarFunction` apply to every connection opened afterwards (`sqlite.go:729-802`). Registration happens once, at package initialisation.

| Function | Result | Deterministic |
|---|---|---|
| `now()` | `time.Now().UTC()` as `2006-01-02T15:04:05.000000Z` | no |
| `gen_random_uuid()` | `uuid.NewString()` (2 call sites + 63 column defaults) | no |
| `ls_add_seconds(ts, secs)` | `ts` parsed, plus `secs` seconds, formatted as above | yes |
| `pg_advisory_xact_lock(x)` | `NULL` — no-op; serialisation comes from the write lock | no |
| `hashtext(x)` | `0` | yes |
| `regexp(pattern, value)` | Go's `regexp.MatchString`, with a compiled-pattern cache; makes `x REGEXP y` work (R5 rule 5) | yes |

- **Known difference**: PostgreSQL's `now()` is the transaction's start time; the registered `now()` is the call time. Nothing relies on equal timestamps from two calls: there are 0 `NOW(), NOW()` pairs and 0 `created_at = updated_at` comparisons in production code.
- **Probe**: a `STRICT` table with `DEFAULT (now())` was created and filled through the registered function.

## R7 — Values at the boundary, and error classes

- **Bind** (SQLite only):

  | Go value | Bound as |
  |---|---|
  | `time.Time`, `*time.Time` | canonical text (FR-019) |
  | `[]byte`, `json.RawMessage` | **text** |
  | maps, and structs other than `time.Time` | JSON text |
  | slices other than `[]byte` outside a list-membership position | JSON array text |
  | `driver.Valuer` (`uuid.UUID` included) | `Value()`, then the rules above |
  | `bool` | 0/1, by the driver (`conn.go:513-520`) |
  | integers, floats, strings, `nil` | unchanged |

  `[]byte` becomes text because a STRICT `TEXT` column refuses a BLOB: the probe got `cannot store BLOB value in TEXT column (3091)`.
- **Scan** (SQLite only), driven by the destination's type:

  | Destination | Conversion |
  |---|---|
  | `*time.Time`, `**time.Time`, `*sql.NullTime` | parse the canonical text; any other text is an error, never a silent zero |
  | maps, structs, slices other than `[]byte` | `json.Unmarshal` |
  | everything else | `database/sql`'s conversion: integer → `bool`, `string` → `[]byte`, integer → `string` (needed for `->>`, which returns INTEGER for JSON numbers — probe) |

- **Error classes**: extended result codes are on by default (`conn.go:113`). SQLITE_CONSTRAINT_UNIQUE 2067 and **PRIMARYKEY 1555** (the probe's duplicate key reported 1555, so both count as unique), FOREIGNKEY 787, NOTNULL 1299, CHECK 275, BUSY 5 / BUSY_SNAPSHOT 517 / LOCKED 6 (`lib/sqlite.go:3108-3266,3938`). `sql.ErrNoRows` becomes `database.ErrNoRows`.

## R8 — Statements rewritten once, for both backends (FR-016)

| Site | New form |
|---|---|
| `internal/nova/tenant_usage.go:31,116` | select each instance's `created_at`; hours summed in Go against one `time.Now()` |
| `internal/neutron/vxlan_coordinator.go:210-212` | `WITH RECURSIVE r(vni) AS (SELECT $1 UNION ALL SELECT vni+1 FROM r WHERE vni < $2) SELECT vni FROM r WHERE vni NOT IN (SELECT vni FROM network_vni_allocations) LIMIT 1` |
| `internal/metadata/service.go:116` | candidates `WHERE device_id IS NOT NULL AND fixed_ips::text LIKE '%' \|\| $1 \|\| '%'` with the quoted address as `$1`, exact `ip_address` match in Go — the pre-filter keeps PostgreSQL from shipping every port per metadata request (R19) |
| `internal/neutron/network.go:1901` | candidates from the `router_interfaces` join (already narrowed by subnet), exact match in Go |
| `internal/nova/aggregates.go:463` | read (`FOR UPDATE` on PostgreSQL), merge in Go, write — one transaction |
| `internal/neutron/reconciler.go:318` | `ROW_NUMBER() OVER (PARTITION BY network_id ORDER BY created_at, id) = 1` |
| `internal/nova/server_filter.go:244-246` (`ip`) | fetch the scope's ports (paired by project with their server), match each address with Go's `regexp` (patterns already validated with it, `:221`), add `id = ANY($n::uuid[])` (the cast keeps PostgreSQL's `uuid = uuid` comparison; SQLite drops it and expands the list); this removes the only `jsonb_array_elements`. Upstream also matches `ip` in Python (`nova/compute/api.py:3244`). `name ~ $n` (`:209`) is **not** rewritten: upstream filters `name` in the database with the backend's operator (`nova/db/main/api.py:1990-2003`), so PostgreSQL keeps `~` and SQLite gets `REGEXP` (R5 rule 5) |
| 7 × `CURRENT_TIMESTAMP` (`nova/handlers.go:3295`, `keystone/handlers.go:1230,1370`, `cinder/volumes.go:2231,2233,2445,2447`) | `NOW()` |
| `internal/neutron/router.go:819` (`pgx.CollectRows`) | scan loop |
| `internal/nova/handlers.go:1714,1809` (`ARRAY_AGG … FILTER`, `'{}'` default) | one row per port × floating address via the existing `LEFT JOIN`, no `GROUP BY`; addresses collected per port in Go. Found while specifying |

Kept unchanged, and verified on SQLite 3.53.4 by the probe:
- `->>` and `->`: the SNAT predicate is `0` for `false` and `1` for a missing key, as on PostgreSQL;
- `IS NOT DISTINCT FROM` (`nova/handlers.go:2639`, `nova/state_reconciler.go:212`);
- `json_group_array` over zero rows gives `[]`;
- `COUNT(DISTINCT …)`, `ON CONFLICT … DO UPDATE … excluded.`, `RETURNING`, window functions and `WITH RECURSIVE`.

## R9 — Static construct check (FR-018)

- **Decision**: `internal/database/sqlcheck_test.go`.
  - **Inputs**: it parses every `.go` file under `internal/` with `go/parser`. A string literal or constant concatenation counts as SQL when it contains a statement keyword as a whole word (`SELECT`, `INSERT`, `UPDATE`, `DELETE`, `WITH`, `FROM`, `WHERE`, `SET`, `VALUES`, `JOIN`, `RETURNING`, `ORDER BY`, `GROUP BY`), or when it is passed to the `internal/common/sqlbuilder.go` methods.
  - **Lexing**: the translator's lexer.
  - **Fails on**:
    - a function call outside an allow-list: `count`, `sum`, `min`, `max`, `avg`, `coalesce`, `nullif`, `lower`, `upper`, `length`, `abs`, `round`, `now`, `gen_random_uuid`, `pg_advisory_xact_lock`, `hashtext`, `make_interval` (only in the supported form), `row_number`, `exists`, `cast`;
    - a deny-listed PostgreSQL operator or keyword: `~*`, `!~`, `ILIKE`, `SIMILAR TO`, `@>`, `<@`, `?|`, `?&`, `#>`, `#>>`, `DISTINCT ON`, `FOR SHARE`, `SKIP LOCKED`, `NOWAIT`, `LATERAL`, `ARRAY[`, `INTERVAL`, `CURRENT_TIMESTAMP`, `ON CONFLICT ON CONSTRAINT`, `DELETE … USING`, `$$`, `TRUNCATE`, `LISTEN`, `NOTIFY`;
    - a cast to an unknown type;
    - `ANY(` outside `= ANY($n)`.
  - **Exemptions**: a PostgreSQL-only test literal carries `//sqlcheck:postgres-only`.
  - **Seeded test**: one failing literal per class (SC-009).
- **Rationale**: an allow-list for functions, which is the open-ended class, and a deny-list for syntax, which is finite and known. Queries no test executes are caught.

## R10 — Migrations: one folder, one view per backend

- **Decision**:
  - **View**: `migrationView(dir, backend) fs.FS`, implementing `fs.ReadDirFS`. It applies the three rules (FR-024) and serves the kept files under their own names. `iofs.New(view, ".")` reads it: `fs.ReadDir`, `DefaultParse`, and `ErrDuplicateMigration` on a collision (`source/iofs/iofs.go:49-77`; regex `source/parse.go:22`). Non-migration files such as `README.md` are skipped by the parser.
  - **PostgreSQL**: `migrate.NewWithSourceInstance("iofs", src, dbURL)` (`migrate.go:145`) with the postgres driver and its unchanged advisory lock (`database/postgres/postgres.go:241`).
  - **SQLite**:
    - the migration run opens its **own** `*sql.DB` through the R3 opener (create-if-missing, same settings), then `sqlite.WithInstance(db, &sqlite.Config{})` (`database/sqlite/sqlite.go:42`) + `migrate.NewWithInstance`. The driver wraps each file in a transaction (`sqlite.go:202-217`).
    - its own `*sql.DB` because `m.Close()` closes the driver's database, which must never be the service pool.
    - importing `database/sqlite` pulls in `modernc.org/sqlite` only, not `mattn/go-sqlite3`.
  - **Validation**: `ValidateMigrations` checks both views — up/down pairs, exactly one SQLite baseline, no duplicate after the rules.
  - **Below the baseline**: on SQLite, `MigrateToVersion` / `ForceMigrationVersion` with a version below the baseline are refused, naming it. `MigrateDown` / `MigrateReset` may go through the baseline's down file to an empty database (reversibility).
  - **`lightstack-migrate`**: picks the backend from the URL. `make migrate` becomes `LIGHTSTACK_DB_URL="$(DB_URL)" go run ./cmd/lightstack-migrate up -migrations migrations` (`Makefile:195-197`), and `make install-tools` drops golang-migrate's `migrate` CLI (`Makefile:240`).
- **Rationale**: golang-migrate rejects two files with one version, and golang-migrate's `migrate` CLI cannot see a view.
- **Alternatives considered**:
  - *Two folders*: rejected by the user (#595 thread).
  - *Renaming the existing files*: rejected (FR-025).

## R11 — The SQLite baseline and the parity test

- **Decision — the baseline is generated, not hand-written**:
  - **Generator**: a test-only generator (`internal/database/baseline_gen_test.go`, run with `-run TestGenerateSQLiteBaseline -update`) migrates a scratch PostgreSQL database to **073**, introspects `information_schema`/`pg_catalog`, and writes `migrations/073_baseline.sqlite.up.sql` (+ `.down.sql`). The generated file is committed.
  - **Mappings**: data-model §3 — `STRICT` tables, text ids, `VARCHAR(n)` → `TEXT CHECK (length(col) <= n)` (R18), `SERIAL` → `INTEGER PRIMARY KEY AUTOINCREMENT`, defaults mapped, the 6 `CHECK`s and the 4 partial-index predicates copied, the GIN index skipped.
  - **Seed rows**: every row present after migrating an empty database. An id the seed generated randomly (a version-4 UUID in a `gen_random_uuid()` column that no migration spells out) is rendered as `gen_random_uuid()`, so every installation gets its own, as on PostgreSQL, and regenerating the file reproduces it byte for byte. The generator fails if another seed row refers to such an id. Rows are ordered by their rendered text.
- **Decision — the parity test (FR-028)**:
  - **Procedure**: migrate an empty PostgreSQL database and an empty SQLite file from the folder; introspect both (PostgreSQL catalogs; SQLite `pragma table_xinfo`, `foreign_key_list`, `index_list`, `index_info` and `sqlite_schema`); normalise; compare.
  - **Normalisation**:
    - types by family;
    - `VARCHAR(n)` ≡ `TEXT` + its length `CHECK`;
    - defaults: `gen_random_uuid()` ≡ `(gen_random_uuid())`, `now()`/`CURRENT_TIMESTAMP` ≡ `(now())`, `true` ≡ `1`, `'{}'::text[]` ≡ `'[]'`;
    - seed rows by natural key where ids are random.
  - **Where it runs**: in the PostgreSQL leg of CI, which has both databases.
- **Rationale**: 60+ tables, 151 indexes and the seed data are too much to transcribe by hand without drift, and the generator documents every mapping as code.
- **Alternatives considered**:
  - *A hand-written baseline*: drift-prone.
  - *Applying PostgreSQL DDL to SQLite*: a column declared `UUID` gets NUMERIC affinity and corrupts all-digit ids (#595).

## R12 — Migration file lock (FR-030)

- **Decision**:
  - **Lock**: `syscall.Flock(LOCK_EX|LOCK_NB)` on `<db>.migrate-lock` (0600), polled every 100 ms up to golang-migrate's `DefaultLockTimeout` (15 s, `migrate.go:27`), then an error naming the lock file. Held around the whole run, outside golang-migrate's in-process lock (`database/sqlite/sqlite.go:175-187`, an `atomic.Bool` that does not span processes).
  - **Platforms**: `flock` exists on Linux and macOS, so stub mode on macOS keeps working (Article IV).
- **Alternatives considered**: a lock row in the database — it needs the schema that does not exist yet, which is the case being guarded.

## R13 — Reservations, the sweep and status refusals (FR-032 – FR-036)

- **Volume create and boot from a new volume** (`provisionVolume`, `internal/cinder/volumes.go:484-528`):
  1. `withVolumeQuota` inserts the row `creating` with `reserved_by = <node>`, then commits.
  2. `writeVolume`'s backing calls (`:533-569`) run outside any transaction.
  3. On failure: `DELETE … WHERE id = $1 AND status = 'creating'`, and reclaim the partial backing.
  4. On success: `UPDATE … SET status = 'available', bootable, source_image_id, updated_at WHERE id = $1 AND status = 'creating'`. If no row is affected, reclaim the backing and fail.
- **Snapshot create** (`:1542-1650`): the same shape around `cephClient.CreateSnapshot`. The deferred goroutine that flipped the status 100 ms later (`:1620-1635`) is removed, because the flip now happens before the response. The response stays `202` with `creating` (FR-036).
- **Sweep**: `cinderService.SweepInterruptedReservations(ctx)` runs from `main` before the listeners open:
  - `UPDATE volumes SET status = 'error', updated_at = now() WHERE status = 'creating' AND reserved_by = $1 RETURNING id`, and the same for snapshots (`RETURNING id, volume_id`);
  - each backing is then reclaimed through the fail-fast storage client. A failure is logged and startup continues; deleting the errored volume later reclaims it.
- **Upstream**: the volume service sets resources it was working on that are still `creating` or `downloading` to `error` per service at start (`cinder/volume/manager.py:_do_cleanup`, lines 691-722 on `stable/2025.2`, scoped by the cleanable-workers mechanism). It does not reclaim the backing; Lightstack does, as #598 asks.
- **Refusals while `creating`** (upstream `cinder/volume/api.py` on `stable/2025.2`), all `400` through `common.NewBadRequestError`:

  | Request | Upstream rule | Upstream message |
  |---|---|---|
  | Volume delete, non-forced | `:478-516`, status must be one of `available`, `error`, `error_restoring`, `error_extending`, `error_managing` | `Invalid volume: Volume status must be available or error or error_restoring or error_extending or error_managing and must not be migrating, attached, belong to a group, have snapshots, awaiting a transfer, or be disassociated from snapshots after volume transfer.` |
  | Extend | `:1556-1594`, status must be `available` (or `in-use` for online extend) | `Invalid volume: Volume <id> status must be 'available' to extend, currently creating.` |
  | Snapshot delete | `:1221-1250`, status must be `available` or `error` | `Invalid snapshot: Snapshot status must be available or error and must not be part of a group.` |

  Attach already requires `available` (`internal/cinder/volumes.go:928-931`).
- **Node name**: `LIGHTSTACK_NODE_NAME`, else `os.Hostname()`, resolved once in `main` and passed to `cinder.NewService` (clarification 2026-10-12).
- **Quota usage** already counts `status != 'deleted'` (`internal/cinder/quotas.go:74,90,486-491`).

- **Audit (T082, SC-008, 2026-10-12)**: every transaction body in `internal/` was extracted mechanically — the callback of each `database.WithTx`, `withCinderQuota` and `withVolumeQuota` call (brace-matched), and the span from `svc.pool.Begin(` to its `Commit` — together with one level of the helpers each body calls, and searched for storage, hypervisor, host-networking, HTTP, process and file operations (`cephClient.`, `vmManager.`, `libvirt`, `storage.`, `brManager.`, `tapManager.`, `dhcpManager.`, `mdRedirect.`, `http.Get/Post/Do/NewRequest`, `exec.Command`, `os.Remove/Create/WriteFile/Rename/Mkdir`, `cinderSvc.`, `neutronSvc.`, `fetchImageBase`). The method was checked against the code before this feature, where it finds exactly the two known cases (volume provisioning through `writeVolume`, and snapshot create).
  - Bodies: 22 — the 13 inventory transactions, the aggregate metadata merge (`internal/nova/aggregates.go`, T036), and the 8 bodies handed to the quota helper (volume and snapshot reservations, extend, volume and snapshot manage, backup create and restore, transfer accept).
  - Result: **no external operation in any of them.** The helpers they call — `checkQuotaLocked`, `lockCinderQuota`, `checkLockedCinderQuota`, `reserveVolume` — are database-only.

## R14 — Nova: check and recheck, and `403` (FR-035, FR-035a)

- **Early checks**: `CheckQuota` × 3 (`internal/nova/handlers.go:778,789,800`) stay as a non-binding refusal.
- **Binding recheck**: inside the insert transaction (`:1037`), before `INSERT INTO instances`:
  1. `SELECT pg_advisory_xact_lock(hashtext($1))` with `'nova_quota:' || project`. This is Cinder's pattern (`internal/cinder/quotas.go:409-421`): a project with no `quotas` row runs on code defaults, and a `FOR UPDATE` on a missing row locks nothing, so the race would stay open for exactly the common case.
  2. Usage plus limit for instances, cores and RAM, through the existing `usageQuery` (`internal/nova/quotas.go:402-433`) and limit lookup.
  3. On refusal: roll back; the existing failure path removes a provisioned boot volume (`:1060-1061`).
  On SQLite the advisory lock is the registered no-op, and `immediate` transactions serialise the check.
- **`403`**: upstream answers `OverQuota` on create with `HTTPForbidden(explanation=error.format_message())` (`nova/api/openstack/compute/servers.py:797-799`). `TooManyInstances` reads `Quota exceeded for %(overs)s: Requested %(req)s, but already used %(used)s of %(allowed)s %(overs)s` (`nova/exception.py:1383-1385`). Nova's four refusals — the early checks and the boot volume's (`handlers.go:780,791,802,987`) — move from `NewQuotaExceededError` (413 `overLimit`, `internal/common/errors.go:280-287`) to `NewForbiddenError` with that message, built from `QuotaExceededError`'s `Limit`, `Usage` and `Requested`. Cinder keeps its upstream-correct 413 (`internal/cinder/quotas.go:572`). `TestBootVolumeQuotaRefusalIs413` (`internal/nova/boot_volume_test.go:338-348`) is rewritten first, to expect 403 (RED).

## R15 — Startup order, exit codes, log line

- **Server order**:
  1. `loadConfig`;
  2. `database.ParseURL` and `CheckDeclaredBackend(os.Getenv("LIGHTSTACK_DB_BACKEND"))`;
  3. the multi-node guard;
  4. node name, then `InitLogger`;
  5. `database.Open`;
  6. `MigrateUp`;
  7. `Describe` and the log line;
  8. `BootstrapAdmin`, then the services;
  9. the Cinder sweep;
  10. the listeners.

  Every check that can refuse runs before step 5, so a refusal leaves nothing on disk (SC-003).
- **Exit codes**: the server keeps its fatal exit (1); `cleanup` and `console-tokens` use their config-error code (2) for refused configurations (`cmd/lightstack/main.go:566-592,514-566`). Messages are in [contracts/configuration-and-startup.md](contracts/configuration-and-startup.md).
- **Missing SQLite file in `cleanup`**: today, `teardown.Run` treats any failed `Connect` as "database unreachable" (`internal/teardown/teardown.go:84-92`). The must-exist failure falls into that path unchanged, which is the clarified behaviour. A refused URL or a backend mismatch never reaches it, because `runCleanup` validates first and exits 2.

## R16 — Tests and CI

- **Shared test helper** `internal/database/dbtest`: it replaces the 10 hand-written pool openers. `Open(migrationsPath)` reads `LIGHTSTACK_TEST_DB_URL`:
  - **unset** → skip, as today (clarification);
  - **`postgres://`** → as today;
  - **`sqlite:///<path>`** → a fresh per-package file (`<path>` with the package name appended), because `go test` runs packages in parallel processes.
  - **`RequirePostgres(t)`** guards one test file — the `groups.domain_id` migration test, whose PL/pgSQL repair block is PostgreSQL-only.
- **Test fixtures made backend-neutral**:
  - `interval` literals → Go-computed times (`console/helpers_test.go:59`, `console/store_test.go:324`, `console/cli_test.go:154`, `nova/console_test.go:807`);
  - `generate_series` → Go loops (`neutron/listing_page_size_test.go:38-50`);
  - `ARRAY[…]` → a bound `[]string` (`neutron/update_statement_test.go:229`).
- **Go's test cache**: it does not see `TestMain`'s environment reads reliably, so both legs run with `-count=1`.
- **Concurrency tests** (FR-039), each run on SQLite with `deferred` and with `immediate`:
  - quotas, attach, detach, port allocation, the default security group, server create and delete, the three metadata replacements, transfer create and delete;
  - **SC-006**: a fake storage client that blocks for 10 s during a volume-from-image create, while another project's write must finish in under 1 s;
  - **SC-005**: two concurrent server creates at the limit — one `202`, one `403` — on both backends, 20 runs;
  - **SC-010**: two "nodes" (two Cinder services with different node names) on one PostgreSQL database;
  - **SC-007**: two migrators as two processes (test-binary re-exec), followed by `PRAGMA integrity_check`.
- **Contract suite**:
  - `test/contract` is in the main module, so a `contract.Seed(t, sql, args...)` helper runs a statement on `LIGHTSTACK_DB_URL` through `database.Open` and replaces the 6 `docker exec lightstack-postgres psql` seedings (`cinder/manage_test.go:155`, `neutron/trunks_test.go:38,115,171`, `neutron/l3_agents_test.go:153,163`).
  - CI's existing contract job sets `LIGHTSTACK_DB_URL` to the Compose PostgreSQL.
- **CI** (`.github/workflows/ci.yml`):
  - the `unit-tests` job (`:56`) adds a SQLite step;
  - a new `contract-tests-sqlite` job builds the binary, writes a stub-mode config with `database.url: sqlite:///${RUNNER_TEMP}/ls/db/lightstack.db`, starts it, waits for `/readyz`, then runs the contract suite and `test/quick_test.sh`;
  - the new job joins `status-check`'s `needs` (`:389`).

## R17 — Constitution

- **Finding**: Article VI ("Shared database connection pool (PostgreSQL)", "~35MB binary") and *Technology Constraints* ("Database: PostgreSQL 14+") name PostgreSQL as the only database.
- **Decision**: amend the constitution to **v1.3.0 (MINOR)** — done on 2026-10-12 in `.specify/memory/constitution.md`. Besides the bounded-lock-wait clause (Article III) and the reservation clause (Article II), it does three things:
  - *Technology Constraints* gains "or SQLite (embedded through `modernc.org/sqlite`; single-node deployments only)";
  - Article VI's pool line becomes backend-neutral;
  - the size figure, which the build had outgrown, becomes "small binary" — stripped `lightstack` 44.8 MB today, about +3.9 MB with the driver (R22). (First "~50MB"; replaced by "small" on 2026-10-13, at the user's request: a figure goes stale with every dependency.)
- **Rationale**: the constitution's own precedent (v1.2.0): a plan's justification is not an amendment.
- **Status**: written; T102 checked the implementation against it on 2026-10-12 — the measured stripped binary is 51.2 MB; Article VI and its restatements say "small binary" rather than a figure. Committing it is the branch owner's.

## R18 — `VARCHAR(n)` length parity

- **Finding**: `internal/` validates string lengths in **one** place (`cinder/volumes.go:1176`). PostgreSQL's `VARCHAR(n)` — 69 × 255, 43 × 50, 9 others — is today's only length guard, and SQLite's `TEXT` has none.
- **Decision**: the baseline gives every `VARCHAR(n)` column `CHECK (length(col) <= n)`, which counts characters as PostgreSQL does. An over-long value then fails on both backends, as it does today on PostgreSQL. Improving that failure to a `400` is out of scope.
- **Alternatives considered**: accepting leniency on SQLite — rejected because the same request would then succeed on one backend and fail on the other (US1 AS6).

## R19 — Metadata lookup cost on PostgreSQL

- **Finding**: `fixed_ips @> $1::jsonb` has no index today (`ports` indexes: `network_id`, `device_id`, `device_owner`, `created_at`), so PostgreSQL already scans. Matching in Go would also ship every attached port to the process on each metadata request — `cloud-init` makes dozens per boot.
- **Decision**: R8's text pre-filter keeps the transferred rows at "ports whose addresses contain this string". The exact match stays in Go.
- **Rationale**: no regression for PostgreSQL deployments (US2).

## R20 — Migration conventions for SQLite

The probe on 3.53.4 confirms:
- `ALTER TABLE … ADD COLUMN` refuses a non-constant default (`DEFAULT (now())`) and `NOT NULL` without a default;
- plain `ADD COLUMN` and `DROP COLUMN` work.

`migrations/README.md` therefore says:
- timestamp defaults through `now()` work in `CREATE TABLE` only;
- a column added later gets its value from the application, or a `.postgres`/`.sqlite` pair;
- changes SQLite's `ALTER TABLE` cannot express (types, constraints) need the documented table rebuild in a `.sqlite` file. For a table nothing references, the rebuild runs inside the file's transaction.
- **Correction (implementation, 2026-10-12)**: `Config.NoTxWrap` exists but applies to every file of a run, not to one file — Lightstack builds the sqlite driver from that `Config` struct, with `NoTxWrap` false — so every file runs in a transaction with foreign keys on. A probe showed what that means for a table **other tables reference**: the rebuild succeeds and `DROP TABLE` deletes every referencing row through `ON DELETE CASCADE`. `migrations/README.md` therefore forbids such rebuilds; the first one needed comes with a migrator change that lets one file run outside a transaction.

Migration 074 (data-model §1) follows the plain-file path.

## R21 — Corrections found while planning

| Item | Was | Is |
|---|---|---|
| Server create success code (#598, spec) | `201` | **`202`** — `internal/nova/handlers.go:1369`, as upstream; spec corrected |
| Quota refusal for a server create | spec said `403`, code says `413 overLimit` | **`403`**, decided 2026-10-12 (FR-035a) |
| Indexes in the schema | 161 (`CREATE INDEX` statements) | **151 distinct** at 073 |
| Test files that open a pool | "10 `maindb_test.go` / `*_db_test.go`" | 8 test mains + `keystone/domain_resolve_test.go` + `keystone/pool_injection_test.go`, plus `test/benchmark` |

## R22 — Dependencies and binary size

- **New modules**: `modernc.org/sqlite` v1.60.1 and its dependencies (`modernc.org/libc`, `mathutil`, `memory`, `ncruces/go-strftime`, `remyoudompheng/bigfft`, `dustin/go-humanize`; `modernc.org/fileutil` is a test-only dependency of the driver). `mattn/go-isatty` moves 0.0.20 → 0.0.24. `golang-migrate`'s `database/sqlite` and `source/iofs` are packages of a module already required; it stays at v4.19.1.
- **Binary size**, stripped as the image builds it: `lightstack` 44.8 MB, `lightstack-migrate` 14.7 MB. The driver adds about 3.9 MB, measured with a two-program probe on v1.46.1 (1.9 → 5.8 MB). The constitution's "~35MB" was already out of date before this feature (R17).

## Appendix A — probe (modernc.org/sqlite v1.60.1, SQLite 3.53.4)

```text
journal_mode wal | synchronous 2 | foreign_keys 1 | busy_timeout 2000 | trusted_schema 1
STRICT table + DEFAULT (now()) via registered function: ok; default value 2026-10-12T11:34:46.184409Z
insert binding $2,$1 out of order: ok, values correct | bool → 1
[]byte into STRICT TEXT: "cannot store BLOB value in TEXT column" (3091)
x IN () → 0 rows | NULL IS NOT DISTINCT FROM NULL → 1 | '{"x":1}'->>'x' → 1 (integer)
'{"enable_snat":false}'->'enable_snat' → 'false' | SNAT predicate: false → 0, missing → 1
json_group_array over 0 rows → '[]' (also with FILTER) | CAST('1.5' AS INTEGER) → 1 | NULLs sort first ascending
ADD COLUMN DEFAULT (now()): "Cannot add a column with non-constant default"
ADD COLUMN NOT NULL without default: "Cannot add a NOT NULL column with default value NULL"
plain ADD COLUMN / DROP COLUMN: ok | duplicate primary key → extended code 1555
deferred tx: read, other connection commits, then write → 517 (SQLITE_BUSY_SNAPSHOT)
file pre-created 0600 → .db, -wal, -shm all 0600 while open
file:<path>?mode=rw on a missing file → "unable to open database file (14)", nothing created
```

## Appendix B — commands behind the counts

```bash
G=/usr/bin/grep
$G -rnE '\b(pool|tx|db)\.(Query|QueryRow|Exec|Begin)\(' --include='*.go' internal cmd pkg | $G -v '_test.go:' | wc -l   # 736 (67 files with -l)
$G -rn 'pgx.ErrNoRows' --include='*.go' . | $G -v '_test.go:' | wc -l                                                  # 165
$G -rn '\*pgxpool\.Pool' --include='*.go' . | $G -v '_test.go:' | wc -l                                                # 61 (28 files)
$G -rn 'pgx\.Tx\b' --include='*.go' . | $G -v '_test.go:' | wc -l                                                      # 29 (10 files; 23 in internal/cinder)
$G -rn 'RowsAffected()' --include='*.go' . | $G -v '_test.go:' | wc -l                                                 # 52
$G -ohiE 'CREATE (UNIQUE )?INDEX( IF NOT EXISTS)? [a-z0-9_]+' migrations/*.up.sql | awk '{print $NF}' | sort -u | wc -l # 151
$G -ohE 'DEFAULT gen_random_uuid\(\)' migrations/*.up.sql | wc -l                                                     # 63
$G -ohiE 'DEFAULT (now\(\)|CURRENT_TIMESTAMP)' migrations/*.up.sql | wc -l                                           # 123
```
