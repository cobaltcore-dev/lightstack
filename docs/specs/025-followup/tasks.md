# Tasks: SPEC-025 Follow-ups

**Input**: Design documents from `docs/specs/025-followup/`
**Prerequisites**: [plan.md](./plan.md), [spec.md](./spec.md), [research.md](./research.md),
[data-model.md](./data-model.md), [contracts/](./contracts/), [quickstart.md](./quickstart.md)

**Tests**: **REQUIRED, not optional.** The Constitution's Test-First workflow is non-negotiable, and
SC-013 requires every new test be *demonstrated* failing before the change. Each story therefore
leads with tests and carries an explicit "confirm RED" task.

**Why the RED step is a task and not a note**: all seven defects answer with a success status today.
`?name=probe` returns probe. A patch returns `200`. Under-quota creation succeeds. So a test that
exercises only the working path passes before the fix exists and proves nothing after it — which is
exactly what happened to nine of SPEC-025's verification steps, caught only during its host pass.

**Organization**: one phase per user story, in the spec's priority order. Every story is
independently implementable and testable.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: content-independent — no dependency on an incomplete task, and no shared decision with
  another task. **This is looser than the template's "different files" rule, deliberately**: most of
  this feature's tests are independent checks that belong in one existing `*_test.go` file, and
  splitting them across files to earn a marker would scatter them away from the code they pin. Sixteen
  `[P]` tasks share `internal/cinder/quotas_test.go`, eight share `internal/nova/server_filter_test.go`,
  eight share `internal/glance/images_test.go`.
  **Consequence for concurrent execution**: `[P]` tasks that name the same file must still be
  serialised at the file level — they can be planned in parallel and written in any order, but not
  edited simultaneously.
- **[Story]**: US1–US7, mapping to the spec's user stories

## Path Conventions

Single Go module at the repository root. Source under `internal/`, unit tests alongside as
`*_test.go`, integration scripts under `test/`.

---

## Phase 1: Setup (Baseline)

**Purpose**: know what was already broken before touching anything.

- [X] T001 Record the baseline gate state — run `make fmt`, `make lint` and `go test ./...` at `HEAD` and note any pre-existing failure in `docs/specs/025-followup/BASELINE.md`, so a failure that predates this feature is not attributed to it
- [X] T002 Check the shared `lightstack-postgres` container's fixture state before any quota work — `SELECT resource, hard_limit FROM quotas WHERE project_id = (SELECT id FROM projects WHERE name='default')` and `SELECT name FROM projects`, recorded in `docs/specs/025-followup/BASELINE.md`. A sibling worktree once left `quotas.instances = 0` for `default`, and a whole contract suite reported failures that had nothing to do with the code. This feature is *about* quotas, so a poisoned row would be attributed to it

**Checkpoint**: baseline known and written down.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: the two shared-package additions that stories depend on. Both are in `internal/common`,
so they are done here rather than inside a story — two stories editing `common` concurrently is the
one file conflict this feature has.

- [X] T003 [P] Add a `406 Not Acceptable` constructor to `internal/common/errors.go` (`NewNotAcceptableError`), alongside the twenty existing constructors — the product has no 406 and US6 needs one
- [X] T004 [P] Add a unit test for the 406 constructor's status code and body shape in `internal/common/errors_test.go`
- [X] T005 [P] Extract a bracket-aware host-only helper in `internal/common/baseurl.go` (export `HostWithoutPort`, or extract it from the unexported `hostWithPort`) — US4 must strip the port from a request host before substituting it, and this logic already exists here
- [X] T006 [P] Add unit tests for the host-only helper in `internal/common/baseurl_test.go`: bare host, host with port, IPv6 literal bracketed (`[::1]:8774`), IPv6 literal unbracketed (`[fe80::1]`), and empty. The two-port authority (`host:35357:8774`) this prevents is a failure `hostWithPort`'s own comment records having produced before

**Checkpoint**: `common` is ready; every user story can now proceed in parallel.

---

## Phase 3: User Story 1 — Narrow a server listing and get the servers asked for (Priority: P1) 🎯 MVP

**Goal**: `GET /v2.1/servers` and `/servers/detail` apply the filters they accept; a filter matching
nothing returns nothing; a filter naming an attribute the product does not record is refused.

**Independent Test**: list servers with a name that exists and one that does not — the second must
return an empty collection. See [contracts/server-list-filters.md](./contracts/server-list-filters.md).

### Tests for User Story 1 ⚠️ WRITE FIRST, CONFIRM RED

