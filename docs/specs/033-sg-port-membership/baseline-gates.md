# Baseline gates — SPEC-033

T001 records where the gates stand before any change, so that T051 can compare (SC-007, SC-008). Nothing was fixed here.

**When**: 2026-11-18. **Code**: `bugfix/579` at `d81f081`, the `main` commit it is based on. The working tree differs only in documents: the constitution, `CLAUDE.md` and this spec directory. **Host**: the openSUSE development host, with podman behind the `docker` CLI.

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
- **The flags.** `-count=1`, because Go's test cache ignores `LIGHTSTACK_TEST_DB_URL`, so a cached result can come from another database. `-p 1`, because tests that read the seeded project race across packages that run in parallel. T051 must use the same flags.
- **The PostgreSQL database.** It is the `make db-up` container, `lightstack-postgres`, at migration 75, which is this branch's newest. It held only the `default` project before the run.
- **The stack.** `bin/lightstack` ran on the host, not under Docker Compose, with `config/lightstack.yaml` and three overrides:
  - `state_path` under `/tmp`;
  - the metadata service trusting `X-Instance-ID`, as the compose file sets it;
  - `LIGHTSTACK_DB_URL` naming the backend: the `make db-up` database, or a fresh file `sqlite:///tmp/ls579/stack-sqlite.db`.

  Compose was not used because its `postgres` service has the same container name and port as the running `make db-up` database. Starting it would have replaced the database other worktrees use. Unlike the compose stack, this one runs without the Redis cache. T051 must use the same method.
- **The order.** The unit suites run before the contract suites. The Glance contract tests create import tasks for the seeded admin and leave them `pending` in `image_tasks`, and `internal/glance`'s `TestTaskListAndGetAreOwnerScoped` then counts them and fails. That happened once on this branch, after the baseline: three such rows from the PostgreSQL contract run, deleted again.

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
  - in `internal/database`: `TestGenerateSQLiteBaseline`, `TestMigrationHelperProcess` and `TestDescribeReportsZeroWhenNotMigrated`.
- **Unit, SQLite**:
  - the PostgreSQL-only tests in `internal/database` and `internal/keystone` (the `groups.domain_id` migration);
  - `TestSchemaParity`;
  - `TestGenerateSQLiteBaseline` and `TestMigrationHelperProcess`;
  - two `internal/glance` subtests of `TestUploadFailsWhenStateCannotBeRecorded`.
- **Contract, both**:
  - the eight `TestTemplate_*` scaffolds;
  - `TestWS0_ImageNormalizesToRaw`;
  - `TestNovaCreateRefusesForeignNetwork_Contract` and `TestNovaFlavorAccessControl_Contract`.
- **Contract, SQLite only**: `TestNovaActionsByName_Contract`, with its 26 subtests. That accounts for the 27 fewer `nova` passes.

## Failures that predate this branch

The bash failure is not a product defect.

**`test/security_groups_test.sh`: Test 1 fails, and Test 11 does nothing.**
- The script stores the group listing in `GROUPS` (`:40`). That is a bash builtin variable, the current user's group ids; assigning to it has no effect. `:43` and `:209` then read the group ids instead of the listing.
- `jq` therefore never sees the listing, and Test 1 reports `Default security group not found` although `default` exists.
- Test 11, deleting `default`, finds no id and skips itself without a word. It is the test FR-008c's `409` would answer.

Proposed for T002, to decide with the test strategy: `test/security_groups_test.sh` renames its variable. That keeps a gate from failing for a reason unrelated to this feature.

## Test strategy approval (T002)

**Approved by the user on 2026-11-18, as written**: research R16 and every test task in `tasks.md`. The script change proposed above was offered as an option and not taken: `test/security_groups_test.sh` keeps its variable. The script therefore stays as recorded here, and T051 compares against this record.

## After the change (T051)

**When**: 2026-11-18. **Code**: the working tree on `d81f081` with this feature's changes, uncommitted. The method is the one above, with the unit suites run before the contract suites.

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

No gate got worse. The three extra skips on SQLite are this feature's PostgreSQL-only lock-order tests: the copy waiting for a change that holds the instance (`internal/neutron`), a port's deletion locking the port first (`internal/neutron`), and a server's deletion locking the instance before its ports (`internal/nova`).

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
- **They booted without a network.** Since a server's groups are its ports' groups, these now boot on a network: `test/contract/nova/server_actions_test.go`'s `TestAddSecurityGroup_Contract` and `TestRemoveSecurityGroup_Contract`, `test/contract/nova/action_by_name_test.go`'s `TestNovaActionsByName_Contract`, and `test/contract/server_readback_test.go`'s `TestNovaServerReadBack_SecurityGroups_Contract`. Their assertions are unchanged, except that the remove test now expects `202` (T020).
- **They read `server_security_groups`.** `internal/nova/security_group_ref_test.go` now reads the resolved group from the boot's allocation.

