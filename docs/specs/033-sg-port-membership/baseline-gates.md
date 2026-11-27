# Baseline gates — SPEC-033

T001 records where the gates stand before any change, so that T051 can compare (SC-007, SC-008). Nothing was fixed here.

**When**: 2026-11-18. **Code**: `bugfix/579` at `d81f081`, the `main` commit it is based on. The working tree differs
only in documents: the constitution, `CLAUDE.md` and this spec directory. **Host**: the openSUSE development host, with
podman behind the `docker` CLI.

## How the gates were run

| Gate | Command |
|---|---|
| Build | `make build` |
| Lint | `make lint` |
| Unit, PostgreSQL | `LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable' go test -count=1 -p 1 -json $(go list ./... \| grep -v '/test/contract')` |
| Unit, SQLite | the same with `LIGHTSTACK_TEST_DB_URL='sqlite:///tmp/ls-test/t.db'`, on a fresh directory |
| Contract | `go test -count=1 -json ./test/contract/... -timeout 10m`, with `LIGHTSTACK_DB_URL` naming the stack's database, against a running stub Lightstack, once per backend |
| Bash | `test/security_groups_test.sh`, `test/neutron_patch_test.sh`, against the PostgreSQL stack, with `OS_AUTH_URL=http://localhost:35357/v3` |

Notes on the method:
- **The flags.** `-count=1`, because Go's test cache ignores `LIGHTSTACK_TEST_DB_URL`, so a cached result can come from
  another database. `-p 1`, because tests that read the seeded project race across packages that run in parallel. T051
  must use the same flags.
- **The PostgreSQL database.** It is the `make db-up` container, `lightstack-postgres`, at migration 75, which is this
  branch's newest. It held only the `default` project before the run.
- **The stack.** `bin/lightstack` ran on the host, not under Docker Compose, with `config/lightstack.yaml` and three
  overrides:
  - `state_path` under `/tmp`;
  - the metadata service trusting `X-Instance-ID`, as the compose file sets it;
  - `LIGHTSTACK_DB_URL` naming the backend: the `make db-up` database, or a fresh file
    `sqlite:///tmp/ls579/stack-sqlite.db`.

  Compose was not used because its `postgres` service has the same container name and port as the running `make db-up`
  database. Starting it would have replaced the database other worktrees use. Unlike the compose stack, this one runs
  without the Redis cache. T051 must use the same method.
- **The order.** The unit suites run before the contract suites. The Glance contract tests create import tasks for the
  seeded admin and leave them `pending` in `image_tasks`, and `internal/glance`'s `TestTaskListAndGetAreOwnerScoped`
  then counts them and fails. That happened once on this branch, after the baseline: three such rows from the PostgreSQL
  contract run, deleted again.

## Results

| Gate | Result |
|---|---|
| Build | passes (`bin/lightstack`, `v0.6.1-76-gd81f081-dirty`) |
| Lint | `0 issues` |
| Unit, PostgreSQL | 2031 passed, 0 failed, 6 skipped |
| Unit, SQLite | 2008 passed, 0 failed, 19 skipped |
| Contract, PostgreSQL | 380 passed, 0 failed, 11 skipped |
| Contract, SQLite | 353 passed, 0 failed, 12 skipped |
| `test/security_groups_test.sh` | **12 of 13**: Test 1 fails, and Test 11 is skipped without saying so (below) |
| `test/neutron_patch_test.sh` | passes |

Counts include subtests.

### Per package

| Package | Unit, PostgreSQL (pass/fail/skip) | Unit, SQLite (pass/fail/skip) |
|---|---|---|
| `cmd/lightstack` | 54/0/0 | 54/0/0 |
| `cmd/lightstack-migrate` | 5/0/0 | 5/0/0 |
| `internal/cinder` | 147/0/1 | 148/0/0 |
| `internal/common` | 185/0/0 | 185/0/0 |
| `internal/console` | 27/0/0 | 27/0/0 |
| `internal/database` | 202/0/3 | 186/0/12 |
| `internal/glance` | 60/0/0 | 58/0/2 |
| `internal/health` | 16/0/0 | 16/0/0 |
| `internal/keystone` | 267/0/0 | 259/0/5 |
| `internal/metadata` | 41/0/0 | 41/0/0 |
| `internal/middleware` | 25/0/0 | 25/0/0 |
| `internal/neutron` | 302/0/1 | 303/0/0 |
| `internal/nova` | 523/0/1 | 524/0/0 |
| `internal/teardown` | 18/0/0 | 18/0/0 |
| `pkg/hypervisor` | 79/0/0 | 79/0/0 |
| `pkg/networking` | 60/0/0 | 60/0/0 |
| `pkg/paths` | 4/0/0 | 4/0/0 |
| `pkg/storage` | 16/0/0 | 16/0/0 |