- [X] T007 [P] [US1] Per-filter negative test in `internal/nova/server_filter_test.go` — for **each** honoured filter (`name`, `status`, `image`, `flavor`, `changes-since`, `user_id`, `project_id`, `key_name`, `locked`, `task_state`, `power_state`), a non-matching value returns an empty collection (FR-001, FR-003, FR-009)
- [X] T008 [P] [US1] Per-filter positive test in `internal/nova/server_filter_test.go` — each filter returns exactly the matching servers, so the negative case is not passing by returning empty always
- [X] T009 [P] [US1] Regex-semantics test for `name` in `internal/nova/server_filter_test.go` — a pattern matching two servers returns both, an anchored pattern returns one, and an invalid pattern is a client error rather than a 500 (FR-003a, research R1)
- [X] T010 [P] [US1] Refused-filter test in `internal/nova/server_filter_test.go` — `host`, `hostname`, `availability_zone`, `tags` and (until US7) `ip` each answer `400` naming the filter (FR-004, FR-003b)
- [X] T011 [P] [US1] Ignored-extra test in `internal/nova/server_filter_test.go` — a parameter that is neither honoured nor refused is ignored and the request succeeds, as today (FR-004a)
- [X] T012 [P] [US1] Scope tests in `internal/nova/authz_test.go` — a non-administrator naming another project sees only their own servers; an administrator naming another project **without** `all_tenants` gets an empty collection and the filter does not imply the opt-in (FR-006, FR-006a)
- [X] T013 [P] [US1] Filter-with-paging test in `internal/nova/server_filter_test.go` — marker paging across a filtered listing repeats no server and skips none, and the marker lookup runs under the same predicates as the page query (FR-005)
- [X] T014 [P] [US1] Brief-versus-detail parity test in `internal/nova/server_filter_test.go` — the same query returns the same set of servers from both representations (FR-002)
- [X] T015 [P] [US1] Unfiltered-unchanged test in `internal/nova/server_filter_test.go` — a request with no filter parameters returns the same servers in the same order as before this feature (FR-007)
- [X] T016 [US1] **Confirm RED** — run the US1 tests and record their failure output in `docs/specs/025-followup/BASELINE.md`. T007, T010 and T012 must fail; T008 and T015 are expected to PASS already and are the controls that prove the tests exercise the right path

### Implementation for User Story 1

- [X] T017 [US1] Add query-parameter readers to `serverFilter` in `internal/nova/server_filter.go`, mirroring `internal/keystone/list_filters.go`'s `eq` / `eqBool` — present-and-non-empty adds a predicate, absent or empty adds nothing, so the unfiltered path stays byte-identical
- [X] T018 [US1] Add the `name` regex predicate to `internal/nova/server_filter.go` using PostgreSQL's `~` operator, with an invalid pattern producing a client error rather than reaching the database as a fault (FR-003a, FR-008)
- [X] T019 [US1] Add the remaining all-caller predicates (`status`, `image` → `image_id`, `flavor` → `flavor_id`, `changes-since` → `updated_at`) to `internal/nova/server_filter.go`
- [X] T020 [US1] Add the administrator-only predicates (`user_id`, `project_id`/`tenant_id`, `key_name`, `locked`, `task_state`, `power_state`) to `internal/nova/server_filter.go`, gated on `common.HasRole(c, common.RoleAdmin)` and applied **on top of** the caller's scope so none of them can widen it (FR-006a)
- [X] T021 [US1] Add the refused-filter enumeration to `internal/nova/server_filter.go` — exactly FR-004's set: `host`, `hostname`, `availability_zone`, `tags`, and `ip` until US7 lands — answering `400` naming the filter, and returning the refusal to the handler rather than panicking or silently dropping it (FR-004, FR-003b)
- [X] T022 [US1] Wire the filter and its refusal into **both** listings in `internal/nova/handlers.go` (`ListServers` and the detail listing), so neither can honour a different set (FR-002)
- [X] T023 [US1] Confirm the marker lookup in `internal/nova/handlers.go` reuses the filter's predicates via `CloneArgs` for both listings, so a filtered page boundary cannot restart at page one (FR-005)
- [X] T024 [US1] **Verify GREEN** — `go test ./internal/nova/...` — and confirm the unfiltered request's generated SQL is unchanged from before the story

**Checkpoint**: filters work, `--name` narrows, a refused filter says so.

---

## Phase 4: User Story 2 — Set an image property and have it stick (Priority: P1)

**Goal**: `PATCH /v2/images/{id}` applies `add` as well as `replace`, refuses what it cannot honour
instead of answering `200`, and applies a document atomically.

**Independent Test**: patch a property with `add`, then read the image back.
See [contracts/image-patch-operations.md](./contracts/image-patch-operations.md).

### Tests for User Story 2 ⚠️ WRITE FIRST, CONFIRM RED

- [X] T025 [P] [US2] `add`-then-read-back test in `internal/glance/images_test.go` — `{"op":"add","path":"/min_disk","value":7}` returns `200`, the response reports 7, and a subsequent read agrees (FR-010, FR-017)
- [X] T026 [P] [US2] `replace` regression test in `internal/glance/images_test.go` — today's behaviour is preserved for all four writable paths
- [X] T027 [P] [US2] `remove` test in `internal/glance/images_test.go` — `403` naming the path, and the property keeps its value rather than resetting to a default (FR-011, research R2)
- [X] T028 [P] [US2] Unsupported-operation test in `internal/glance/images_test.go` — an operation RFC 6902 does not define answers `400` naming the operation (FR-012)
- [X] T029 [P] [US2] Non-allowlisted-path test in `internal/glance/images_test.go` — a service-managed property answers `403` naming the path, including an arbitrary custom property the loop currently discards (FR-013)
- [X] T030 [P] [US2] Atomicity test in `internal/glance/images_test.go` — a document whose first operation is valid and second is not leaves the image entirely unchanged (FR-014)
- [X] T031 [P] [US2] Zero-row test in `internal/glance/images_test.go` — a patch against a non-existent image, and against one owned by another project, each answer `404` rather than `200` (FR-015)
- [X] T032 [P] [US2] Value-type test in `internal/glance/images_test.go` — `{"op":"replace","path":"/min_disk","value":"ten"}` answers `400`
- [X] T033 [US2] **Confirm RED** — T025, T027, T028, T029, T030 and T031 must fail; T026 is the control and should pass. Record output in `docs/specs/025-followup/BASELINE.md`

