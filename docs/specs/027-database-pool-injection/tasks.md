---
description: "Task list for 027-database-pool-injection"
---

# Tasks: Inject the database pool through service constructors (ADR-001)

**Input**: Design documents from `docs/specs/027-database-pool-injection/`
**Prerequisites**: [plan.md](plan.md), [spec.md](spec.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/go-signatures.md](contracts/go-signatures.md), [quickstart.md](quickstart.md)

**Tests**: The spec asks for exactly one new test, the Keystone demonstrator (FR-008), written first and RED. Every other test change converts an existing test to the injected pool. The existing suites are the behavioral oracle (Clarifications 2026-10-01).

**Delivery**: One working tree, **one commit** at the end. **Never commit**: the branch owner reviews and commits. No task references code outside this branch.

**Organization**: Phases follow the dependency order. US2 converts the four smallest packages, Keystone included, so ADR-001's smallest-first order holds across phases (FR-013). US1 is the no-change gate, so it runs after the code it verifies.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an unfinished task)
- **[Story]**: US1 (no behavior change), US2 (test with your own pool), US3 (every dependency explicit)

## Conventions used by every task

- `TESTDB='postgres://lightstack:secret@localhost:5432/lightstack_test?sslmode=disable'`. This is a separate database on the compose stack's PostgreSQL, the same layout CI uses. Recreate it before each full unit run with `docker compose -f deployments/docker-compose.yml exec postgres dropdb -U lightstack --if-exists lightstack_test` followed by `… createdb -U lightstack lightstack_test`.
- **Package conversion**:
  - In methods, `database.DB` becomes `<receiver>.pool`.
  - Package-level helpers gain a pool parameter (`(ctx, pool, …)`, or `pool` first when there is no `ctx`), per research R3. Their callers pass `svc.pool`.
  - Constructors match [contracts/go-signatures.md](contracts/go-signatures.md).
  - A package whose last reference is gone drops its `internal/database` import.
  - Comments that name `database.DB` are updated too, since SC-001's search counts them.
- **Step check**: `go build ./... && go vet ./...`, then the touched packages' tests with `LIGHTSTACK_TEST_DB_URL="$TESTDB" go test -count=1 -p 1 ./internal/<pkg>/...`.
- Scratch output (baselines, coverage, commit message) goes under `/tmp/027-*`, never into the repository.

---

## Phase 1: Setup (baseline on the unmodified tree)

**Purpose**: Capture the oracle that US1 compares against, before any code changes (FR-010).

- [X] T001 Inspect the running containers with `docker ps`. On 2026-10-01, `lightstack`, `lightstack-postgres` and `lightstack-redis` were up, built from an unknown tree. **Ask the user before replacing them.** No repository change.
- [X] T002 Bring the compose stack up from this worktree at `44ed3d1` with `docker compose -f deployments/docker-compose.yml up -d --build`. Wait for `curl -s http://localhost:35357/v3` to answer, then create `lightstack_test` per Conventions.
- [X] T003 Record the unit and lint baseline into `/tmp/027-baseline-unit.txt`. Run `LIGHTSTACK_TEST_DB_URL="$TESTDB" go test -race -count=1 -p 1 $(go list ./... | grep -v /test/contract) 2>&1 | grep -E '^(ok|FAIL|---|\?)'`, then `make lint`. If lint reports paths under a deleted worktree, run `go tool golangci-lint cache clean` and repeat. Then run the same packages **without** a test database, using `env -u LIGHTSTACK_TEST_DB_URL go test -count=1 -p 1 -v $(go list ./... | grep -v /test/contract) 2>&1`. Record its per-package `ok`/`FAIL` lines and its count of `--- SKIP` lines in `/tmp/027-baseline-nodb.txt`.
- [X] T004 Record the stack baseline into `/tmp/027-baseline-stack.txt`:
  - `make test-contract`: passed/failed/skipped counts, plus the list of failing tests;
  - `./test/quick_test.sh`: exit code and summary;
  - `make test-horizon-ui`: summary;
  - `docker compose -f deployments/docker-compose.yml exec lightstack /app/lightstack cleanup --config /app/config/lightstack.yaml --dry-run`: output, once with PostgreSQL up and once with `postgres` stopped, restarting `postgres` afterwards;
  - shutdown: `docker compose -f deployments/docker-compose.yml stop lightstack`, then `… logs --tail 30 lightstack` (the sequence through `Lightstack stopped`, with no pool errors) and `docker inspect -f '{{.State.ExitCode}}' lightstack`. Start it again with `… start lightstack`.