| Package | Contract, PostgreSQL | Contract, SQLite |
|---|---|---|
| `test/contract` | 5/0/9 | 5/0/9 |
| `test/contract/cinder` | 65/0/0 | 65/0/0 |
| `test/contract/glance` | 29/0/0 | 29/0/0 |
| `test/contract/keystone` | 66/0/0 | 66/0/0 |
| `test/contract/neutron` | 68/0/0 | 68/0/0 |
| `test/contract/nova` | 147/0/2 | 120/0/3 |

### Skipped tests

- **Unit, PostgreSQL**:
  - the three concurrency matrices (`internal/cinder`, `internal/neutron`, `internal/nova`);
  - in `internal/database`: `TestGenerateSQLiteBaseline`, `TestMigrationHelperProcess` and
    `TestDescribeReportsZeroWhenNotMigrated`.
- **Unit, SQLite**:
  - the PostgreSQL-only tests in `internal/database` and `internal/keystone` (the `groups.domain_id` migration);
  - `TestSchemaParity`;
  - `TestGenerateSQLiteBaseline` and `TestMigrationHelperProcess`;
  - two `internal/glance` subtests of `TestUploadFailsWhenStateCannotBeRecorded`.
- **Contract, both**:
  - the eight `TestTemplate_*` scaffolds;
  - `TestWS0_ImageNormalizesToRaw`;
  - `TestNovaCreateRefusesForeignNetwork_Contract` and `TestNovaFlavorAccessControl_Contract`.
- **Contract, SQLite only**: `TestNovaActionsByName_Contract`, with its 26 subtests. That accounts for the 27 fewer
  `nova` passes.

## Failures that predate this branch

The bash failure is not a product defect.

**`test/security_groups_test.sh`: Test 1 fails, and Test 11 does nothing.**
- The script stores the group listing in `GROUPS` (`:40`). That is a bash builtin variable, the current user's group
  ids; assigning to it has no effect. `:43` and `:209` then read the group ids instead of the listing.
- `jq` therefore never sees the listing, and Test 1 reports `Default security group not found` although `default`
  exists.
- Test 11, deleting `default`, finds no id and skips itself without a word. It is the test FR-008c's `409` would answer.

Proposed for T002, to decide with the test strategy: `test/security_groups_test.sh` renames its variable. That keeps a
gate from failing for a reason unrelated to this feature.

## Test strategy approval (T002)

**Approved by the user on 2026-11-18, as written**: research R16 and every test task in `tasks.md`. The script change
proposed above was offered as an option and not taken: `test/security_groups_test.sh` keeps its variable. The script
therefore stays as recorded here, and T051 compares against this record.

## After the change (T051)

**When**: 2026-11-18. **Code**: the working tree on `d81f081` with this feature's changes, uncommitted. The method is
the one above, with the unit suites run before the contract suites.

| Gate | Baseline | Now |
|---|---|---|
| `gofmt` / `go vet ./...` | — | clean / clean |
| Build | passes | passes |
| Lint | `0 issues` | `0 issues` |
| Unit, PostgreSQL | 2031 passed, 0 failed, 6 skipped | 2212 passed, 0 failed, 6 skipped |
| Unit, SQLite | 2008 passed, 0 failed, 19 skipped | 2186 passed, 0 failed, 22 skipped |
| Contract, PostgreSQL | 380 passed, 0 failed, 11 skipped | 394 passed, 0 failed, 11 skipped |
| Contract, SQLite | 353 passed, 0 failed, 12 skipped | 367 passed, 0 failed, 12 skipped |
| `test/security_groups_test.sh` | 12 of 13 | 12 of 13, the same lines |
| `test/neutron_patch_test.sh` | passes | passes |

No gate got worse. The three extra skips on SQLite are this feature's PostgreSQL-only lock-order tests: the copy waiting
for a change that holds the instance (`internal/neutron`), a port's deletion locking the port first
(`internal/neutron`), and a server's deletion locking the instance before its ports (`internal/nova`).

### Per package, unit