### Implementation for User Story 2

- [X] T034 [US2] Restructure `UpdateImage` in `internal/glance/images.go` into two passes — validate every operation in the document first, then apply — so a refusal happens before any write (FR-014)
- [X] T035 [US2] Apply the validated operations inside one transaction via `database.WithTx` in `internal/glance/images.go`, replacing the per-operation `Exec` whose errors are currently logged and swallowed
- [X] T036 [US2] Implement `add` as the same write `replace` performs for the four allowlisted scalar paths in `internal/glance/images.go` (FR-010)
- [X] T037 [US2] Refuse `remove` with `common.NewForbiddenError` naming the path in `internal/glance/images.go` — all four are base properties with schema defaults, so none has an absent state (FR-011)
- [X] T038 [US2] Refuse a non-allowlisted path with `403` and an undefined operation with `400`, each naming what was refused, in `internal/glance/images.go` (FR-012, FR-013)
- [X] T039 [US2] Validate each operation's value type against its column in `internal/glance/images.go` before the transaction opens
- [X] T040 [US2] Check the affected-row count in `internal/glance/images.go` and answer `404` when a write matches nothing (FR-015)
- [X] T041 [US2] Make the response reflect the applied state and remove the now-false `// Apply updates (simplified - only handles replace operations)` comment in `internal/glance/images.go` (FR-016)
- [X] T042 [US2] **Verify GREEN** — `go test ./internal/glance/...` — and confirm `openstack image set --min-disk 10` changes the value, since that CLI path is what sends `add`

**Checkpoint**: `image set` writes what it says it wrote.

---

## Phase 5: User Story 3 — A project cannot exceed its block-storage quota (Priority: P1)

**Goal**: every path that adds to a project's block-storage consumption consults the quota, refuses
with `413` when it would exceed, and leaves nothing behind.

**Independent Test**: set a project's volume limit to its current usage, then attempt one more volume
through each adding path. See [contracts/block-storage-quota.md](./contracts/block-storage-quota.md).

**This is the largest story in the feature**: eight call sites across four files, plus a usage-counter
correction with a user-visible consequence.

### Tests for User Story 3 ⚠️ WRITE FIRST, CONFIRM RED

- [X] T043 [P] [US3] Per-resource refusal test in `internal/cinder/quotas_test.go` — at the limit for each of `volumes`, `gigabytes`, `snapshots`, `backups`, `backup_gigabytes`, the corresponding creation answers `413` with code `overLimit` naming the resource (FR-018, FR-020, FR-028, FR-029)
- [X] T044 [P] [US3] Per-resource success test in `internal/cinder/quotas_test.go` — one below each limit, creation succeeds, so the refusal test is not passing by refusing everything
- [X] T045 [P] [US3] Per-volume-ceiling test in `internal/cinder/quotas_test.go` — a single request larger than `per_volume_gigabytes` is refused on that ground even when the project's total allowance permits it (FR-024)
- [X] T046 [P] [US3] Boundary test in `internal/cinder/quotas_test.go` — a request landing exactly *at* the limit succeeds and the next one is refused (off-by-one in both directions)
- [X] T047 [P] [US3] Default-limits test in `internal/cinder/quotas_test.go` — a project with no `cinder_quotas` row is enforced against the package defaults, and against the same values the quota views report (FR-025)
- [X] T048 [P] [US3] Boot-path test in `internal/nova/boot_from_volume_test.go` — a boot-from-new-volume request for a project at its limit is refused, and no volume exists afterwards (FR-019, FR-022, FR-029). **This is the one no handler-level test can see**
- [X] T049 [P] [US3] Backup-restore path test in `internal/cinder/quotas_test.go` — restoring a backup into a new volume for a project at its limit is refused and inserts nothing (FR-019)
- [X] T050 [P] [US3] Managed-import path test in `internal/cinder/quotas_test.go` — importing an unmanaged backing store for a project at its limit is refused (FR-019)
- [X] T051 [P] [US3] Transfer-accept path test in `internal/cinder/quotas_test.go` — accepting a transfer for a project at its limit is refused, **the transfer stays pending**, the volume remains with its sender, and the transfer is still acceptable after the limit is raised (FR-019, FR-019a)
- [X] T052 [P] [US3] Extend test in `internal/cinder/quotas_test.go` — `os-extend` is enforced against the gigabytes **delta**, not the resulting size, and is refused when the delta does not fit (FR-028)
- [X] T053 [P] [US3] Snapshot-gigabytes counting test in `internal/cinder/quotas_test.go` — a 5 GB volume plus a snapshot of it reports 10 GB used, not 5, and the same figure appears in the quota-set view **and** the absolute-limits view (FR-026, FR-026a)
- [X] T054 [P] [US3] Snapshot-fills-allowance test in `internal/cinder/quotas_test.go` — a snapshot of a volume whose size alone exhausts the remaining gigabytes is refused
- [X] T055 [P] [US3] Volume-with-snapshots deletion test in `internal/cinder/quotas_test.go` — deleting a volume that has snapshots releases the volume's gigabytes and not the snapshots', which survive it
- [X] T056 [P] [US3] Concurrency test in `internal/cinder/quotas_test.go` — two simultaneous creations against a single remaining allowance yield exactly one volume (FR-023)
- [X] T057 [P] [US3] Immediate-effect test in `internal/cinder/quotas_test.go` — a limit raised by an administrator applies to the next request with no restart (FR-027)
- [X] T058 [P] [US3] Existing-overage test in `internal/cinder/quotas_test.go` — a project already past its allowance keeps what it has, further additions are refused, and nothing is deleted
- [X] T059 [P] [US3] Denial-logging test in `internal/cinder/quotas_test.go` — a quota denial emits an event naming project, resource, limit and requested amount (FR-029a)
- [X] T060 [US3] **Confirm RED** — every test above except T044 must fail, since nothing enforces anything today. Record output in `docs/specs/025-followup/BASELINE.md`

