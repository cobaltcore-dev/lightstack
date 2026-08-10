# Tasks: Tenant Scoping & Authorization Model

**Input**: Design documents from `docs/specs/023-tenant-scoping-authz/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md
**Branch**: `feature/549` | **Base**: `feature/545` @ `9992ebf`

**Tests**: MANDATORY. Constitution Article VII makes test-first non-negotiable, and spec FR-022 requires every behaviour change to be demonstrated failing before and passing after. Test tasks precede their implementation within every phase.

**Organization**: Grouped by user story. Six stories, all independently implementable and testable.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: parallelizable — different file, no dependency on an incomplete task
- **[Story]**: US1–US6, mapping to spec.md
- Every task carries an exact file path

## Path conventions

Single Go binary, six embedded services. Source under `internal/`, tests under `test/`, docs under `docs/`.

---

## Phase 1: Setup

**Purpose**: Capture the pre-change baseline so every later claim is measured, not asserted.

- [X] T001 Record the current test baseline to `docs/specs/023-tenant-scoping-authz/BASELINE.md`: run `make test`, `go test ./test/contract/...`, `./test/quick_test.sh`, `./test/multi_tenant_test.sh` and `make test-all`, capturing pass/fail counts. `make test-all` is expected RED on B10 (#546) — record it so this feature is not blamed for a pre-existing failure.
- [X] T002 [P] Record the static baseline in the same `BASELINE.md`: `grep -c 'HasRole' internal/nova/*.go` (4), `grep -c 'project_id = \$' internal/nova/*.go` (102), `grep -rn 'all_tenants' --include='*.go' .` (0), `grep -c 'HasRole\|Forbidden' internal/nova/aggregates.go` (0).
- [X] T003 [P] Verify the stub stack comes up per `quickstart.md` and that `docker compose -f deployments/docker-compose-horizon.yml up -d --build` succeeds.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: The two shared helpers every story-level change calls. Nothing in Phase 3 or 4 can start until T007 lands.

**⚠️ BLOCKING**: T004–T007 gate User Stories 1 and 6. Stories 2, 3 and 5 do not depend on this phase.

- [X] T004 [P] Write unit tests for the cross-tenant query flag in `internal/common/query_test.go`, covering the full table in research R2: absent → false, present-and-empty → **true**, `1 t true on y yes` (any case) → true, `0 f false off n no` → false, unrecognised → false.
- [X] T005 [P] Write unit tests for the scope-derived override helper in `internal/nova/authz_test.go`: an administrator resolves any project's instance; a non-administrator resolves only their own; a non-administrator asking for a foreign instance gets the same not-found signal as one asking for an absent instance (contract C2, research R10).
- [X] T006 [P] Implement `QueryFlag(c *gin.Context, name string) bool` in `internal/common/query.go` per research R2 (FR-005a). `strconv.ParseBool` is insufficient — it rejects `on`/`yes`/`y`/`no`/`off` and cannot express the valueless form. Document why, citing the Nova API reference.
- [X] T007 Implement the scope-derived override helper in `internal/nova/authz.go`, generalised from `lookupConsoleInstance` (`internal/nova/console.go:189`) — FR-001, FR-002. It MUST return the **owning project** (not a boolean) because FR-007 needs it for attribution, and MUST return `pgx.ErrNoRows` for both "absent" and "someone else's".

**Checkpoint**: helpers exist and are unit-tested. Per the Constitution's Test-First workflow (step 2, "get approval — review test strategy"), confirm the contract set in `contracts/` and the RED tests written so far are the agreed strategy before the implementation waves begin. User Stories 1 and 6 can then start.

---

## Phase 3: User Story 1 — An operator administers any tenant's compute resources (Priority: P1) 🎯 MVP

**Goal**: An operator holding `admin` can read and act on any tenant's server, without membership of that tenant's project and without database access.

**Independent Test**: Per `quickstart.md` US1 — two projects, two non-administrative users, a server owned by B. Every compute operation answered for the operator; every one still refused for a non-administrative member of A, and refused without revealing the server exists.

### 3a — Tests (write first, confirm RED)

- [X] T008 [P] [US1] Revive `test/contract/nova/rbac_test.go.broken` as `test/contract/nova/rbac_test.go`, fixing the gophercloud v1.14.1 signature drift: `resetstate.ResetStateOpts` → `resetstate.ResetState(client, id, resetstate.StateError)` (research R9). Confirm it compiles and runs.
- [X] T009 [US1] Extend `test/contract/nova/rbac_test.go` with a **second project's** server, so the administrator path exercises a project the administrator does not own. Without this the existing assertions pass today and prove nothing.
- [X] T010 [P] [US1] Write contract tests for C1–C3 in `test/contract/nova/admin_override_test.go`: administrator reads and acts on another project's server; a non-administrative member gets the same answer for a foreign server as for an absent one (FR-003).
- [X] T011 [P] [US1] Write contract tests for L1–L4 in `test/contract/nova/all_tenants_test.go`: default listing unchanged; every accepted `all_tenants` spelling including the bare and empty forms; every rejected spelling; a non-administrator's use silently ignored and byte-identical to the same request without it (FR-005a, FR-005b).
- [X] T012 [US1] Write contract tests for C6 (name matching never spans projects) and C7 (attribution against the owning project) in `test/contract/nova/admin_override_test.go`.
- [X] T013 [US1] Confirm all of T008–T012 are RED and record the output in `BASELINE.md`.

### 3b — FR-008a: the two broken administrator-only actions (independently shippable)

**This block carries no policy decision and can merge on its own.** It repairs two operations that gate on `admin` and then scope to the caller's project, so an administrator is refused `404` for exactly the instances they exist to handle.

- [X] T014 [US1] Fix `EvacuateInstance` in `internal/nova/advanced_actions.go:483` (FR-008a): keep the administrative gate, replace `SELECT status FROM instances WHERE id = $1 AND project_id = $2` with the unscoped lookup an administrator-only operation implies.
- [X] T015 [US1] Fix `ResetStateAction` in `internal/nova/advanced_actions.go:1221` (FR-008a): the existence check is scoped to the caller while the subsequent `UPDATE instances … WHERE id = $3` is not — make the check consistent with the gate.
- [X] T016 [US1] Confirm T008/T009 in `test/contract/nova/rbac_test.go` now pass for reset-state and evacuate (contract C4), and that a non-administrator still receives `403` on both.

### 3c — The override across the server operations

- [X] T017 [US1] Resolve the effective project **once** in `ServerAction` (`internal/nova/handlers.go:1421`) and expose it to the ~25 sub-handlers it dispatches to, each of which currently re-reads `c.GetString("project_id")` independently. Resolving per sub-handler would mean 25 chances to diverge (FR-001).
- [X] T018 [US1] Apply the helper to the point lookups in `internal/nova/handlers.go`: `GetServer` (:1168), `DeleteServer` (:1244), `GetServerMetadata`, `UpdateServerMetadata`, `ResetServerMetadata`.
- [X] T019 [US1] Switch the action sub-handlers in `internal/nova/handlers.go` to the effective project from T017: `ForceDeleteInstanceAction`, `LockInstanceAction`, `UnlockInstanceAction`, `PauseInstanceAction`, `UnpauseInstanceAction`, `RebuildInstanceAction`, `RescueInstanceAction`, `CreateImageAction`.
- [X] T020 [US1] Switch the 17 action handlers in `internal/nova/advanced_actions.go` to the effective project: `AddSecurityGroup`, `RemoveSecurityGroup`, `ChangePassword`, `ConfirmResizeInstance`, `RevertResizeInstance`, `CreateBackupAction`, `LiveMigrateInstance`, `MigrateInstance`, `ResetNetworkAction`, `ResizeInstance`, `RestoreInstance`, `ResumeInstance`, `ShelveInstance`, `SuspendInstance`, `UnshelveInstance` (plus the two already done in T014/T015).
- [X] T021 [P] [US1] Apply the helper in `internal/nova/tags.go` — `ListServerTags`, `ReplaceServerTags`, `DeleteAllServerTags`, `AddServerTag`, `DeleteServerTag`.
- [X] T022 [P] [US1] Apply the helper in `internal/nova/diagnostics.go` — `GetServerDiagnostics`, `ListInstanceActions`, `GetInstanceAction`.
- [X] T023 [P] [US1] Apply the helper in `internal/nova/volume_attachment.go` — `ListVolumeAttachments`, `GetVolumeAttachment`, `AttachVolume`, `DetachVolume`.
- [X] T024 [P] [US1] Apply the helper in `internal/nova/interface_attach.go` — `ListInterfaceAttachments`, `AttachInterface`, `DetachInterface`.
- [X] T025 [P] [US1] Apply the helper in `internal/nova/server_update.go` — `UpdateServer`.
- [X] T026 [US1] Rewrite `lookupConsoleInstance` (`internal/nova/console.go:189`) to delegate to the shared helper, so the console stops being a special case (FR-001). Replace its string literal `"admin"` at `:191` with `common.RoleAdmin`, which every other site uses.
- [X] T027 [US1] Widen the ports sub-query in `getInstanceAddresses` (`internal/nova/handlers.go`) to the effective project. It is scoped to the **caller**, so widening only the server lookup would return a server with an empty `addresses` dict — a server that appears to have no network.
- [X] T028 [US1] Confirm `CreateServer` (`internal/nova/handlers.go`) is **unchanged** and cannot create into another project (FR-004a, contract C5).

### 3d — Name matching, listings and attribution

- [X] T029 [US1] Keep the project predicate on **name** matching even for administrators, in `DeleteServer` (`internal/nova/handlers.go:1244`) and `ServerAction` (`:1421`), which match `id::text = $1 OR name = $1`. A name miss must stay "not found" and MUST NOT fall through to a cross-project search (FR-005c, FR-005d, contract C6).
- [X] T030 [US1] Implement `all_tenants` in `ListServers` (`internal/nova/handlers.go:906`) using `common.QueryFlag`, gated on `common.HasRole(c, common.RoleAdmin)`. A non-administrator's use is silently ignored, never refused (FR-005, FR-005a, FR-005b).
- [X] T031 [US1] Implement `all_tenants` in `ListServersDetail` (`internal/nova/handlers.go:977`) the same way (FR-005).
- [X] T032 [US1] Add `id` as a tiebreaker to the ordering in `ListServers` and `ListServersDetail` in `internal/nova/handlers.go` (`ORDER BY created_at DESC, id DESC`) and make the marker comparison composite. `created_at` is not unique, so page boundaries are already non-deterministic and widening across projects makes ties likelier (research R3, contract L5).
- [X] T033 [US1] Drop the project predicate from both marker lookups in `internal/nova/handlers.go` when the cross-tenant view is active — `ListServers` resolves its marker via `ROW_NUMBER()`, `ListServersDetail` via a `created_at` value, and both currently ignore a failed resolution and silently return page 1 (contract L6).
- [X] T034 [US1] Include the owning project on each entry in `ListServers` in `internal/nova/handlers.go` when a listing spans projects — `ListServers` currently returns only `id`, `name` and `links` (FR-006, contract L4).
- [X] T035 [US1] Record instance actions against the **owning** project in `internal/nova/handlers.go:621`, passing the owner rather than the caller. No schema change: `instance_actions.project_id` is already `UUID NOT NULL` (FR-007).
- [X] T036 [US1] Extend the instance-action write path in `internal/nova/` beyond the single creation site at `internal/nova/handlers.go:621` — today there is exactly one insert site, so an administrator deleting or resizing a tenant's server leaves no trace and FR-007 is satisfied only vacuously. Use upstream's action vocabulary (research R6) so `os-instance-actions` stays client-compatible (FR-007a).
- [X] T037 [US1] Log refused cross-tenant compute attempts as a `Warn` naming caller project, target and owner, mirroring `internal/neutron/network.go:964`. A permitted access by the owner logs nothing (FR-007b, contract C8).
- [X] T038 [US1] Confirm T008–T012 are now GREEN and record RED→GREEN evidence in `BASELINE.md`.

**Checkpoint**: User Story 1 complete and independently demonstrable via `quickstart.md`.

---

## Phase 4: User Story 6 — Operator-only surfaces reject tenants (Priority: P1)

**Goal**: Surfaces that describe or reshape the deployment require the administrative role. Contains the feature's only privilege escalation.

**Independent Test**: Per `quickstart.md` US6 — as a non-administrative tenant, every aggregate operation and the deployment-wide migration listing is refused; as an operator, each succeeds unchanged.

**Depends on**: T007 for the per-server migration handlers only. The two gates (T041, T042) depend on nothing.

### 4a — Tests

- [X] T039 [P] [US6] Write contract tests for O1–O3 in `test/contract/nova/operator_surfaces_test.go`: the migration list and all six aggregate routes refused for a non-administrator, admitted for an operator. Include the aggregate **create** — it currently succeeds for any tenant.
- [X] T040 [US6] Write contract tests for O4 in `test/contract/nova/operator_surfaces_test.go`: a non-administrative member of another project cannot read, act on or **delete** a server's migration records; the owner and an operator can.

### 4b — Implementation

- [X] T041 [US6] Gate `ListMigrations` (`internal/nova/migrations.go:14`) on `common.HasRole(c, common.RoleAdmin)`. Its own comment already records this is "an admin endpoint in real OpenStack" (FR-008e).
- [X] T042 [US6] Gate all nine handlers in `internal/nova/aggregates.go` — `ListAggregates` (:20), `CreateAggregate` (:81), `GetAggregate` (:127), `UpdateAggregate` (:177), `DeleteAggregate` (:243), `AggregateAction` (:266), `AddHostToAggregate` (:294), `RemoveHostFromAggregate` (:350), `SetAggregateMetadata` (:412). Gate at `AggregateAction`'s dispatcher rather than per action, so an unrecognised action name cannot bypass it (FR-008f, contract O3).
- [X] T043 [US6] Add an ownership check to the four per-server migration handlers in `internal/nova/migrations.go` — `ListServerMigrations` (:72), `GetServerMigration` (:132), `DeleteServerMigration` (:176), `ServerMigrationAction` (:201) — then apply the override from T007. These have **no** check today; this is the one in-scope change that tightens a per-server path for non-administrators (FR-008g, contract O4).
- [X] T044 [US6] Confirm the existing contract tests still pass unchanged — `test/contract/nova/migrations_list_test.go` and `aggregates_test.go` authenticate via `setupNovaClient` as `OS_USERNAME` defaulting to `admin` (contract O5).
- [X] T045 [US6] Confirm T039–T040 in `test/contract/nova/operator_surfaces_test.go` are GREEN and record RED→GREEN evidence in `docs/specs/023-tenant-scoping-authz/BASELINE.md`.

**Checkpoint**: no compute surface that describes or reshapes the deployment is reachable without the administrative role (SC-004b, SC-004c).

---

## Phase 5: User Story 2 — A domain-scoped authentication request gets an honest answer (Priority: P2)

**Goal**: A domain-scoped request is refused with `400` naming the scope, instead of `201` with a role-less token.

**Independent Test**: Per `quickstart.md` US2 — the ticket's own reproduction returns `400`; the refusal is identical for every domain named; `openstack token issue` still works.

**Depends on**: nothing. Fully parallel with Phases 3, 4, 6 and 8.

### 5a — Tests

- [X] T046 [P] [US2] Write contract tests for S1–S3 and S6–S7 in `test/contract/keystone/scope_test.go`: a domain scope is refused `400` and never answered with a success carrying neither the scope nor any roles (FR-010); the refusal is distinguishable from the `401` a bad credential gets; both the password and token re-scoping paths agree; a request naming two scope kinds is refused.
- [X] T047 [US2] Write the S2 uniformity test in `test/contract/keystone/scope_test.go`: a domain that exists, one that does not, the `default` alias, an unknown UUID and a malformed identifier all produce an identical status and body.
- [X] T048 [US2] Write the S4 regression guard in `test/contract/keystone/scope_test.go` — **the highest-risk test in the feature**. A project scope carrying a nested `domain` qualifier MUST still authenticate (FR-015). That form is what Horizon, Terraform and the CLI send on every request; catching it breaks every client.
- [X] T049 [US2] Write the S5 compatibility test in `test/contract/keystone/scope_test.go`: project-scoped by name, project-scoped by id, explicit `"unscoped"`, and no scope key at all are all answered exactly as today.
- [X] T050 [US2] Write the S8 backwards-compatibility test in `test/contract/keystone/scope_test.go`: issue a token **before** the change, apply the change, and confirm the token still validates and carries exactly the authority it had when issued (FR-017). Every live client holds one of these at deploy time.

### 5b — Implementation

- [X] T051 [US2] Add a top-level `Domain` member to `ScopeField` (`internal/keystone/auth.go:55`) — FR-009 — and decode it in `UnmarshalJSON` (`:68`), which currently discards everything but `project`. It is parsed **so it can be detected and refused**, never acted upon.
- [X] T052 [US2] Refuse a top-level domain scope in `AuthenticateToken` (`internal/keystone/handlers.go`), immediately after binding and **before** dispatching to either authentication path — one check satisfies the both-paths-agree requirement by construction (FR-009, FR-011, FR-013, FR-016). Do NOT refuse inside `UnmarshalJSON`: a bind failure becomes `400 "invalid request body: " + err`, conflating an unsupported scope with malformed JSON (research R5).
- [X] T053 [US2] Refuse **without resolving** the domain reference, in the `AuthenticateToken` check added by T052 in `internal/keystone/handlers.go`. Rejecting before resolution is what makes the refusal uniform and therefore non-disclosing (FR-014); the implementation MUST NOT query the domains table on this path.
- [X] T054 [US2] Refuse a request naming more than one scope kind, in the same check in `internal/keystone/handlers.go` (FR-012, contract S7).
- [X] T055 [US2] Verify `TokenClaims` (`internal/keystone/auth.go:22`) and `AuthResponse.Token` (`:117`) are **unchanged** — no domain member is added to either, because no domain-scoped token is ever issued.
- [X] T056 [US2] Confirm T046–T049 in `test/contract/keystone/scope_test.go` are GREEN, with T048 given explicit attention, and record RED→GREEN evidence in `docs/specs/023-tenant-scoping-authz/BASELINE.md`.

**Checkpoint**: every scope request is either honoured or refused; none is silently degraded.

---

## Phase 6: User Story 3 — A tenant cannot read another tenant's resource usage (Priority: P2)

**Goal**: Close the usage leak — the aggregate view becomes administrator-only, the per-project view is scoped to the caller.

**Independent Test**: Per `quickstart.md` US3 — a non-administrative tenant is refused the aggregate view and cannot read another project's usage by identifier.

**Depends on**: nothing. Fully parallel with Phases 3, 4, 5 and 8.

- [X] T057 [P] [US3] Write contract tests for U1–U4 in `test/contract/nova/tenant_usage_authz_test.go`: the aggregate refused for a non-administrator; a non-administrator's foreign identifier never yields another project's usage; a malformed or unknown target refused rather than zeroed.
- [X] T058 [US3] Gate `ListTenantUsage` (`internal/nova/tenant_usage.go:14`) on `common.HasRole(c, common.RoleAdmin)`. It groups by `i.project_id` with no predicate and no role check (FR-008b).
- [X] T059 [US3] Route `GetTenantUsage` (`internal/nova/tenant_usage.go:66`) through `common.QuotaTargetProject`, which already pins a non-administrator to their own project and refuses to *resolve* a foreign identifier so the endpoint cannot probe which projects exist. It currently passes `c.Param("id")` straight into `WHERE i.project_id = $1` (FR-008c).
- [X] T060 [US3] Stop swallowing errors into zeroes in both handlers in `internal/nova/tenant_usage.go` — a malformed identifier produces `invalid input syntax for type uuid` and currently surfaces as `200` with every figure at zero, indistinguishable from a real project owning no instances. A real project with no instances must still return `200` with zeroes (FR-008d).
- [X] T061 [US3] Confirm the existing `test/contract/nova/tenant_usage_test.go` still passes — it authenticates as `admin` and asserts `200`.
- [X] T062 [US3] Confirm T057 in `test/contract/nova/tenant_usage_authz_test.go` is GREEN and record RED→GREEN evidence in `docs/specs/023-tenant-scoping-authz/BASELINE.md`.

**Checkpoint**: a non-administrative caller can learn nothing about any project but their own from the usage endpoints (SC-004a).

---

## Phase 7: User Story 4 — Isolation results that also cover the override (Priority: P3)

**Goal**: The multi-tenancy suite covers both directions — the boundary holding for tenants, and the override working for operators.

**Independent Test**: The suite contains a check that authenticates as an operator and reaches another project's resource; removing a project-scoping predicate turns it red.

**Depends on**: Phases 3 and 4 — there is no override to cover until they land.

- [X] T063 [US4] Add positive override coverage to `test/multi_tenant_test.sh`: an operator reaching a resource owned by another project. All six existing checks are isolation-only, so a regression re-narrowing administrators would leave the suite green (FR-019).
- [X] T064 [US4] Add both-direction coverage for the operator-only surfaces to `test/multi_tenant_test.sh` — refused as a tenant, admitted as an operator. A gate added but never exercised as a success is indistinguishable from one that broke the endpoint (FR-019a, contract O6).
- [X] T065 [US4] Add the deliberately-red check to `test/multi_tenant_test.sh` and document the mutation procedure in `quickstart.md`: remove one project-scoping predicate from a point lookup, confirm the suite fails, restore it (FR-020).
- [X] T066 [US4] Confirm #545's guarantees still hold in `test/multi_tenant_test.sh` after the override lands — no identity used to assert isolation holds `admin`, an unresolvable role name fails the run loudly, teardown runs on any exit (FR-021).
- [X] T067 [US4] Run `./test/multi_tenant_test.sh` twice consecutively against the same deployment and confirm identical results (SC-009).
- [X] T068 [US4] Verify in `test/multi_tenant_test.sh` that cross-project deletion leaves no dangling state the owning tenant can observe — released addresses, detached volumes, and the console tokens #534 added.

**Checkpoint**: the suite's green result is evidence rather than assumption.

---

## Phase 8: User Story 5 — Filtering an identity collection actually filters (Priority: P3)

**Goal**: The six identity collections that discard every query filter honour them.

**Independent Test**: For each collection, a filtered request returns only matches; an unfiltered request is unchanged.

**Depends on**: nothing. Fully parallel with Phases 3–6. **The cleanest split-out if the branch needs trimming.**

- [X] T069 [P] [US5] Write contract tests for F1–F4 in `test/contract/keystone/list_filters_test.go`: each collection filtered by name returns only matches; a filter matching nothing returns an empty collection, never the full one; unfiltered requests unchanged; unknown filters ignored.
- [X] T070 [P] [US5] Add the `name` filter to `ListRoles` (`internal/keystone/handlers.go:624`), following the conditional-`WHERE` shape #545 established in `ListDomains` (`internal/keystone/domains.go:20`) — FR-024, FR-025, FR-026.
- [X] T071 [P] [US5] Add `name`, `domain_id` and `enabled` filters to `ListUsers` (`internal/keystone/handlers.go:324`).
- [X] T072 [P] [US5] Add `name`, `domain_id`, `enabled` and `parent_id` filters to `ListProjects` (`internal/keystone/handlers.go:385`).
- [X] T073 [P] [US5] Add `name` and `domain_id` filters to `ListGroups` (`internal/keystone/groups.go:17`).
- [X] T074 [P] [US5] Add `type` and `name` filters to `ListServices` (`internal/keystone/services.go:18`).
- [X] T075 [P] [US5] Add `interface`, `service_id` and `region_id` filters to `ListEndpoints` (`internal/keystone/services.go:269`).
- [X] T076 [US5] Verify `ListSubnets` in `internal/neutron/network.go` is **untouched** (FR-027) — same bug class, owned by #546's B6; fixing it here would conflict across branches (FR-027, contract F5).
- [X] T077 [US5] Remove the client-side `jq 'select(.name=="member")'` workaround from `test/multi_tenant_test.sh` and confirm the suite's result is unchanged, demonstrating the server-side filter is real (SC-011, contract F6).

**Checkpoint**: no identity collection answers a filter with the unfiltered collection.

---

## Phase 9: Polish & Cross-Cutting Concerns

- [X] T078 Add a static consistency check to `BASELINE.md` proving no in-scope compute handler still reads `c.GetString("project_id")` directly for a point lookup — the mitigation for the 44-handler consistency risk (SC-002).
- [X] T079 [P] Update `docs/blueprint.md`: `:592` states "Lightstack has no system scope — it uses project / admin role checks", `:594` and `:588` describe the flat project-scoped model, `:149` lists domain-scoped assignments as missing. Reconcile all four with the behaviour as shipped (FR-023).
- [X] T080 [P] Update `docs/KEYSTONE_AUTH_FLOW.md` to state that project and unscoped are the only two scope kinds and that a domain scope is refused.
- [X] T081 [P] Update the *JWT Authentication* and *Project Isolation* notes in `CLAUDE.md`, which state resources are scoped by `project_id` from the token without qualification.
- [X] T082 [P] Review `docs/compatibility/OPENSTACK_COMPATIBILITY_REPORT.md` for statements this feature invalidates.
- [X] T083 [P] Update `docs/testing/BLUEPRINT_TEST_COVERAGE.md:51`, which records `rbac_test.go.broken` as disabled and not counted as coverage.
- [X] T084 Run `make fmt` and `make lint` (targets in `Makefile`) and confirm zero issues across `internal/` and `test/`.
- [X] T085 Run the full suite — `make test`, `go test ./test/contract/...`, `./test/quick_test.sh`, `./test/integration_test.sh`, `./test/multi_tenant_test.sh` — and compare against `BASELINE.md`. `make test-all` remains RED on B10 (#546); confirm no *new* failure.
- [X] T086 Exercise Horizon's Admin panel against the surfaces this feature gates — Admin › Compute › Host Aggregates, Admin › Instances (which sends `all_tenants`) and the migration list — per the Constitution's "Horizon dashboard tested (if UI-affecting changes)" merge gate. `test/horizon_compat_test.sh` does **not** cover these endpoints, so running it alone proves nothing; verify against a live Horizon per `docs/specs/018-horizon-dashboard-verification/`.
- [X] T087 Verify the three services this feature does not edit are genuinely unchanged (SC-004): run `./test/multi_tenant_test.sh`, `./test/security_groups_test.sh` and the Cinder/Glance/Neutron contract suites under `test/contract/`, and confirm no cross-tenant behaviour differs from `BASELINE.md`.
- [X] T088 Execute `quickstart.md` end to end on the stub stack and confirm every before/after row.
- [X] T089 Confirm no schema change and no migration were introduced: `git diff --stat migrations/` must be empty (data-model.md).

---

## Dependencies

```
Phase 1 (Setup)
   └─> Phase 2 (Foundational: T004–T007)  ⚠ BLOCKING for US1 + US6 only
          ├─> Phase 3  US1 (P1)  🎯 MVP
          │      └─> 3b (T014–T016) is independently shippable
          └─> Phase 4  US6 (P1)   [T041, T042 need nothing; T043 needs T007]

Phase 5  US2 (P2)  ─┐
Phase 6  US3 (P2)  ─┼─ independent of everything above and of each other
Phase 8  US5 (P3)  ─┘

Phase 7  US4 (P3)  <── requires Phases 3 and 4 (nothing to cover before then)

Phase 9  Polish    <── requires all
```

**Story independence**: US2, US3 and US5 depend on nothing and can be built in any order, by different people, at the same time as US1. US6's two gates are likewise unblocked; only its per-server migration work needs the Phase 2 helper. US4 is the sole story with a hard dependency on another.

## Parallel execution

**Maximum parallelism after T007:**

- One track: T017 → T020 (the `ServerAction` dispatcher and the advanced actions — same files, must serialise)
- Parallel with it: T021, T022, T023, T024, T025 (five distinct files)
- Parallel with both: all of Phase 5 (US2), Phase 6 (US3), Phase 8 (US5), and T041/T042 of Phase 4

**Within Phase 8**: T070–T075 touch four different files and are fully parallel.

**Within Phase 9**: T079–T083 are five distinct documents.

## Implementation strategy

**Ship in three increments.**

1. **First merge — T014–T016 (FR-008a).** Two operations that gate on `admin` and then refuse the administrator. No policy decision, ~10 lines, and it turns the revived RBAC contract test green. Valuable on its own.
2. **MVP — Phase 3 complete (US1).** The ticket's headline: an operator can administer any tenant's compute resources. Demonstrable end to end via `quickstart.md`.
3. **Everything else.** US6 should follow close behind — it contains the only privilege escalation. US2, US3 and US5 can land in any order.

**If the branch needs trimming**: split out **US5** (Phase 8). It has no confidentiality or capability consequence, #545 already removed the sharp edge from the one place it had bitten, and it is the only story touching a service outside compute. **Do not trim US6** — it is the highest-severity work here.

## Task summary

| Phase | Story | Priority | Tasks | Count |
|---|---|---|---|---|
| 1 | Setup | — | T001–T003 | 3 |
| 2 | Foundational | — | T004–T007 | 4 |
| 3 | US1 — administrative override | P1 🎯 | T008–T038 | 31 |
| 4 | US6 — operator-only surfaces | P1 | T039–T045 | 7 |
| 5 | US2 — domain scope | P2 | T046–T056 | 11 |
| 6 | US3 — usage confidentiality | P2 | T057–T062 | 6 |
| 7 | US4 — validation coverage | P3 | T063–T068 | 6 |
| 8 | US5 — identity filtering | P3 | T069–T077 | 9 |
| 9 | Polish | — | T078–T089 | 12 |
| | | | **Total** | **89** |

28 tasks are marked `[P]`. Every task carries a checkbox, a sequential ID, a story label where the phase requires one, and an exact file path.

`[P]` is used strictly: no two parallel tasks write the same file. T012, T040 and T047–T049 append to a file an earlier `[P]` task creates, so they are sequential within that file even though they are independent of every other phase.