| Package | PostgreSQL (pass/fail/skip) | SQLite (pass/fail/skip) |
|---|---|---|
| `cmd/lightstack` | 54/0/0 | 54/0/0 |
| `cmd/lightstack-migrate` | 5/0/0 | 5/0/0 |
| `internal/cinder` | 147/0/1 | 148/0/0 |
| `internal/common` | 185/0/0 | 185/0/0 |
| `internal/console` | 27/0/0 | 27/0/0 |
| `internal/database` | 202/0/3 | 186/0/12 |
| `internal/glance` | 60/0/0 | 58/0/2 |
| `internal/health` | 16/0/0 | 16/0/0 |
| `internal/keystone` | 267/0/0 | 259/0/5 |
| `internal/metadata` | 41/0/0 | 41/0/0 |
| `internal/middleware` | 25/0/0 | 25/0/0 |
| `internal/neutron` | 391/0/1 | 390/0/2 |
| `internal/nova` | 557/0/1 | 557/0/1 |
| `internal/teardown` | 18/0/0 | 18/0/0 |
| `pkg/hypervisor` | 79/0/0 | 79/0/0 |
| `pkg/networking` | 118/0/0 | 118/0/0 |
| `pkg/paths` | 4/0/0 | 4/0/0 |
| `pkg/storage` | 16/0/0 | 16/0/0 |

### Per package, contract

| Package | PostgreSQL | SQLite |
|---|---|---|
| `test/contract` | 5/0/9 | 5/0/9 |
| `test/contract/cinder` | 65/0/0 | 65/0/0 |
| `test/contract/glance` | 29/0/0 | 29/0/0 |
| `test/contract/keystone` | 66/0/0 | 66/0/0 |
| `test/contract/neutron` | 74/0/0 | 74/0/0 |
| `test/contract/nova` | 155/0/2 | 128/0/3 |

### Existing tests that met the change

Some tests outside the task list relied on what the feature removes, and were changed as follows:
- **They booted without a network.** Since a server's groups are its ports' groups, these now boot on a network:
  `test/contract/nova/server_actions_test.go`'s `TestAddSecurityGroup_Contract` and `TestRemoveSecurityGroup_Contract`,
  `test/contract/nova/action_by_name_test.go`'s `TestNovaActionsByName_Contract`, and
  `test/contract/server_readback_test.go`'s `TestNovaServerReadBack_SecurityGroups_Contract`. Their assertions are
  unchanged, except that the remove test now expects `202` (T020).
- **They read `server_security_groups`.** `internal/nova/security_group_ref_test.go` now reads the resolved group from
  the boot's allocation.

### Test-run leftovers in the shared database

The PostgreSQL runs left rows in the `make db-up` database, which was cleaned up afterwards:
- the Glance contract tests' `pending` import tasks;
- the `createBackup` and `createImage` images of the Nova contract tests;
- two networks per unit run from `internal/metadata`'s `TestNetworkDataJSONToleratesNullFixedIPs`, whose cleanup runs on
  the already-cancelled `t.Context()`.

All three predate this feature.

## Mutation check (T052)

Each fix was reverted off the worktree, one at a time, through `go test -overlay`, and the named tests were run against
it. The driver applies a mutation only when its target text occurs exactly once. All 49 were caught. Two are caught by a
timeout rather than a failing assertion, because the test then blocks for ever: removing the runner's deadline (M05),
and removing the node lock's deadline (M07).

