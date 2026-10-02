# Research: Inject the database pool through service constructors (ADR-001)

**Feature**: `027-database-pool-injection` | **Date**: 2026-10-01 | **Spec**: [spec.md](spec.md)

Every number below was measured on `feature/533` @ `44ed3d1` with `git grep`. The method lives in the [Inventory](#r12-inventory-method) section.

## R1. What is injected

**Decision**: The concrete `*pgxpool.Pool` from `github.com/jackc/pgx/v5/pgxpool`, already a module dependency. One pool per process.

**Rationale**: ADR-001 settles this, and the spec does not reopen it (FR-007). Using the pool directly keeps every call site's method set (`QueryRow`, `Query`, `Exec`, `Begin`) unchanged. The conversion is a change of receiver, not of API.

**Alternatives considered**: An interface, per-service pools and a repository layer — all rejected in ADR-001 and out of scope.

## R2. Constructor rule

**Decision**: Every database-using constructor takes `pool *pgxpool.Pool` as its **first** parameter and stores it in a `pool` field:

| Constructor | Today | After |
|---|---|---|
| `keystone.NewAuthService` | `(jwtSecret, tokenTTL, cache)` | `(pool, jwtSecret, tokenTTL, cache)` |
| `keystone.NewService` | `(authService, cache)` | `(pool, authService, cache)` |
| `nova.NewService` | `(libvirtURI, libvirtMode, cache)` | `(pool, libvirtURI, libvirtMode, cache)` |
| `neutron.NewService` | `(mode, cache)` | `(pool, mode, cache)` |
| `cinder.NewService` | `(mode, cephPool, cephConf)` | `(pool, mode, cephPool, cephConf)` |
| `glance.NewService` | `(mode, cephPool, cephConf, s3opts, cache)` | `(pool, mode, cephPool, cephConf, s3opts, cache)` |
| `metadata.NewService` | `(bindAddr)` | `(pool, bindAddr)` |
| `compute.NewNodeRegistry` | `(nodeID, tunnelIP, heartbeatInterval)` | `(pool, nodeID, tunnelIP, heartbeatInterval)` |
| `neutron.NewVXLANCoordinator` | `(vxlanManager, nodeRegistry, nsManager, pollInterval, vniStart, vniEnd)` | `(pool, vxlanManager, …)` |

There are two exceptions, both deliberate:

- `neutron.NewNetworkReconciler(svc, pollInterval)` already holds the Neutron service, so it reads `r.svc.pool`. Adding a second copy of the same pool would invite the two to drift.
- `placement.NewService()` never touches the database (Clarifications 2026-10-01) and is unchanged.

**Rationale**: This is ADR-001's constructor pattern. The receiver-type split of today's references is 667 in methods: nova `*Service` 209, neutron `*Service` 174 + `*NetworkReconciler` 4 + `*VXLANCoordinator` 6, cinder `*Service` 111, keystone `*Service` 80 + `*AuthService` 16, glance `*Service` 54, metadata `*Service` 10 and compute `*NodeRegistry` 3. So each of these types gains exactly one field.

**Alternatives considered**: Giving keystone's `Service` the pool through its `AuthService`. Rejected because ADR-001 and #554 want every `NewService` to take the pool explicitly, and a hidden second path is what this feature removes.

## R3. Package-level helpers

**Decision**: The 20 package-level functions that reach the database take the pool as a **parameter**. 18 use the handle directly (23 references), and 2 reach it only through the transaction helper (`neutron.EnsureDefaultSecurityGroup`, `nova.CheckQuota`; found during implementation, after `WithTx` gained its pool parameter). They take the pool right after `ctx` when they have one (`(ctx, pool, …)`), first otherwise. They do not become methods.

| Package | Helpers | Callers |
|---|---|---|
| `cinder` | `cinderQuotaUsage`, `resolveCinderQuotaLimits` | `quotas.go:131,142,226`, `volumes.go:2095` — all inside `*Service` methods |
| `common` | `ResolveProjectRef` (and its caller `QuotaTargetProject`) | 11 call sites in nova/cinder/neutron handlers |
| `keystone` | `BuildServiceCatalog` (no ctx → `pool` first) | `auth.go:407,612,778` (`*AuthService` methods) + 3 test calls |
| `keystone` | `createAdmin`, `grantAdminRole`, `seedCustomAdmin`, `seedDefaultAdmin` via `BootstrapAdmin(ctx, pool, cfg)` | `main.go:120` + 13 test calls |
| `neutron` | `listSecurityGroupRows`, `securityGroupOwnedByCaller`, `securityGroupRulesByGroup`, `queryStrings` | `ports.go:786,798,855,1011,1035`, `teardown.go:76,96,108,128,135` |
| `nova` | `securityGroupNamesForServer(s)`, `allocateNextIP`, `countUsage`, `resolveQuotaLimits` | `handlers.go:1104,1247,2065`, `interface_attach.go:118`, `quotas.go:34,192,253` |
| `nova` | `CheckQuota` (via `WithTx`) | `handlers.go:433,444,455` |
| `neutron` | `EnsureDefaultSecurityGroup` (via `WithTx`) | `ports.go:136,1393`, `nova/handlers.go:519,541` + 4 nova test calls |
| `teardown` | `enumerateDomainIDs` | `teardown.go:95` |

**Rationale**:

- One rule for all 20 is easier to apply and to review than a per-helper choice.
- It matches ADR-001's own `WithTx(ctx, pool, fn)`.
- `BootstrapAdmin` (called from `main` before any service exists), `enumerateDomainIDs` (the cleanup job) and `common.ResolveProjectRef` (shared by three services) have no service to hang a method on. A method rule would have needed exceptions for exactly those.
- The call-site churn is the same either way: `helper(ctx, …)` becomes `helper(ctx, svc.pool, …)`.

**Alternatives considered**: Methods on the owning service. Rejected for the three receiver-less callers above, and because a mixed rule costs more review attention than it saves keystrokes.

## R4. The `internal/database` API

**Decision**:

| Today | After |
|---|---|
| `var DB *pgxpool.Pool` | deleted |
| `Connect(ctx, url, cfg) error` — sets `DB` | `Connect(ctx, url, cfg) (*pgxpool.Pool, error)` — same parse, config and ping-on-connect; the caller owns `Close` |
| `WithTx(ctx, fn)` | `WithTx(ctx, pool, fn)` (ADR-001 §4) |
| `HealthCheck(ctx)` | `HealthCheck(ctx, pool)` — keeps the nil guard and its error text |
| `Close()`, `Stats()`, `ConnectSimple()` | deleted. `Close` becomes `pool.Close()` at its owners; the other two have **no callers** |
| `PoolConfig`, `DefaultPoolConfig`, the migrate functions | unchanged (the migrate functions take a URL and never touched the handle) |

**Rationale**:

- `Connect` keeps the one thing it adds over `pgxpool.NewWithConfig`: the configured pool plus a ping, so a bad URL still fails at startup with the same message (spec US1 scenario 2).
- Keeping the nil guard in `HealthCheck` keeps readiness's "not initialized" answer.
- Deleting the dead helpers is FR-006's clean break.

**Alternatives considered**:

- Keeping `database.Close()` taking a pool. Rejected because it only wraps `pool.Close()`.
- Keeping `Stats(pool)` for observability. Rejected because nothing calls it today; `pool.Stat()` is one call away if a metrics endpoint ever wants it.

## R5. Process wiring

**Decision**:

- In `cmd/lightstack/main.go`: `pool, err := database.Connect(…)`, fatal on error with today's message, then `defer pool.Close()` in place of `defer database.Close()` (`:101-104`).
- `health.DatabaseCheck(pool)` (`internal/health/health.go:41-46`), and `keystone.BootstrapAdmin(ctx, pool, cfg.Keystone)` (`:120`).
- Every constructor in R2 receives the same `pool`.

**Rationale**: The deferred close runs when `main` returns. That is after the shutdown sequence: drain, stop the background goroutines, then `srv.Shutdown` for every listener. So the close order is today's (FR-004, US1 scenario 5). `log.Fatalf` paths skip the defer today and still do.

**Alternatives considered**: An explicit `pool.Close()` after the shutdown loop. Equivalent, but it adds a line the defer already covers.

## R6. The `cleanup` job

**Decision**:

- `teardown.Run` keeps its own pool: `pool, connErr := database.Connect(ctx, cfg.Database.URL, database.DefaultPoolConfig())`.
- On error it takes the unchanged host-only path. On success it calls `defer pool.Close()`, then `enumerateDomainIDs(ctx, pool)` and `neutron.NewService(pool, netMode, nil)`. Today's code is at `internal/teardown/teardown.go:84-97`.

**Rationale**: The job is a separate process mode, so it owns its pool's whole lifecycle. US1 scenario 4 requires both of its paths to behave as before.

## R7. Shared quota-target helper (review r4148510424)

**Decision**: `common.QuotaTargetProject(c, pool)` and `common.ResolveProjectRef(ctx, pool, ref)`. The 11 callers pass `svc.pool`. `internal/common` then imports `pgxpool`, not `internal/database`.

**Rationale**: FR-012. The helper keeps its single shared implementation; only where its pool comes from changes.

## R8. Tests

**Decision**:

- **Package test pools**: each package's `TestMain` assigns `testPool, err = database.Connect(…)` in place of setting the handle. The `TestMain`s are in `cinder/volumes_db_test.go`, `glance/images_db_test.go`, `keystone/maindb_test.go`, `metadata/service_db_test.go`, `neutron/network_db_test.go` and `nova/maindb_test.go`. `testPool.Close()` replaces `database.Close()`. `testDBAvailable` and `requireDB` keep their meaning: without `LIGHTSTACK_TEST_DB_URL` the DB-backed tests skip, and with it set a connect failure is fatal.
- **Fixtures and routers**: 184 test references across 29 files become `testPool.…`. Test routers pass `testPool` to the constructors. Database-free tests pass `nil`, exactly as the handle was nil for them.
- **Others**: `health_test.go` calls `DatabaseCheck(nil).Name`. `test/benchmark/database_bench_test.go` opens a pool per benchmark and closes it.

**Rationale**: This is the spec assumption: one shared test pool per test package; the demonstrator is the exception (FR-009). A nil pool reaching a query panics, just as the nil handle does today, so the edge case "fails loudly, never silently" keeps today's behavior.

**Alternatives considered**: A pool per test. Rejected: it means hundreds of extra connections and a slower suite, and proves nothing the demonstrator doesn't.

## R9. The Keystone demonstrator (FR-008)

**Decision**: A new `internal/keystone/pool_injection_test.go`:

- It skips without `LIGHTSTACK_TEST_DB_URL`. Otherwise it opens **its own** pool with `database.Connect` and closes it in `t.Cleanup`. It never reads `testPool`.
- It constructs `NewAuthService(pool, …)` and `NewService(pool, auth, nil)`, and registers the routes behind a stub auth function, as `identity_surface_test.go` does.
- It then exercises one database-backed path per pool holder:
  - `POST /v3/auth/tokens` for the seeded `admin`/`secret` scoped to `default`, which goes through the `AuthService` (expect 201 and an `X-Subject-Token`);
  - `GET /v3/projects/{seeded default project id}` with an admin stub, which goes through the `Service` (expect 200).

**RED**: Written before Keystone is converted, the test does not compile, because neither constructor takes a pool. GREEN comes with Keystone's step. After the handle is deleted, the compiler guarantees no other pool exists (Clarifications 2026-10-01).

**Rationale**: It covers both Keystone pool holders. It needs nothing beyond the seeded data (migration `002_seed_data`), and it runs on its own: `go test -run TestServiceRunsOnItsOwnPool ./internal/keystone/` (SC-004).

## R10. Step order (FR-013) and the transition

**Decision**: One working tree and one commit (Clarifications 2026-10-01). The steps below each end with `go build ./... && go vet ./...` and the touched packages' unit tests. A later step never depends on an unconverted earlier one.

0. Record the baseline (R11) on the unmodified tree.
1. `internal/database`: change `Connect`, `WithTx`, `HealthCheck` and delete the dead helpers. `main`, `teardown`, the six `TestMain`s, the benchmark and `health` all adopt the returned pool. **Transition only**: `Connect` still assigns the handle, and the 7 `WithTx` call sites pass `database.DB`, so the unconverted packages keep working. Both are gone by step 11.
2. Write the Keystone demonstrator. **RED**: the package doesn't compile, and the only reason is that neither Keystone constructor takes a pool yet.
3. `compute` → 4. `metadata` → 5. `glance` → 6. `keystone` (demonstrator **GREEN**) → 7. `cinder` → 8. `neutron` (with the `teardown` wiring that constructs it) → 9. `nova`.
10. `common`: `QuotaTargetProject(c, pool)` and its 11 call sites → `svc.pool`.
11. Delete `var DB` and the transition line in `Connect`. The compiler must report nothing.
12. Docs (R13), then the full gate set (R11).

**Rationale**: This is ADR-001's smallest-first order. The database step comes before the demonstrator so that the demonstrator's RED has exactly one cause. `teardown` moves with `neutron` because it constructs a Neutron service. `common` goes last because its callers live in the three largest packages. The transition scaffolding exists only between steps and never reaches the commit (FR-006).

## R11. Gates and the baseline

**Decision**: Record each of these on `44ed3d1` before step 1, and repeat after step 12:

| Gate | Command | Environment |
|---|---|---|
| Unit, DB-backed | `LIGHTSTACK_TEST_DB_URL=… go test -race -count=1 -p 1 $(go list ./... \| grep -v /test/contract)` | A fresh test database. `-p 1` rules out the known parallel-package seeded-data flake, so the "identical" comparison is not noisy. CI's own invocation (no `-p 1`) is run once more at the end. |
| Unit, no DB | `env -u LIGHTSTACK_TEST_DB_URL go test -count=1 -p 1 -v $(go list ./... \| grep -v /test/contract)`: per-package `ok`/`FAIL` plus the `--- SKIP` count | The DB-backed tests skip. The after-run has exactly one more skip than the baseline: the demonstrator. |
| Lint | `make lint` | `unused` included |
| Contract | `make test-contract`: passed/failed/skipped counts | Compose stack built from this worktree |
| CLI | `./test/quick_test.sh` | same stack |
| Horizon UI probe | `make test-horizon-ui` | same stack |
| Shutdown | `docker compose … stop lightstack`, then the log tail and the container's exit code | The log runs from `Shutting down servers...` to `Lightstack stopped`, with no pool errors, and the exit code is 0 |
| Acceptance search | `git grep -n 'database\.DB\b' -- '*.go'` | must be empty after step 11 |

`make db-up` and the compose stack both create a container named `lightstack-postgres` on port 5432, so the unit run and the stack runs happen one after the other. Alternatively, the unit run uses a `lightstack_test` database on the stack's server, which is what CI does.

**Real mode**: the constitution's merge gate asks for both modes to be tested "if applicable". It is applicable here. The VXLAN coordinator, the node registry, the network reconciler, the cleanup job's real-mode purge and the libvirt-path handlers all change, and none of them run in stub mode. The plan therefore adds a short real-mode smoke on the host: server create with a network, volume attach, delete, and `lightstack cleanup --dry-run`. The owner runs it from `HOST_TESTING_REAL_MODE.md`.

## R12. Inventory method

- References: `git grep -o 'database\.DB\b' HEAD -- 'internal/*.go' 'pkg/*.go' 'cmd/*.go' ':!*_test.go' | wc -l` gives 690 in 60 files. The same over `'*_test.go'` gives 184 in 29 files.
- Methods versus package functions: an `awk` pass tracking the enclosing top-level `func` gives 667 in methods and 23 in 18 package-level functions.
- `svc.pool`: 0 hits. Only `internal/database/db.go` imports `pgxpool`.

## R13. Documentation (FR-011)

| File | Today | Change |
|---|---|---|
| `CLAUDE.md:208` | describes the injected pattern as the convention | point at a real constructor (e.g., `nova.NewService`) |
| `CLAUDE.md:363` | "Database Queries" idiom shows `database.DB.QueryRow` | `svc.pool.QueryRow` |
| `docs/ARCHITECTURE.md:213-231` | constructor diagram with signatures that never existed (`metadata.NewService(pool)`, keystone missing `NewService`) | match R2 exactly |
| `docs/TROUBLESHOOTING.md:102` | sample uses `database.DB.Exec` | `svc.pool.Exec` |
| `docs/STUB_PLACEHOLDERS.md:429` | sample uses `database.DB.QueryRow` | `svc.pool.QueryRow` |
| `docs/WHATS_LEFT.md:39` | H-3 open, "661 sites" | resolved |
| `docs/adr/ADR-001-database-dependency-injection.md` | Accepted | add an implementation note (SPEC-027) |

Historical documents keep their wording as records of their time (SC-005): `docs/superpowers/**`, `docs/review/**`, `docs/archive/**`, earlier specs, and ADR-001's Context section.