- [X] T005 Record the acceptance-search baseline into `/tmp/027-baseline-grep.txt` with `git grep -n 'database\.DB\b' -- '*.go' | wc -l`. The expected value is 874.

**Checkpoint**: Baselines saved; the tree is unchanged (`git status` shows only the spec folder and `CLAUDE.md`).

---

## Phase 2: Foundational (database API and every pool owner)

**Purpose**: Change the `internal/database` API and adopt the returned pool at every owner, so the packages can then convert one by one.

**⚠️ CRITICAL**: Blocks all user-story phases. **Transition scaffolding** (removed in T028): `Connect` still assigns the handle, and the 7 `WithTx` call sites pass `database.DB`.

- [X] T006 In `internal/database/db.go`:
  - `Connect(ctx, connString, poolConfig)` returns `(*pgxpool.Pool, error)`, with the same parse, config and ping, and the same error texts. As transition scaffolding it still assigns `DB = pool`.
  - `HealthCheck(ctx, pool)` keeps the nil guard and its `database connection not initialized` message.
  - Delete `Close`, `Stats` and `ConnectSimple`.
- [X] T007 In `internal/database/tx.go`, change to `WithTx(ctx, pool, fn)`. Pass `database.DB` as transition scaffolding at the 7 call sites: `internal/cinder/volumes.go:1707,1921`, `internal/neutron/security_group_default.go:39`, `internal/neutron/ports.go:1440`, `internal/nova/quotas.go:440` and `internal/nova/handlers.go:577,2335`.
- [X] T008 [P] In `internal/health/health.go:41-46`, change to `DatabaseCheck(pool)` (the probe calls `database.HealthCheck(ctx, pool)`). In `internal/health/health_test.go:181`, use `DatabaseCheck(nil).Name`.
- [X] T009 In `cmd/lightstack/main.go:101-110`, change to `pool, err := database.Connect(ctx, cfg.Database.URL, poolConfig)` with the same `log.Fatalf("Failed to connect to database: %v", err)`. Replace `defer database.Close()` with `defer pool.Close()`, and use `health.New(zlog.Logger, health.DatabaseCheck(pool))`.
- [X] T010 In `internal/teardown/teardown.go:84-97`, change to `pool, connErr := database.Connect(...)`. On error the host-only path is unchanged. On success, `defer pool.Close()` and call `enumerateDomainIDs(ctx, pool)`; its 1 reference becomes `pool`. `neutron.NewService` stays as it is until T023.
- [X] T011 Point each of the six `TestMain`s at a package-level `testPool *pgxpool.Pool` assigned from `database.Connect`, and close it with `testPool.Close()`. `testDBAvailable` and `requireDB` stay as they are. The files are `internal/cinder/volumes_db_test.go`, `internal/glance/images_db_test.go`, `internal/keystone/maindb_test.go`, `internal/metadata/service_db_test.go`, `internal/neutron/network_db_test.go` and `internal/nova/maindb_test.go`.
- [X] T012 [P] In `test/benchmark/database_bench_test.go`, each benchmark opens its own pool with `database.Connect` and calls `defer pool.Close()`. Its 6 references become `pool`.
- [X] T013 Step check: `go build ./... && go vet ./...` is clean, and `go test -count=1 ./internal/health/... ./internal/teardown/... ./cmd/...` is green.

**Checkpoint**: Every owner holds a pool, and the build is green with the handle still in place.

---

## Phase 3: User Story 2 — test a service against a database you supply (Priority: P1) 🎯 MVP

**Goal**: The four smallest packages take the pool. The Keystone demonstrator proves the payoff (FR-008, SC-004).

**Independent Test**: `LIGHTSTACK_TEST_DB_URL="$TESTDB" go test -count=1 -run TestServiceRunsOnItsOwnPool -v ./internal/keystone/` passes on a pool the test opened itself.

### Test for User Story 2 (write first, confirm RED)