### Implementation for User Story 3

- [X] T061 [US3] Add the `ctx`-shaped check helper to `internal/cinder/quotas.go` — `(ctx, projectID, resource, amount)` wrapping `database.WithTx` with `SELECT ... FOR UPDATE` on the project's quota row, mirroring `nova.CheckQuota`'s interior. The signature differs from Nova's deliberately: the compute boot path has no `*gin.Context` (research R5)
- [X] T062 [US3] Resolve limits inside the helper through the existing `resolveCinderQuotaLimits` in `internal/cinder/quotas.go`, including the package defaults for a project with no row, so enforcement and reporting cannot diverge (FR-025)
- [X] T063 [US3] Handle the no-quota-row case in `internal/cinder/quotas.go` so the defaults path still serialises concurrent requests — there is no row to lock, and the concurrency guarantee must not hold only for projects an administrator has touched (FR-023)
- [X] T064 [US3] Correct the `gigabytes` count in `cinderQuotaUsage` in `internal/cinder/quotas.go` to sum volume size **and** snapshot size, matching upstream's "total gigabytes for volumes and snapshots" (FR-026). `snapshots.size_gb` already exists and is `NOT NULL`
- [X] T065 [US3] Confirm the corrected count reaches all three reporting surfaces — `internal/cinder/quotas.go:131`, `internal/cinder/quotas.go:219` and `internal/cinder/volumes.go:2181` all read the shared resolution (FR-026a)
- [X] T066 [US3] Add a per-volume-ceiling check for `per_volume_gigabytes` in `internal/cinder/quotas.go`, evaluated per request and independent of the project's remaining total (FR-024)
- [X] T067 [US3] Call the check at the top of `provisionVolume` in `internal/cinder/volumes.go`, **before** the backing store is created — placed there, the two paths through it (`CreateVolume` and `CreateBootVolumeFromImage`) need no rollback at all (FR-021, research R5)
- [X] T068 [US3] Call the check before the `INSERT INTO volumes` on the backup-restore path in `internal/cinder/backups.go` (FR-019)
- [X] T069 [US3] Call the check before the `INSERT INTO volumes` in `ManageVolume` in `internal/cinder/manage.go` (FR-019)
- [X] T070 [US3] Call the check in `AcceptVolumeTransfer` in `internal/cinder/transfers.go`, refusing without consuming the transfer — nothing is created here, so the obligation is to leave the sender's state intact (FR-019a)
- [X] T071 [US3] Call the check on the `os-extend` action in `internal/cinder/volumes.go` for the gigabytes **delta** (FR-028)
- [X] T072 [US3] Call the check in `CreateSnapshot` in `internal/cinder/volumes.go` against `snapshots` and `gigabytes` (FR-028a)
- [X] T073 [US3] Call the check in `CreateBackup` in `internal/cinder/backups.go` against `backups` and `backup_gigabytes` (FR-028a)
- [X] T074 [US3] Re-derive the call-site list from the code one final time — `grep -n "INSERT INTO volumes\|INSERT INTO snapshots\|INSERT INTO volume_backups" internal/cinder/*.go` — and confirm every one is covered. Planning found four volume-inserting routes where the spec named two; a route added later and left unchecked reproduces the defect exactly (FR-019)
- [X] T075 [US3] Map the refusal to `common.NewQuotaExceededError` at every call site — `internal/cinder/volumes.go`, `internal/cinder/backups.go`, `internal/cinder/manage.go`, `internal/cinder/transfers.go` and `internal/nova/handlers.go` — so each path answers the same `413` / `overLimit` shape naming the resource (FR-020)
- [X] T076 [US3] Refuse the boot-from-new-volume case pre-flight in `internal/nova/handlers.go`, reusing SPEC-025's existing pre-flight refusal shape, so no server is left implying a volume exists (FR-022)
- [X] T077 [US3] Log the denial in `internal/cinder/quotas.go` naming project, resource, limit and requested amount — and log **only** this; the client-error refusals elsewhere in this feature stay out of the log (FR-029a)
- [X] T078 [US3] **Verify GREEN** across all eight call sites — `go test ./internal/cinder/... ./internal/nova/...`