| # | Reverted | Result |
|---|---|---|
| M01 | report read from server_security_groups (T006) | caught |
| M02 | ChainMatches always true (T008) | caught |
| M03 | the payload's -F dropped (T008) | caught |
| M04 | EnsureIngressFilter restoring regardless of ChainMatches (T008) | caught |
| M05 | the runner's one-second context removed (T008, T035) | caught (timeout) |
| M06 | the node lock removed (T009) | caught |
| M07 | the node lock's deadline removed (T009) | caught (timeout) |
| M08 | the add/remove undo removed (T010) | caught |
| M09 | the add/remove undo's compare-and-set removed (T010) | caught |
| M10 | the port update undo removed (T030) | caught |
| M11 | the port update undo's compare-and-set removed (T030) | caught |
| M12 | the floating-IP create undo removed (T036) | caught |
| M13 | the floating-IP create undo's compare-and-set removed (T036) | caught |
| M14 | the floating-IP update undo's compare-and-set removed (T036) | caught |
| M15 | the rule undo removed (T037) | caught |
| M16 | the 404 for a server with no port removed (T010) | caught |
| M17 | the foreign-key violation passed through as an error (T003) | caught |
| M18 | copy-from ignored by the allocation (T025) | caught |
| M19 | copy-from ignored by the boot (T026) | caught |
| M20 | the net_id port without default (T022) | caught |
| M21 | the listing's filters ignored (T031) | caught |
| M22 | null treated as absent (T030) | caught |
| M23 | the last-group refusal restored (T019) | caught |
| M24 | the NAT before the filter (T036, T039) | caught |
| M25 | the address added before the NAT (T035) | caught |
| M26 | the address removed after the NAT (T035) | caught |
| M27 | the mark dropped from the NAT (T035) | caught |
| M28 | the rule rebuild skipped (T037) | caught |
| M29 | the in-use check removed (T038) | caught |
| M30 | the reconciler removing nothing (T039) | caught |
| M31 | the reconciler removing after a partial read (T039) | caught |
| M32 | the reconciler's orphan step after an incomplete read (T039) | caught |
| M33 | the reconciler removing unmarked rules without a record (T039) | caught |
| M34 | a filter removed while an unmarked rule still reaches it (T036, T039) | caught |
| M35 | the address left out of the pass's comparison (T039) | caught |
| M36 | a missing uplink skipped by RefreshPortFilters (T009) | caught |
| M37 | a missing uplink skipped by convergeFloatingIP (T036) | caught |
| M38 | a missing uplink skipped by the rule rebuild (T037) | caught |
| M39 | a missing uplink ignored by the reconciler (T039) | caught |
| M40 | a failed delete's restore without the port forwardings (T036) | caught |
| M41 | a failed delete's restore not applying a deleted port's SET NULL (T036) | caught |
| M42 | a port deletion leaving its floating IP associated (T036) | caught |
| M43 | a server deletion leaving its floating IP associated (T041) | caught |
| M44 | a port deletion not converging (T036) | caught |
| M45 | a server deletion not converging (T041) | caught |
| M46 | a port deletion reading the floating IPs before it locks the port (T036) | caught |
| M47 | a server deletion locking ports before the instance (T041) | caught |
| M48 | forceDelete deleting only the instance row again (T041) | caught |
| M49 | the pass reading only associated floating IPs (T039) | caught |

Two items of T052's list are not overlay mutations, because `main` is that code. Their tests were run against `main` and
failed:
- add writing `server_security_groups` only (T010, the earlier-release case):
  `TestAddSecurityGroup_EarlierReleaseServer` failed on `main`, before T018;
- create programming before its insert (T036, failure on create): `TestFloatingIPDataplane_Failures`'s create cases
  failed on `main`, which programs the NAT before the insert, before T043.

# Round 2

## Round 2 baseline (T058)

**When**: 2026-11-20. **Code**: `bugfix/579` at `1517716`. That is round 1's code (`dcda565`) plus round 2's planning:
this spec directory, the constitution and `CLAUDE.md`. **Host**: the same openSUSE development host.

The method is round 1's (above), with three changes:
- `go vet ./...`, `gofmt -l` and `helm unittest charts/lightstack` were added.
- `test/nova_server_actions_test.sh` is gone; round 1 deleted it. Of the bash scripts, only
  `test/security_groups_test.sh` runs.
- The PostgreSQL database was recreated with `make db-down && make db-up` before the run. The suites' `TestMain`
  migrated the new one to 75. The eleven `spec020-…` to `spec025…` projects it had held are fixtures the unit suites
  create on every run and keep; they came back during this run.

T160 must use the same method.

| Gate | Result |
|---|---|
| Build | passes |
| Lint | `0 issues` |
| `go vet ./...` / `gofmt -l` | clean / clean |
| Unit, PostgreSQL | 2212 passed, 0 failed, 6 skipped |
| Unit, SQLite | 2186 passed, 0 failed, 22 skipped |
| Contract, PostgreSQL | 367 passed, 0 failed, 12 skipped |
| Contract, SQLite | 367 passed, 0 failed, 12 skipped |
| `helm unittest charts/lightstack` | 24 suites, 169 tests, all pass |
| `test/security_groups_test.sh` | 12 of 13: Test 1 fails, and Test 11 skips itself without saying so |

The unit counts match T051's per package, so the per-package tables under T051 hold for this baseline too.

Contract on PostgreSQL has 27 fewer passes than T051 recorded, and one more skip. All 27 are
`TestNovaActionsByName_Contract` with its 26 subtests. It now skips on PostgreSQL too, with `no active image available
to boot from`. The recreated database holds no image, as the SQLite stack's never does. Round 1's database still had one
active image. No code change is involved. Per package, contract, on both backends:

| Package | PostgreSQL | SQLite |
|---|---|---|
| `test/contract` | 5/0/9 | 5/0/9 |
| `test/contract/cinder` | 65/0/0 | 65/0/0 |
| `test/contract/glance` | 29/0/0 | 29/0/0 |
| `test/contract/keystone` | 66/0/0 | 66/0/0 |
| `test/contract/neutron` | 74/0/0 | 74/0/0 |
| `test/contract/nova` | 128/0/3 | 128/0/3 |