- [X] T014 [US2] Write `internal/keystone/pool_injection_test.go` with `TestServiceRunsOnItsOwnPool`, per research R9:
  - Skip when `LIGHTSTACK_TEST_DB_URL` is unset. Open a pool with `database.Connect` and close it in `t.Cleanup`. Never read `testPool`.
  - Construct `NewAuthService(pool, "pool-injection-secret", time.Hour, nil)` and `NewService(pool, auth, nil)`, and register the routes behind a stub auth function, as `identity_surface_test.go` does.
  - `POST /v3/auth/tokens`: password `admin`/`secret`, scoped to project `default` → `201` with an `X-Subject-Token`.
  - `GET /v3/projects/00000000-0000-0000-0000-000000000002` with an `admin` stub → `200`, and the body names `default`.
  - **Confirm RED**: `go vet ./internal/keystone/` fails with "too many arguments", because neither constructor takes a pool yet.

### Implementation for User Story 2

- [X] T015 [US2] `compute`:
  - In `internal/compute/node_registry.go`, add a `pool` field and `NewNodeRegistry(pool, nodeID, tunnelIP, heartbeatInterval)`. Its 3 references become `nr.pool`.
  - Update the call site in `cmd/lightstack/main.go`.
  - Run the step check.
- [X] T016 [US2] `metadata`:
  - In `internal/metadata/service.go`, add a `pool` field and `NewService(pool, bindAddr)`. Its 10 references become `svc.pool`.
  - In `internal/metadata/service_db_test.go`, the 5 references become `testPool`, and `NewService(":0")` becomes `NewService(testPool, ":0")`.
  - Update `cmd/lightstack/main.go` and run the step check.
- [X] T017 [US2] `glance`:
  - Add a `pool` field and `NewService(pool, mode, cephPool, cephConf, s3opts, cache)` in `internal/glance/images.go`.
  - 54 references become `svc.pool` across `cache.go`, `images.go`, `import.go`, `metadefs.go` and `tasks.go`.
  - In `internal/glance/images_db_test.go`, 13 references become `testPool`, and the constructor gains the pool.
  - Update `cmd/lightstack/main.go` and run the step check.
- [X] T018 [US2] `keystone`, production code:
  - `AuthService`: a `pool` field and `NewAuthService(pool, …)`. Its 16 method references become `s.pool`, in `auth.go`.
  - `Service`: a `pool` field and `NewService(pool, authService, cache)`, in `handlers.go`. Its 80 references become `svc.pool`, across `application_credentials.go`, `auth.go`, `credentials.go`, `domains.go`, `ec2_credentials.go`, `groups.go`, `handlers.go`, `s3tokens.go` and `services.go`.
  - `BuildServiceCatalog(pool, projectID, cache, s3Endpoint)`: its callers at `auth.go:407,612,778` pass `s.pool`.
  - `BootstrapAdmin(ctx, pool, cfg)`, with `createAdmin`, `grantAdminRole`, `seedCustomAdmin` and `seedDefaultAdmin` taking the pool, in `bootstrap.go`.
  - Update `cmd/lightstack/main.go:120,142,145`.
- [X] T019 [US2] `keystone` tests:
  - 57 references become `testPool`, across `auth_rgw_compat_test.go`, `bootstrap_concurrency_test.go`, `bootstrap_test.go`, `ec2_credentials_test.go`, `identity_notfound_test.go`, `identity_patch_readback_test.go`, `maindb_test.go` and `s3tokens_disabled_test.go`.
  - Every `NewService`, `NewAuthService`, `BootstrapAdmin` and `BuildServiceCatalog` call in `internal/keystone/*_test.go` gets a pool: `testPool` when the test is DB-backed, `nil` when it is DB-free. This includes `identity_surface_test.go`, `routes_wiring_test.go` and `auth_catalog_test.go`.
- [X] T020 [US2] Step check for `compute`, `metadata`, `glance` and `keystone`. The demonstrator is **GREEN** when run on its own, per the Independent Test above.

**Checkpoint**: US2 is delivered. Four packages use the injected pool, and the rest still run through the scaffolding.

---

## Phase 4: User Story 3 — every component declares its database dependency (Priority: P2)

**Goal**: The remaining packages convert, the handle is deleted, and the docs describe what exists (FR-001…FR-003, FR-011, FR-012).

**Independent Test**:
- `git grep -n 'database\.DB\b' -- '*.go'` is empty.
- Every constructor matches [contracts/go-signatures.md](contracts/go-signatures.md).
- quickstart §5's docs search is empty.

### Implementation for User Story 3