**Checkpoint**: no route adds block storage past a project's allowance.

---

## Phase 6: User Story 4 — Reach the deployment under the name it was addressed by (Priority: P2)

**Goal**: the served catalog names the host the client addressed, and no client is handed a catalog
derived from another client's request.

**Independent Test**: request a token under a hostname other than the configured one and inspect the
catalog. See [contracts/service-catalog-addressing.md](./contracts/service-catalog-addressing.md).

### Tests for User Story 4 ⚠️ WRITE FIRST, CONFIRM RED

- [X] T079 [P] [US4] Request-host test in `internal/keystone/catalog_host_test.go` — a token requested under a host other than `LIGHTSTACK_ENDPOINT_HOST` yields a catalog naming the addressed host, with each endpoint keeping its own port (FR-030, FR-031, FR-034)
- [X] T080 [P] [US4] Two-port regression test in `internal/keystone/catalog_host_test.go` — a request host carrying a port (`lightstack.example.com:35357`) must not produce `http://lightstack.example.com:35357:8774`
- [X] T081 [P] [US4] Cross-caller cache test in `internal/keystone/catalog_host_test.go` — two catalogs requested under different hosts in succession, within one project and with the cache enabled, and neither is served the other's (FR-031a, FR-034)
- [X] T082 [P] [US4] Fallback test in `internal/keystone/catalog_host_test.go` — a request carrying no usable host still yields absolute, parseable endpoints, falling back to the configured host and then to `localhost` (FR-032)
- [X] T083 [P] [US4] Object-storage test in `internal/keystone/catalog_host_test.go` — the externally configured `s3` endpoint is served as configured, unaffected by the host rule (FR-033)
- [X] T084 [US4] **Confirm RED** — T079, T081 must fail; T082 and T083 are controls and should pass. Record output in `docs/specs/025-followup/BASELINE.md`

### Implementation for User Story 4

- [X] T085 [US4] Thread the resolved host into `BuildServiceCatalog` in `internal/keystone/auth.go` — change the signature and update both call sites (`auth.go:364`, `auth.go:548`), each of which already holds the request context
- [X] T086 [US4] Apply the precedence in `substituteURLTemplates` in `internal/keystone/auth.go` — request host, then `LIGHTSTACK_ENDPOINT_HOST`, then `localhost` — keeping the existing prefix-replacement approach, which is what preserves each endpoint's own port (research R7)
- [X] T087 [US4] Strip the port from the request host using the Phase 2 helper before substituting, in `internal/keystone/auth.go` (FR-031, and the failure T080 pins)
- [X] T088 [US4] Apply the same rule to `buildHardcodedCatalog` in `internal/keystone/auth.go`, so the fallback catalog cannot serve a differently-addressed one
- [X] T089 [US4] Include the resolved host in the catalog cache key in `internal/keystone/auth.go` (`auth.go:591` read, `auth.go:691` write) and correct the now-false comment asserting the catalog is immutable (FR-031a)
- [X] T090 [US4] **Verify GREEN**, and record in `docs/CONFIGURATION.md` that `X-Forwarded-Host` is deliberately **not** trusted (FR-031b) together with the `proxy_set_header Host $host` requirement a proxy deployment needs

**Checkpoint**: a client that can reach the deployment can use its catalog.

---

## Phase 7: User Story 5 — `restore` answers as upstream does (Priority: P3)

**Goal**: `restore` stays routed with upstream's answers (404/409); deferred delete is not introduced.
*(Revised 2026-09-30 — the tasks below first withdrew the action; see spec.md, Session 2026-09-30.)*

**Independent Test**: invoke `restore` on a live and on a deleted server; confirm 409 and 404.
See [contracts/server-actions.md](./contracts/server-actions.md).

### Tests for User Story 5 ⚠️ WRITE FIRST, CONFIRM RED

- [X] T091 [P] [US5] Upstream-answer test in `internal/nova/restore_action_test.go` — `{"restore": null}` receives 409 for a live server and 404 for a deleted one, never the unknown-action 400 (FR-035, FR-037)
- [X] T092 [P] [US5] *(Superseded 2026-09-30.)* Asserted `restore` was absent from the action dispatch table; it stays routed (FR-035), and T091's 409 is what proves the route
- [X] T093 [P] [US5] Deletion-unchanged test in `internal/nova/restore_action_test.go` — deletion semantics are identical before and after the change (FR-036)
- [X] T094 [US5] **Confirm RED** — T091 must fail (the withdrawal answered 400 "unknown action"). Record output in `docs/specs/025-followup/BASELINE.md`