### Test-run leftovers in the shared database

The PostgreSQL runs left rows in the `make db-up` database, which was cleaned up afterwards:
- the Glance contract tests' `pending` import tasks;
- the `createBackup` and `createImage` images of the Nova contract tests;
- two networks per unit run from `internal/metadata`'s `TestNetworkDataJSONToleratesNullFixedIPs`, whose cleanup runs on the already-cancelled `t.Context()`.

All three predate this feature.

## Mutation check (T052)

Each fix was reverted off the worktree, one at a time, through `go test -overlay`, and the named tests were run against it. The driver applies a mutation only when its target text occurs exactly once. All 49 were caught. Two are caught by a timeout rather than a failing assertion, because the test then blocks for ever: removing the runner's deadline (M05), and removing the node lock's deadline (M07).

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

Two items of T052's list are not overlay mutations, because `main` is that code. Their tests were run against `main` and failed:
- add writing `server_security_groups` only (T010, the earlier-release case): `TestAddSecurityGroup_EarlierReleaseServer` failed on `main`, before T018;
- create programming before its insert (T036, failure on create): `TestFloatingIPDataplane_Failures`'s create cases failed on `main`, which programs the NAT before the insert, before T043.

# Round 2

## Round 2 baseline (T058)

**When**: 2026-11-20. **Code**: `bugfix/579` at `1517716`. That is round 1's code (`dcda565`) plus round 2's planning: this spec directory, the constitution and `CLAUDE.md`. **Host**: the same openSUSE development host.

The method is round 1's (above), with three changes:
- `go vet ./...`, `gofmt -l` and `helm unittest charts/lightstack` were added.
- `test/nova_server_actions_test.sh` is gone; round 1 deleted it. Of the bash scripts, only `test/security_groups_test.sh` runs.
- The PostgreSQL database was recreated with `make db-down && make db-up` before the run. The suites' `TestMain` migrated the new one to 75. The eleven `spec020-…` to `spec025…` projects it had held are fixtures the unit suites create on every run and keep; they came back during this run.

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

Contract on PostgreSQL has 27 fewer passes than T051 recorded, and one more skip. All 27 are `TestNovaActionsByName_Contract` with its 26 subtests. It now skips on PostgreSQL too, with `no active image available to boot from`. The recreated database holds no image, as the SQLite stack's never does. Round 1's database still had one active image. No code change is involved. Per package, contract, on both backends:

| Package | PostgreSQL | SQLite |
|---|---|---|
| `test/contract` | 5/0/9 | 5/0/9 |
| `test/contract/cinder` | 65/0/0 | 65/0/0 |
| `test/contract/glance` | 29/0/0 | 29/0/0 |
| `test/contract/keystone` | 66/0/0 | 66/0/0 |
| `test/contract/neutron` | 74/0/0 | 74/0/0 |
| `test/contract/nova` | 128/0/3 | 128/0/3 |

Failures that predate round 2: only `test/security_groups_test.sh`, for the reason recorded under T001 (the `GROUPS` builtin). T081 renames the variable. Nothing was fixed here.

## Round 2 test strategy approval (T059)

**Approved by the user on 2026-11-20, as written**: research R43 and every round-2 test task in `tasks.md` (T060–T151), with `test/security_groups_test.sh`'s rename (T081).

The user also set a checkpoint. Implementation stops after Phase 17a, the API increment, for review and a commit, and resumes with Phase 18.

## Phase 17a checkpoint (after T107)

**When**: 2026-11-20. **Code**: the working tree on `1517716` with Phases 11–17a, uncommitted. The method is the baseline's, run by `gates.sh`. Its unit run found eight older Neutron tests that pinned the old administrator scope (recorded under T106). After those changes, the unit suites ran again in full on both backends, and `test/security_groups_test.sh` ran again against the PostgreSQL stack. The contract results are from the gate run: no code outside tests changed after it.

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

Contract per package moved only in `test/contract/neutron` (74 → 98) and `test/contract/nova` (128 → 129). `test/security_groups_test.sh`'s Test 1 passes now because the seeded project's `default` existed when it ran. Lightstack does not create a project's `default` when it is listed, as upstream's `get_security_groups` does, so the test still depends on what ran before it. That predates this feature.

Afterwards the shared database was cleaned of what the runs leave behind: three pending `image_tasks` rows and two backup images from the Glance contract tests, and ten `md-net`/`md-null-net` networks with their ports from `internal/metadata`'s cleanup, which runs with a cancelled context (round 1 recorded it).