- [X] T021 [US3] `cinder`, production code:
  - Add a `pool` field and `NewService(pool, mode, cephPool, cephConf)` in `internal/cinder/volumes.go`.
  - 111 method references become `svc.pool`, across `backups.go`, `groups.go`, `manage.go`, `qos_specs.go`, `quotas.go`, `transfers.go`, `volumes.go`, `volume_type_access.go` and `volume_types.go`.
  - `cinderQuotaUsage` and `resolveCinderQuotaLimits` take `(ctx, pool, …)`. Their callers are at `quotas.go:131,142,226` and `volumes.go:2095`.
  - The `WithTx` sites at `volumes.go:1707,1921` pass `svc.pool`.
  - Update `cmd/lightstack/main.go`.
- [X] T022 [US3] `cinder` tests: 29 references become `testPool`, across `quotas_test.go`, `quota_usage_failure_test.go` and `volumes_db_test.go`, and every constructor call gains a pool. Run the step check.
- [X] T023 [US3] `neutron`, production code:
  - In `internal/neutron/network.go`, add a `pool` field and `NewService(pool, mode, cache)`.
  - In `vxlan_coordinator.go`, `NewVXLANCoordinator(pool, …)`; its 6 references become `vc.pool`.
  - In `reconciler.go`, the 4 `NetworkReconciler` references become `r.svc.pool`.
  - 174 `Service` references become `svc.pool`, across `address_scopes.go`, `agents.go`, `auto_allocated_topology.go`, `floatingip.go`, `metering.go`, `network.go`, `network_ip_availability.go`, `port_forwarding.go`, `ports.go`, `qos.go`, `rbac.go`, `reconciler.go`, `router.go`, `subnet_pools.go`, `teardown.go` and `trunks.go`.
  - `listSecurityGroupRows`, `securityGroupOwnedByCaller`, `securityGroupRulesByGroup`, `queryStrings` and `EnsureDefaultSecurityGroup` (it reaches the database through `WithTx`) take `(ctx, pool, …)`. Their callers pass `svc.pool`, including Nova's at `nova/handlers.go:519,541`, and the 4 Nova test calls pass `testPool`.
  - The `WithTx` sites at `security_group_default.go:39` and `ports.go:1440` pass `svc.pool`.
  - Update `cmd/lightstack/main.go` (`NewService`, `NewVXLANCoordinator`) and `internal/teardown/teardown.go` (`neutron.NewService(pool, netMode, nil)`).
- [X] T024 [US3] `neutron` tests:
  - 39 references become `testPool`, across `listing_page_size_test.go`, `network_db_test.go`, `network_ownership_test.go`, `network_selectability_test.go`, `quota_target_test.go`, `quota_usage_failure_test.go`, `security_group_rule_authz_test.go` and `subnet_ownership_test.go`.
  - Every constructor call gains a pool, `teardown_test.go` included.
  - Run the step check for `neutron` and `teardown`.
- [X] T025 [US3] `nova`, production code:
  - Add a `pool` field and `NewService(pool, libvirtURI, libvirtMode, cache)` in `internal/nova/handlers.go`.
  - 209 method references become `svc.pool`, across `advanced_actions.go`, `aggregates.go`, `console.go`, `diagnostics.go`, `flavors.go`, `handlers.go`, `interface_attach.go`, `keypairs.go`, `migrations.go`, `quotas.go`, `servergroups.go`, `server_update.go`, `tags.go`, `tenant_usage.go` and `volume_attachment.go`.
  - `securityGroupNamesForServer`, `securityGroupNamesForServers`, `allocateNextIP`, `countUsage` and `resolveQuotaLimits` take `(ctx, pool, …)`, and `CheckQuota(c, pool, …)` (through `WithTx`) does too. Their callers pass `svc.pool`, including `CheckQuota` at `handlers.go:433,444,455`.
  - The `WithTx` sites at `quotas.go:440` and `handlers.go:577,2335` pass `svc.pool`.
  - Update `cmd/lightstack/main.go`.
- [X] T026 [US3] `nova` tests:
  - 35 references become `testPool`, across `boot_from_volume_test.go`, `quotas_test.go`, `quota_target_test.go`, `quota_usage_failure_test.go`, `security_group_ref_test.go`, `server_detail_secgroups_test.go` and `spec017_helpers_test.go`.
  - `newNovaTestRouter` in `testrouter_test.go` passes `testPool`, and every other constructor or helper call gains a pool.
  - Run the step check.