### Implementation for User Story 5

- [X] T095 [US5] Reduce `RestoreInstance` in `internal/nova/advanced_actions.go` to upstream's 409 (the dispatcher already answers 404) and keep its dispatch-table entry, leaving deletion semantics untouched (FR-035, FR-036)
- [X] T096 [US5] Confirm `SOFT_DELETED` no longer appears anywhere — `grep -rn "SOFT_DELETED" internal/ --include='*.go'` must return nothing, since the removed comparison was its only occurrence
- [X] T097 [US5] **Verify GREEN** — `go test ./internal/nova/...` — and confirm no other advertised action's answer changed (FR-037)

**Checkpoint**: the advertised action list contains only reachable actions.

---

## Phase 8: User Story 6 — Negotiate a microversion and get what was negotiated (Priority: P3)

**Goal**: negotiation is read in one place, the applied version is reported, an out-of-range version
is refused, and the behaviours the product emits are gated where upstream gates them.

**Independent Test**: request a response at an older negotiated version and at the current one.
See [contracts/compute-microversions.md](./contracts/compute-microversions.md).

**⚠️ Every test in this phase must negotiate EXPLICITLY.** A request with no header resolves to the
product's *maximum* (upstream uses the minimum; FR-040 requires Lightstack keep its behaviour), so a test
relying on the default header exercises the maximum and proves nothing about a gate (research R4).

### Tests for User Story 6 ⚠️ WRITE FIRST, CONFIRM RED

- [X] T098 [P] [US6] Out-of-range test in `internal/nova/microversion_test.go` — a negotiated version above the advertised maximum answers `406` (FR-041)
- [X] T099 [P] [US6] Malformed-header test in `internal/nova/microversion_test.go` — a header that does not parse is treated as no negotiation and the request **succeeds**, preserving SPEC-025's A-006 decision (FR-041a)
- [X] T100 [P] [US6] Header-precedence test in `internal/nova/microversion_test.go` — `OpenStack-API-Version` wins when both headers are present, and `latest` resolves to the maximum
- [X] T101 [P] [US6] Applied-version test in `internal/nova/microversion_test.go` — the response header names the version actually applied, not a constant (FR-038)
- [X] T102 [P] [US6] No-header test in `internal/nova/microversion_test.go` — a request with no version header resolves to the advertised **maximum** and receives today's behaviour unchanged (FR-040, FR-040a), pinning the divergence deliberately so a future change cannot make it silently
- [X] T103 [US6] **Confirm RED** — T098 and T101 must fail; T099, T100 and T102 are controls and should pass. Record output in `docs/specs/025-followup/BASELINE.md`

### Implementation for User Story 6

- [X] T104 [US6] Audit the compute response fields the product emits — the serializers in `internal/nova/handlers.go` and `internal/nova/advanced_actions.go` — against the microversion at which upstream introduced each, working through research R6's candidate list and confirming every version against upstream's `rest_api_version_history`. The list is the enumeration to verify, not the answer (FR-042)
- [X] T105 [US6] Record the audit outcome in `docs/specs/025-followup/microversion-audit.md` — every behaviour checked, the version upstream introduced it at, whether a gate was added, and for anything left ungated, why (FR-043)
- [X] T106 [US6] Record both deliberate divergences in `docs/specs/025-followup/microversion-audit.md`: the no-header default resolving to the maximum (FR-040a, research R4) and the tolerated malformed header (FR-041a, A-006), so the advertised `min_version`'s limits are stated rather than rediscovered (FR-043)
- [X] T107 [US6] Route negotiation through the single helper in `internal/nova/microversion.go` and report the applied version in the response (FR-038)
- [X] T108 [US6] Refuse an out-of-range version in `internal/nova/microversion.go` using the Phase 2 `406` constructor, leaving the unparseable case tolerated (FR-041, FR-041a)
- [X] T109 [US6] Add a gate for each behaviour the audit found to be introduced part-way through the advertised range, in the emitter that produces it — `internal/nova/handlers.go` and `internal/nova/advanced_actions.go` — reading the negotiated version through `internal/nova/microversion.go` only (FR-039)
- [X] T110 [US6] Add a boundary test per gate introduced in `internal/nova/microversion_test.go`, negotiating explicitly on both sides (FR-044)
- [X] T111 [US6] Correct the comments that claim gates which do not exist — `createImage`'s note about 2.45 in `internal/nova/advanced_actions.go` among them — and update `internal/nova/microversion.go`'s header comment, which currently states this is the product's only gate (FR-043)
- [X] T112 [US6] **Verify GREEN** — `go test ./internal/nova/... ./internal/common/...`

**Checkpoint**: a client that negotiates gets what it negotiated; one that does not is unaffected.

---

## Phase 9: User Story 7 — A server listing does not slow down in proportion to the servers in it (Priority: P3)

**Goal**: one address query per listing rather than one per server, with byte-identical output.