Failures that predate round 2: only `test/security_groups_test.sh`, for the reason recorded under T001 (the `GROUPS`
builtin). T081 renames the variable. Nothing was fixed here.

## Round 2 test strategy approval (T059)

**Approved by the user on 2026-11-20, as written**: research R43 and every round-2 test task in `tasks.md` (T060–T151),
with `test/security_groups_test.sh`'s rename (T081).

The user also set a checkpoint. Implementation stops after Phase 17a, the API increment, for review and a commit, and
resumes with Phase 18.

## Phase 17a checkpoint (after T107)

**When**: 2026-11-20. **Code**: the working tree on `1517716` with Phases 11–17a, uncommitted. The method is the
baseline's, run by `gates.sh`. Its unit run found eight older Neutron tests that pinned the old administrator scope
(recorded under T106). After those changes, the unit suites ran again in full on both backends, and
`test/security_groups_test.sh` ran again against the PostgreSQL stack. The contract results are from the gate run: no
code outside tests changed after it.

| Gate | Baseline (T058) | Phase 17a |
|---|---|---|
| Build | passes | passes |
| Lint | `0 issues` | `0 issues` |
| `go vet ./...` / `gofmt -l` | clean / clean | clean / clean |
| Unit, PostgreSQL | 2212 passed, 0 failed, 6 skipped | 2453 passed, 0 failed, 6 skipped |
| Unit, SQLite | 2186 passed, 0 failed, 22 skipped | 2414 passed, 0 failed, 24 skipped |
| Contract, PostgreSQL | 367 passed, 0 failed, 12 skipped | 392 passed, 0 failed, 12 skipped |
| Contract, SQLite | 367 passed, 0 failed, 12 skipped | 392 passed, 0 failed, 12 skipped |
| `helm unittest charts/lightstack` | 24 suites, 169 tests, all pass | 24 suites, 171 tests, all pass (T070's two cases) |
| `test/security_groups_test.sh` | 12 of 13 | 13 of 13 |

Contract per package moved only in `test/contract/neutron` (74 → 98) and `test/contract/nova` (128 → 129).
`test/security_groups_test.sh`'s Test 1 passes now because the seeded project's `default` existed when it ran.
Lightstack does not create a project's `default` when it is listed, as upstream's `get_security_groups` does, so the
test still depends on what ran before it. That predates this feature.

Afterwards the shared database was cleaned of what the runs leave behind: three pending `image_tasks` rows and two
backup images from the Glance contract tests, and ten `md-net`/`md-null-net` networks with their ports from
`internal/metadata`'s cleanup, which runs with a cancelled context (round 1 recorded it).


## Phase 20 (after T124)

**When**: 2026-11-20. **Code**: the working tree on the API commit (`fix(neutron): answer the networking API as upstream
does`, then `ba700dc`, rewritten since) with Phases 18–20, uncommitted. The method is the baseline's, run by `gates.sh`.
A first run, before T123's lock order was settled and `CreatePort` honoured `fixed_ips`, failed
`TestPortForwardingLifecycle_Contract` on both backends. Phase 19's internal-address check refused the address the test
had asked for and `CreatePort` had ignored (recorded under T123). The run below is the second.

| Gate | Phase 17a | Phase 20 |
|---|---|---|
| Build | passes | passes |
| Lint | `0 issues` | `0 issues` |
| `go vet ./...` / `gofmt -l` | clean / clean | clean / clean |
| Unit, PostgreSQL | 2453 passed, 0 failed, 6 skipped | 2499 passed, 1 failed, 6 skipped |
| Unit, SQLite | 2414 passed, 0 failed, 24 skipped | 2461 passed, 0 failed, 24 skipped |
| Contract, PostgreSQL | 392 passed, 0 failed, 12 skipped | 419 passed, 0 failed, 11 skipped |
| Contract, SQLite | 392 passed, 0 failed, 12 skipped | 392 passed, 0 failed, 12 skipped |
| `helm unittest charts/lightstack` | 24 suites, 171 tests, all pass | 24 suites, 171 tests, all pass |
| `test/security_groups_test.sh` | 13 of 13 | all pass |

The PostgreSQL unit failure is `internal/glance`'s `TestTaskListAndGetAreOwnerScoped`: it found three pending import
tasks the first run's Glance contract tests had left in the shared database. With those removed, the package passes. The
PostgreSQL contract count rose by 27 because `TestNovaActionsByName_Contract` skips unless the database happens to hold
an active image to boot from, and the first run had left one. Its 27 subtests passed.

Afterwards the shared database was cleaned of what the two runs left behind: six pending `image_tasks` rows and four
backup images from the Glance contract tests, and ten `md-net`/`md-null-net` networks with their ports from
`internal/metadata`'s older tests.

## Phase 21 (after T134)

**When**: 2026-11-23. **Code**: the working tree on the API commit with Phases 18–21, uncommitted. The method is the
baseline's, run by `gates.sh`.

| Gate | Phase 20 | Phase 21 |
|---|---|---|
| Build | passes | passes |
| Lint | `0 issues` | `0 issues` |
| `go vet ./...` / `gofmt -l` | clean / clean | clean / clean |
| Unit, PostgreSQL | 2499 passed, 1 failed, 6 skipped | 2467 passed, 0 failed, 6 skipped |
| Unit, SQLite | 2461 passed, 0 failed, 24 skipped | 2428 passed, 0 failed, 24 skipped |
| Contract, PostgreSQL | 419 passed, 0 failed, 11 skipped | 419 passed, 0 failed, 11 skipped |
| Contract, SQLite | 392 passed, 0 failed, 12 skipped | 392 passed, 1 failed, 11 skipped |
| `helm unittest charts/lightstack` | 24 suites, 171 tests, all pass | 24 suites, 171 tests, all pass |
| `test/security_groups_test.sh` | all pass | all pass |

The unit counts fell because T130 deleted the filter-only tests (listed there). The SQLite contract failure is
`TestNovaActionsByName_Contract`, which runs only when an active image exists: it took the first image while a Glance
contract test, running in parallel against the same stack, deleted it, and the create answered `400` (image not found),
as the stack's log shows. That race predates this feature; the same test passed on PostgreSQL in this run.

## Phase 22 (after T141)

**When**: 2026-11-23. **Code**: the working tree on the API commit with Phases 18–22, uncommitted. The method is the
baseline's, run by `gates.sh`, whose unit runs found two tests that pinned what Phase 22 changes on purpose, recorded
under T138: a port deletion whose router cannot be converged now keeps the port (FR-040, where R25's departure let the
deletion stand), and the old node of a moved binding converges from the records. With both updated, `internal/neutron`,
`internal/nova` and `internal/glance` ran again in full on both backends; the rest of the gate run stands, as no code
outside those tests changed after it.

| Gate | Phase 21 | Phase 22 |
|---|---|---|
| Build | passes | passes |
| Lint | `0 issues` | `0 issues` |
| `go vet ./...` / `gofmt -l` | clean / clean | clean / clean |
| Unit, PostgreSQL | 2467 passed, 0 failed, 6 skipped | 2488 passed, 4 failed, 6 skipped in the gate run; the three packages again: 1264 passed, 0 failed, 2 skipped |
| Unit, SQLite | 2428 passed, 0 failed, 24 skipped | 2450 passed, 3 failed, 24 skipped in the gate run; the three packages again: 1260 passed, 0 failed, 6 skipped |
| Contract, PostgreSQL | 419 passed, 0 failed, 11 skipped | 419 passed, 0 failed, 11 skipped |
| Contract, SQLite | 392 passed, 1 failed, 11 skipped | 392 passed, 0 failed, 12 skipped |
| `helm unittest charts/lightstack` | 24 suites, 171 tests, all pass | 24 suites, 171 tests, all pass |
| `test/security_groups_test.sh` | all pass | all pass |

The fourth PostgreSQL failure was `internal/glance`'s `TestTaskListAndGetAreOwnerScoped` again, failing on the Glance
contract tests' leftovers in the shared database. The shared database was cleaned of six pending `image_tasks` rows and
four backup images.

## Phase 23 (after T148)

**When**: 2026-11-23. **Code**: the working tree on the API commit with Phases 18–23, uncommitted. One gate run by
`gates.sh`, green throughout; no rerun was needed. Phase 23's mutation checks, run off the worktree through `go test
-overlay`, are with T161's: 19 of 19 caught, one of them only after its test gained a recorded port on a node that is no
longer active (T144's note).

| Gate | Phase 22 | Phase 23 |
|---|---|---|
| Build | passes | passes |
| Lint | `0 issues` | `0 issues` |
| `go vet ./...` / `gofmt -l` | clean / clean | clean / clean |
| Unit, PostgreSQL | 2488 passed, 4 failed, 6 skipped in the gate run; the three packages again: 1264 passed, 0 failed, 2 skipped | 2529 passed, 0 failed, 6 skipped |
| Unit, SQLite | 2450 passed, 3 failed, 24 skipped in the gate run; the three packages again: 1260 passed, 0 failed, 6 skipped | 2490 passed, 0 failed, 24 skipped |
| Contract, PostgreSQL | 419 passed, 0 failed, 11 skipped | 419 passed, 0 failed, 11 skipped |
| Contract, SQLite | 392 passed, 0 failed, 12 skipped | 392 passed, 0 failed, 12 skipped |
| `helm unittest charts/lightstack` | 24 suites, 171 tests, all pass | 24 suites, 171 tests, all pass |
| `test/security_groups_test.sh` | all pass | all pass |

## Phase 24 (after T153)

**When**: 2026-11-23. **Code**: the working tree on the API commit with Phases 18–24, uncommitted. One gate run by
`gates.sh`. Its two PostgreSQL failures were `internal/glance`'s `TestTaskListAndGetAreOwnerScoped` and
`TestImageMarkerPagingDoesNotDropTiedRows`, failing on the Glance contract tests' leftovers from the Phase 23 run in the
shared database, as in Phase 22; with six pending `image_tasks` rows and four backup images removed, `internal/glance`
passed again on PostgreSQL. `test/compose_kvm_state_test.sh`, outside `gates.sh`, passes with the two new mounts.

| Gate | Phase 23 | Phase 24 |
|---|---|---|
| Build | passes | passes |
| Lint | `0 issues` | `0 issues` |
| `go vet ./...` / `gofmt -l` | clean / clean | clean / clean |
| Unit, PostgreSQL | 2529 passed, 0 failed, 6 skipped | 2535 passed, 2 failed, 6 skipped in the gate run; `internal/glance` again: passes |
| Unit, SQLite | 2490 passed, 0 failed, 24 skipped | 2498 passed, 0 failed, 24 skipped |
| Contract, PostgreSQL | 419 passed, 0 failed, 11 skipped | 419 passed, 0 failed, 11 skipped |
| Contract, SQLite | 392 passed, 0 failed, 12 skipped | 392 passed, 0 failed, 12 skipped |
| `helm unittest charts/lightstack` | 24 suites, 171 tests, all pass | 25 suites, 177 tests, all pass |
| `test/security_groups_test.sh` | all pass | all pass |
| `test/compose_kvm_state_test.sh` | — | 2 passed |

## Round 2 mutation check (T161)

**When**: 2026-11-23. **Code**: the working tree on the API commit with Phases 11–24, uncommitted. As T052's: each
change reverted off the worktree, one at a time, through `go test -overlay`, applied only when its target text occurs
exactly once, and the pinning tests run against it — on PostgreSQL, the migrator's on SQLite, the pure renderers without
a database. 57 reversions, all caught. Six needed a second run, as noted: R17 once the requested-address tests ran too,
R32 and R50 once a test was extended, and R20, R34 and R44 once the reversion's text built. The complete-read rule's
reversions (T142) and the rest of Phase 23's are in its 19-of-19 run above, not repeated. Two of T161's items are not
overlay reversions: AddRouterInterface's one transaction (T093) has no single statement whose removal splits it, and its
pin, `TestRouterInterfaces`' refused interface storing no port, ran green with the code; the remove order (T113) is R38.

| # | Reverted | Result |
|---|---|---|
| R01 | the description filter ignored (T071) | caught |
| R02 | the stateful filter ignored (T071) | caught |
| R03 | the shared filter ignored (T071) | caught |
| R04 | the revision_number filter ignored (T071) | caught |
| R05 | the tag filters ignored (T071) | caught |
| R06 | the project_id/tenant_id filters ignored (T071) | caught |
| R07 | a member deleting an unused default (T078) | caught |
| R08 | renaming default (T078) | caught |
| R09 | creating a group named default (T078) | caught |
| R10 | renaming a group to default (T078) | caught |
| R11 | the ambiguity check of add/removeSecurityGroup (T082) | caught |
| R12 | the ambiguity check of server create (T082) | caught |
| R13 | the migrator's foreign-key check (T061) | caught |
| R14 | a new group's starting rules (T079) | caught |
| R15 | default's member rules (T079) | caught |
| R16 | the pools ignored by the allocator (T086) | caught |
| R17 | the gateway not excluded (T087) | caught, by the requested-address tests (`TestRequestedAddresses` and the gateway and interface tests); the default pool already leaves the gateway out, so the allocator's own tests cannot see the exclusion |
| R17b | the default pool keeping the gateway (T087) | caught |
| R18 | the floating IPs not excluded (T087) | caught |
| R19 | InvalidAllocationPool (IP version) (T086) | caught |
| R20 | OutOfBoundsAllocationPool (T086) | caught (the first text left a variable unused and did not build) |
| R21 | OverlappingAllocationPools (T086) | caught |
| R22 | GatewayConflictWithAllocationPools (T086) | caught |
| R23 | the allocation lock (T097) | caught |
| R24 | the router lookup's floating-network check (T098) | caught |
| R25 | the gateway-interface preference (T098) | caught |
| R26 | FR-029's check (T098) | caught |
| R27 | the gateway reported from its port (T093) | caught |
| R28 | RouterExternalGatewayInUseByFloatingIp (T094) | caught |
| R29 | RouterInterfaceInUseByFloatingIP (T094) | caught |
| R30 | the node token's audience (T108) | caught |
| R31 | the node token's lifetime (T108) | caught |
| R32 | the node token's expiry required (T108) | caught, after `TestNodeCallToken` gained a token without `exp` (and one without `iat`): the library checks an expiry that is present, so only a token without one shows `WithExpirationRequired` |
| R33 | the node token's node (T108) | caught |
| R34 | the node token's body digest (T108) | caught (the first text left an import unused and did not build) |
| R35 | the SNAT exception (T112) | caught |
| R36 | the hairpin rule (T112) | caught |
| R37 | the proxy entries before the routes (T113) | caught |
| R38 | removal: the namespace before the legs (T113) | caught |
| R39 | the static range made dynamic (T119) | caught |
| R40 | option 121's metadata route (T119) | caught |
| R41 | metadata identity by address only (T121) | caught |
| R42 | the spoof chain (T125) | caught |
| R43 | the INVALID drop (T125) | caught |
| R44 | the remote-group expansion (T125) | caught (the first text left a variable unused and did not build) |
| R45 | the zone rules (T125) | caught |
| R46 | the TAP name (T128) | caught |
| R47 | a subnet's record deleted before its DHCP (T135) | caught |
| R48 | a subnet deletion's re-apply (T135) | caught |
| R49 | a port's record deleted before its units (T135) | caught |
| R50 | a port deletion's re-apply (T135) | caught, after `TestDeletionOrder_Port` gained a final transaction that fails (`failingStatement`): only the units' failure was pinned, not the record's |
| R51 | bridge-netfilter not failing readiness (T150) | caught |
| R52 | the settings never read back (T149) | caught |
| R53 | projectScope's administrator branch (T102) | caught |
| R54 | a show's administrator bypass (T102) | caught |
| R55 | a write's administrator target (T104) | caught |
| R56 | an administrator's foreign group id refused (T105) | caught |

## Round 2 results (T160)

**When**: 2026-11-23. **Code**: the working tree on the API commit with Phases 18–25, uncommitted. **Method**: T058's —
the PostgreSQL database recreated with `make db-down && make db-up` first (nothing else was connected to it), then
`gates.sh`. Green throughout; no rerun.

| Gate | T058 baseline | Round 2 |
|---|---|---|
| Build | passes | passes |
| Lint | `0 issues` | `0 issues` |
| `go vet ./...` / `gofmt -l` | clean / clean | clean / clean |
| Unit, PostgreSQL | 2212 passed, 0 failed, 6 skipped | 2538 passed, 0 failed, 6 skipped |
| Unit, SQLite | 2186 passed, 0 failed, 22 skipped | 2499 passed, 0 failed, 24 skipped |
| Contract, PostgreSQL | 367 passed, 0 failed, 12 skipped | 419 passed, 0 failed, 11 skipped |
| Contract, SQLite | 367 passed, 0 failed, 12 skipped | 392 passed, 0 failed, 12 skipped |
| `helm unittest charts/lightstack` | 24 suites, 169 tests, all pass | 25 suites, 177 tests, all pass |
| `test/security_groups_test.sh` | 12 of 13: Test 1 fails, Test 11 skips itself | 13 of 13; Test 11 passes (`409` for a member deleting `default`) |

Contract on PostgreSQL: of the 52 more passes, 27 are `TestNovaActionsByName_Contract` and its subtests, which skipped
at T058 for want of an active image and found one this time — the unit suites, which run first, leave one in the
recreated database — 24 are round 2's own Neutron contract tests (`test/contract/neutron`, 74 → 98), and one more is in
`test/contract/nova`. Contract on SQLite: the same 24 and the same one. The per-package counts:

| Package | PostgreSQL | SQLite |
|---|---|---|
| `test/contract` | 5/0/9 | 5/0/9 |
| `test/contract/cinder` | 65/0/0 | 65/0/0 |
| `test/contract/glance` | 29/0/0 | 29/0/0 |
| `test/contract/keystone` | 66/0/0 | 66/0/0 |
| `test/contract/neutron` | 98/0/0 | 98/0/0 |
| `test/contract/nova` | 156/0/2 | 129/0/3 |