- [X] T027 [US3] `common`:
  - In `internal/common/project.go`, `ResolveProjectRef(ctx, pool, ref)` and `QuotaTargetProject(c, pool)`. The import changes from `internal/database` to `pgxpool`, and the doc comment says where the pool comes from.
  - The 11 call sites pass `svc.pool`: `internal/nova/quotas.go:26,143,186,312`, `internal/cinder/quotas.go:100,123,175,251` and `internal/neutron/network.go:371,386,399`.
  - In `internal/common/project_test.go`, call `QuotaTargetProject(c, nil)`. A nil pool makes the test's claim, that these paths never reach the database, literally enforced.
  - Run the step check for `common`, `nova`, `cinder` and `neutron`.
- [X] T028 [US3] Delete `var DB` and the transition assignment in `Connect` from `internal/database/db.go`. Then:
  - `go build ./... && go vet ./...` is clean;
  - `git grep -n 'database\.DB\b' -- '*.go'` prints nothing;
  - `git grep -n 'DB = pool' -- internal/database` prints nothing.

### Documentation for User Story 3

- [X] T029 [P] [US3] `CLAUDE.md:208`: the DI paragraph points at a real constructor, `nova.NewService(pool, …)` in `internal/nova/handlers.go`. `CLAUDE.md:363`: the "Database Queries" idiom uses `svc.pool.QueryRow`.
- [X] T030 [P] [US3] `docs/ARCHITECTURE.md:213-231`: the constructor diagram uses the exact signatures from [contracts/go-signatures.md](contracts/go-signatures.md), with `keystone.NewService`, `compute.NewNodeRegistry`, `neutron.NewVXLANCoordinator` and `health.DatabaseCheck` added and `metadata.NewService(pool, bindAddr)` corrected.
- [X] T031 [P] [US3] Change the code samples at `docs/TROUBLESHOOTING.md:102` and `docs/STUB_PLACEHOLDERS.md:429` to `svc.pool`.
- [X] T032 [P] [US3] `docs/WHATS_LEFT.md:39`: mark H-3 resolved. In `docs/adr/ADR-001-database-dependency-injection.md`, add an implementation note (implemented by SPEC-027 `docs/specs/027-database-pool-injection/`). Leave the Context section as it is, since it is a record.
- [X] T033 [US3] Verify the docs: quickstart §5's search prints nothing. List every database-using constructor with `grep -rn --include='*.go' -E '^func New' internal | grep -v _test.go` and check each against the contract (FR-002). Then list every construction in the two owners with `grep -n -E '\.(New[A-Za-z]*|BootstrapAdmin|DatabaseCheck)\(' cmd/lightstack/main.go internal/teardown/teardown.go`, and confirm each database-using one is passed `pool`, never `nil` (contract invariant 5).

**Checkpoint**: US3 is delivered. No handle, every dependency explicit, docs true.

---

## Phase 5: User Story 1 — nothing changes for anyone running or calling Lightstack (Priority: P1)

**Goal**: Prove that the end state behaves exactly like the baseline (FR-005, FR-010, SC-003). This runs last because it verifies the finished tree.

**Independent Test**: Every result below equals its Phase 1 baseline.

- [X] T034 [US1] Run the unit tests and lint after the change. Recreate `lightstack_test`, rerun T003's command into `/tmp/027-after-unit.txt` and `diff` it against `/tmp/027-baseline-unit.txt`: each package gives the same `ok`/`FAIL`, and the only new test is `TestServiceRunsOnItsOwnPool`. Rerun T003's no-database command into `/tmp/027-after-nodb.txt`. It gives the same per-package results and exactly one more `--- SKIP` than the baseline, which is the demonstrator. `make lint` reports `0 issues`.
- [X] T035 [US1] Run the CI-equivalent unit command once: `LIGHTSTACK_TEST_DB_URL="$TESTDB" go test -race -coverprofile=/tmp/027-coverage.txt -covermode=atomic $(go list ./... | grep -v /test/contract)`. It is green.
- [X] T036 [US1] Rebuild the stack from this worktree with `docker compose -f deployments/docker-compose.yml up -d --build`. Rerun T004's four checks into `/tmp/027-after-stack.txt` and compare: the contract counts and the failing-test list are identical, `quick_test.sh` and the Horizon probe give the same results, and the cleanup dry-run takes the same path in both states.
- [X] T037 [US1] Check fail-fast and readiness, per quickstart §3. The binary, started against a closed database port, exits 1 with `Failed to connect to database:`, the same as the baseline binary built from `44ed3d1`. `/readyz` returns `200` while the database is up and `503` with `database unavailable` once it stops. The shutdown check from T004 gives the same log sequence and exit code `0` (US1 scenario 5).
- [X] T038 [US1] Write the real-mode smoke runbook `docs/specs/027-database-pool-injection/HOST_TESTING_REAL_MODE.md` (the stub-mode gates are in `HOST_TESTING.md`): a server create with a network, a volume attach and detach, a delete, and `lightstack cleanup --dry-run`, each expected to behave as before. Generate it with the host-testing workflow. **The user runs it** on the real-mode host (constitution: both modes).

**Checkpoint**: US1 is satisfied in stub mode. Real mode waits on the user's host run.

---

## Phase 6: Polish & handover

- [X] T039 Final review of the tree:
  - `gofmt -l $(git ls-files '*.go')` prints nothing.
  - `git grep -n -E 'database\.DB\b|DB = pool' -- '*.go'` prints nothing.
  - `git diff --stat` shows only this feature's files.
  - No scratch file sits inside the repository.
- [X] T040 Draft the single commit message into `/tmp/027-commit-msg.txt` for the user. Use Conventional Commits, e.g. `refactor(database): inject the pool through service constructors (ADR-001)`, mention cloud-storage#554, and reference no other PR. **Do not commit.**
- [ ] T041 Draft the reply to review thread r4148510424, saying what was actually done, and show it to the user. Post it only on their approval, after their commit is pushed.
- [ ] T042 On the user's approval only:
  - add `Closes cloud-storage#554` to PR #79's description;
  - tick the finished items in the `## Checklist` comment on cloud-storage#554.

---

## Dependencies & Execution Order

### Phase dependencies

- **Setup (Phase 1)** → **Foundational (Phase 2)** → **US2 (Phase 3)** → **US3 (Phase 4)** → **US1 (Phase 5)** → **Polish (Phase 6)**.
- The phases are strictly sequential:
  - every package step edits `cmd/lightstack/main.go`;
  - US3's `common` step (T027) needs all three of its caller packages converted;
  - T028 needs every package converted;
  - US1 verifies the finished tree.

### Story dependencies

- **US2** depends on Foundational only.
- **US3** depends on US2. The scaffolding would let it start after Foundational, but smallest-first order (FR-013) puts it after US2.
- **US1** depends on US2 and US3. It verifies their union, and its baseline is in Setup.

### Within a phase

- A package's test task follows its production task, because the constructor signature must exist first.
- T014 (RED) precedes T015–T019, and T020 confirms GREEN.

### Parallel opportunities

- T008 ∥ T012, in Foundational.
- T029 ∥ T030 ∥ T031 ∥ T032, the docs, after T028.
- Nothing else. The shared `main.go` and the fixed step order make the rest sequential by design.

## Parallel Example: US3 documentation

```bash
# After T028, the four doc tasks touch disjoint files:
Task: "T029 CLAUDE.md:208 and :363"
Task: "T030 docs/ARCHITECTURE.md:213-231"
Task: "T031 docs/TROUBLESHOOTING.md:102 + docs/STUB_PLACEHOLDERS.md:429"
Task: "T032 docs/WHATS_LEFT.md:39 + ADR-001 note"
```

## Implementation Strategy

### MVP first (US2)

1. Phase 1: the baseline.
2. Phase 2: the database API and its owners.
3. Phase 3: the demonstrator RED, then compute → metadata → glance → keystone, then the demonstrator GREEN.
4. **Stop and validate**: SC-004 holds, and the four packages' tests are green.

### Incremental delivery inside one commit

- Every step leaves the build green (step check), so a failure points at the step that caused it (FR-013).
- The transition scaffolding from T006/T007 is removed in T028, so the commit carries none of it (FR-006).
- The only commit is the user's, after T039–T040.

## Notes

- 42 tasks: Setup 5, Foundational 8, US2 7, US3 13, US1 5, Polish 4.
- Counts quoted in tasks are from `44ed3d1` (research R12). The compiler is the authority if a count drifts.
- Outward-facing steps (T001's replacement of running containers, T041's reply, T042's PR and issue edits) wait for the user's explicit go.