**Independent Test**: list a project with several servers and assert the address map matches the
per-server view while the query count is independent of the server count.
See [contracts/server-list-addresses.md](./contracts/server-list-addresses.md).

### Tests for User Story 7 ⚠️ WRITE FIRST, CONFIRM RED

- [X] T113 [P] [US7] Equivalence test in `internal/nova/addresses_test.go` — each server's address map in a detailed listing is identical to what the per-server view reports, floating IPs included (FR-046)
- [X] T114 [P] [US7] Round-trip-count test in `internal/nova/addresses_test.go` — the number of address queries does not grow with the number of servers, asserted rather than assumed (FR-045, FR-048)
- [X] T115 [P] [US7] Empty-map test in `internal/nova/addresses_test.go` — a server with no ports reports an empty address map, not `null` and not a missing key (FR-047)
- [X] T116 [P] [US7] Attribution test in `internal/nova/addresses_test.go` — in a cross-tenant administrative listing, each address is attributed to its own server, and a port whose project disagrees with its instance's contributes nothing
- [X] T117 [US7] **Confirm RED** — T114 must fail (N queries today); T113, T115 and T116 are controls and should pass, since this story must not change output. Record output in `docs/specs/025-followup/BASELINE.md`

### Implementation for User Story 7

- [X] T118 [US7] Add a batched address lookup in `internal/nova/handlers.go` taking the page's instance ids, returning `device_id` alongside each row so results can be bucketed by server, and keeping the existing `floating_ips` LEFT JOIN with `ARRAY_AGG ... FILTER` and `GROUP BY p.id` unchanged — that aggregation is B17's fix (FR-046)
- [X] T119 [US7] Preserve the port-to-instance project pairing in the batched query via `JOIN instances i ON i.id = p.device_id AND i.project_id = p.project_id` in `internal/nova/handlers.go`. Dropping the predicate would let a mismatched port contribute an address to the wrong server (research R8)
- [X] T120 [US7] Replace the per-row `getInstanceAddresses` call in the detail listing in `internal/nova/handlers.go` with a lookup into the batched map, keeping `getInstanceAddresses` for the single-server view
- [X] T121 [US7] Promote `ip` from the refused set to the honoured set in `internal/nova/server_filter.go`, using a `ports` subquery on `device_id` now that this story's query exists — `ip` is in upstream's general-user filter set and must never be silently ignored (research R1, FR-003, FR-004)
- [X] T122 [US7] Update the `ip` test in `internal/nova/server_filter_test.go` from asserting a refusal to asserting the filter narrows, with a non-matching address returning empty (FR-009)
- [X] T123 [US7] **Verify GREEN** — `go test ./internal/nova/...`

**Checkpoint**: listing cost is flat in the server count, and `ip` is honoured rather than refused.

---

## Phase 10: Polish & Cross-Cutting Concerns

- [ ] T124 [P] Extend `test/nova_gaps_test.sh` with the filter, microversion and address checks — each with a negative assertion, and each written so it cannot pass against the pre-change binary
- [ ] T125 [P] Extend `test/quota_test.sh` with per-resource refusal across the eight call sites, including the boot path and the transfer-pending case
- [ ] T126 [P] Extend `test/spec007_catalog_scheme_test.sh` with a catalog requested under a host other than the configured one, and the two-hosts-in-succession cache check
- [X] T127 [P] Add release-note entries in `docs/` for the two user-visible changes with no user action behind them: block-storage quota begins being enforced where it never was, and reported `gigabytes` usage rises for any project holding snapshots (FR-026). They should be read together
- [X] T128 [P] Update `docs/CONFIGURATION.md` — the `LIGHTSTACK_ENDPOINT_HOST` constraint documented for SPEC-025 is now a fallback rather than a requirement, and the forwarded-header decision from T090 belongs beside it
- [X] T129 [P] Remove the `LIGHTSTACK_ENDPOINT_HOST` workaround note from `test/nova_gaps_test.sh` if US4 makes it unnecessary, rather than leaving a stale explanation of a fixed problem
- [X] T130 Generate the real-mode host runbook at `docs/specs/025-followup/HOST_TESTING_REAL_MODE.md` — five of the seven findings were found on a host and none by a contract suite, so every story needs a host step with a desired outcome
- [X] T131 Run the full gate set: `make fmt`, `make lint`, `go test ./...`, `./test/nova_gaps_test.sh`, `./test/quota_test.sh`, `./test/spec007_catalog_scheme_test.sh`. No migration to apply — this feature adds none. Confirms SC-013 and SC-014
- [X] T132 Walk `quickstart.md` end to end against a running deployment, confirming each "Now" block no longer reproduces — **superseded and exceeded by the real-mode host pass**: R1–R9 cover every quickstart reproduction against a real deployment, and the results are recorded in BASELINE.md. This is the measurable-outcome pass: it confirms SC-001 through SC-012 — the filter narrowing (SC-001, SC-002), the image property sticking (SC-003, SC-004), the quota holding on every route (SC-005, SC-006, SC-007, SC-008), the full command sequence under any resolvable address (SC-009), the reachable action list (SC-010), the negotiated microversion (SC-011) and the flat listing cost (SC-012). Record which criterion each step evidences
- [X] T133 Audit every new test for the failure mode this feature exists to fix — a check that cannot fail. Walk `internal/nova/server_filter_test.go`, `internal/nova/addresses_test.go`, `internal/nova/microversion_test.go`, `internal/glance/images_test.go`, `internal/cinder/quotas_test.go`, `internal/keystone/catalog_host_test.go` and the three `test/*.sh` scripts, and for each check confirm it was seen RED against `BASELINE.md` or is explicitly labelled a control. Nine of SPEC-025's verification steps asserted nothing and were caught only during its host pass (SC-013)
- [ ] T134 Branch review before commit

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: no dependencies. T002 in particular should run before any quota test is trusted
- **Foundational (Phase 2)**: depends on Setup. **Blocks US4 (T087 needs T005) and US6 (T108 needs
  T003)**. Does not block US1, US2, US3 or US7
- **User Stories (Phases 3–9)**: independent of each other, with one exception below
- **Polish (Phase 10)**: depends on the stories it validates

### User Story Dependencies

- **US1 (P1)**: independent. MVP
- **US2 (P1)**: independent — different service, different file
- **US3 (P1)**: independent. Largest by a wide margin
- **US4 (P2)**: needs T005/T006 from Foundational
- **US5 (P3)**: independent. Smallest change in the feature
- **US6 (P3)**: needs T003/T004 from Foundational
- **US7 (P3)**: independent to build, but **coupled to US1 in one direction**: T121/T122 promote
  `ip` from refused to honoured, and they belong to US7 because they need its query. If US7 is
  dropped, `ip` stays refused and US1 remains correct — which is why it is refused in T021 rather
  than left pending (FR-003b states both states). **File conflict**: T121/T122 edit
  `internal/nova/server_filter.go` and `internal/nova/server_filter_test.go`, which are US1's files
  — so they must land after T021/T024, not beside them

### Within Each Story

Tests → confirm RED → implementation → verify GREEN. The RED step is not optional: for US1 and US3 the
*positive* tests pass against today's code, so only the negative ones distinguish a fix from a no-op.

### Parallel Opportunities

- T003–T006 (Foundational) are two independent files: `errors.go` and `baseurl.go`
- Every story's test tasks are marked [P] — different test files, or different functions in one file
- US1, US2, US3 and US7 can be built concurrently: `nova/server_filter.go`, `glance/images.go`,
  `cinder/*.go` and `nova/handlers.go` respectively. **US1 and US7 overlap in two files**:
  `nova/handlers.go` (T022/T023 against T118/T120) and `nova/server_filter.go` (T021 against T121),
  so those pairs must not run concurrently
- US5 touches only `advanced_actions.go` and one dispatch-table line, and can be done at any point by anyone
- T124–T129 in Polish are all different files

## Parallel Example: User Story 3 tests

```bash
# The per-path tests are independent — different call sites, same test file, different functions:
Task: "T048 Boot-path refusal test in internal/nova/boot_from_volume_test.go"
Task: "T049 Backup-restore path test in internal/cinder/quotas_test.go"
Task: "T050 Managed-import path test in internal/cinder/quotas_test.go"
Task: "T051 Transfer-accept path test in internal/cinder/quotas_test.go"
Task: "T052 Extend delta test in internal/cinder/quotas_test.go"
```

## Implementation Strategy

### MVP First (User Story 1 only)

1. Phase 1 Setup — know the baseline, and check the shared database's fixtures
2. Phase 2 Foundational — not strictly needed for US1, but it is six small tasks and unblocks two later stories
3. Phase 3 US1 — filters honoured, unsupported ones refused
4. **STOP and VALIDATE**: `openstack server list --name X` narrows; a non-matching name returns nothing

US1 alone is a defensible increment: it closes the highest-severity finding in the set, the one where
a wrong-but-plausible listing hands back another server's state.

### Incremental Delivery

1. Setup + Foundational
2. US1 → the wrong-rows defect closes (MVP)
3. US2 → `image set` writes what it reports
4. US3 → the exhaustion path closes. Ship the release notes **with** this one, not after
5. US4 → per-command endpoint overrides work
6. US5 → `restore` keeps upstream's answers
7. US6 → negotiation is honoured where the product emits gated behaviour
8. US7 → listing cost flattens, and `ip` becomes a real filter

### Suggested Grouping If Staffed

- **One person on US3 alone.** Eight call sites, four files, a counter correction with a visible
  consequence, and the only concurrency requirement in the feature
- **One person on US1 + US7**, since they share `nova/handlers.go` and the `ip` decision
- **One person on US2 + US4 + US5 + US6**, which are four unrelated small changes in three services

## Notes

- **No migration.** Nothing to apply, nothing to reverse
- **No new Go modules.** One new shared helper (the 406 constructor)
- Every defect here answers `2xx` today. A test that only exercises the working path is not evidence
- Commit per task or per logical group; stop at any checkpoint to validate a story on its own
- T133 exists because the most likely way this feature is delivered wrong is a full suite of green
  tests that could never have been red
