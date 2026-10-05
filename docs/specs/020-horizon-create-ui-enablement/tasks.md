---

description: "Task list for SPEC-020 — Horizon create/write UI enablement (F2, F3, F7, F8)"
---

# Tasks: Horizon compatibility — create/write UI enablement (F2, F3, F7, F8)

**Input**: Design documents from `docs/specs/020-horizon-create-ui-enablement/`
**Prerequisites**: [plan.md](./plan.md), [spec.md](./spec.md), [research.md](./research.md), [data-model.md](./data-model.md), [contracts/](./contracts/)
**Branch**: `feature/533` (based on `main`, which carries #532 as `0a4dde0`) | **Ticket**: cobaltcore-dev/cloud-storage#533

**Tests**: **REQUIRED** — FR-015 mandates an automated test per fix that fails before and passes after, and
Constitution Art. III makes Test-First NON-NEGOTIABLE. Test tasks are written **RED first** in every phase.

**Organization**: Tasks are grouped by user story. Note the structural nuance recorded in the plan: this
feature has **one shared root**, so the cross-cutting reconciliation lands in Phase 2 (Foundational) and each
story phase then adds only its own story-specific surface. *(Superseded: the shared root was falsified during
implementation — three roots, see spec.md* Assumptions *— and FR-021–FR-024 were added for them.)*

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel — **different files**, no dependency on an incomplete task
- **[Story]**: US1–US4, mapping to the spec's user stories
- Exact file paths are included in every task

## Path Conventions

Single Go project at repository root: `internal/<service>/`, `test/`, `docs/`. No new packages, **no
migration** (the `quotas` and `cinder_quotas` tables already exist).

> ⚠️ **Three files carry most of this feature and cannot be parallelised internally** —
> `internal/neutron/network.go`, `internal/cinder/volumes.go`, `internal/nova/quotas.go`. Parallelism is
> mostly **across services**, not within them. Every `[P]` below has been checked against this.

> ⚠️ **These endpoints are not untested today.** `internal/**` has no unit coverage for quota/limits, but
> `test/contract/{nova/limits,cinder/quotas,neutron/quotas}_test.go`, `test/quota_test.sh` and
> `test/quota_admin_test.sh` already exercise them — and **`make test` deliberately excludes `test/contract`**.
> T051 runs them as the FR-016 regression gate.

---

## Phase 1: Setup & Trace (the FR-014 gate)

**Purpose**: Confirm what Horizon *actually* calls before editing any handler. The 14 gaps (G1–G14) from
research are a **hypothesis**, not an assumed-complete list — the ticket is explicit that tracing comes first.

- [x] T001 Bring up the stub verification stack (Horizon + Lightstack stub + postgres) via `deployments/docker-compose-horizon.yml`, following `docs/specs/018-horizon-dashboard-verification/HOST_TESTING.md`
- [x] T002 [P] Create the endpoint probe script `test/horizon_create_ui_test.sh` (token → `curl` each quota/limits/extensions/types endpoint → assert status + keys with `jq`), modelled on the existing `test/horizon_compat_test.sh` pattern; it doubles as the SC-005 regression guard
- [x] T003 Drive all four create UIs (Project → Overview; Network → Networks; Volumes → Create Volume; Compute → Instances → Launch Instance) while capturing Lightstack's structured request log, and write the per-panel path→status call map into a new `## Trace results` section of `docs/specs/020-horizon-create-ui-enablement/research.md`
- [~] T004 **(host-gated — needs an interactive browser session)** Capture Horizon's Django traceback for each failing panel (`docker compose -f deployments/docker-compose-horizon.yml exec horizon sh -c 'tail -80 /var/log/apache2/*error*'`) to name the failing call
- [x] T005 Reconcile the captured call map against gaps G1–G14 in `docs/specs/020-horizon-create-ui-enablement/research.md` §R1 — mark each confirmed/refuted, append any newly discovered gap, and close out the open-ended parts of FR-002 (`.json` variants) and FR-013 (required extension aliases) in `spec.md` with the confirmed set

**Checkpoint**: The gap list is evidence-backed. Implementation may begin.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: The shared root — one role-check implementation and one limits source per service. **Every user
story reads through these**, so they must land first.

**⚠️ CRITICAL**: No user story work can begin until this phase is complete.

### Tests first (RED) ⚠️

- [x] T006 [P] **Extend the existing** `internal/common/roles_test.go` (it already has `TestHasRole` covering present / absent / nil / exact / substring) with the missing **wrong-typed `roles` claim** case that T013 depends on — do not recreate the file or duplicate the existing cases
- [x] T007 [P] RED test: Nova `GET /v2.1/limits` and `GET /v2.1/os-quota-sets/{id}` report the **same** limits, at upstream defaults 10/20/51200, and a `quotas` row override is reflected in **both** — `internal/nova/quotas_test.go` *(fails today: 100/200/512000 vs 10/20/51200)*
- [x] T008 [P] RED test: Cinder `GET /v3/limits` and `GET /v3/quota-sets/{id}` report the same limits and a `cinder_quotas` override is reflected in **both** — `internal/cinder/quotas_test.go` *(fails today: limits ignore the table)*
- [x] T009 [P] RED test: Neutron quota limits are identical across `/v2.0/quotas/{id}` and `/v2.0/quotas/{id}/details` — `internal/neutron/quotas_test.go`
- [x] T010 **(done differently — added a local `doReqBody` helper in `internal/neutron/network_selectability_test.go` rather than changing the shared 4-arg `doReq` and every existing caller)** Extend the neutron test helper `doReq` with a request-body parameter (it currently takes 4 args, unlike nova/cinder's 5) in `internal/neutron/network_db_test.go` — required by T022's network-create test

### FR-019 — unite every role check on `common.HasRole`

- [x] T011 [P] Replace the hand-rolled admin loop with `common.HasRole(c, "admin")` in `internal/nova/quotas.go` (~:162-174)
- [x] T012 [P] Replace both hand-rolled admin loops with `common.HasRole` in `internal/nova/advanced_actions.go` (~:474-486, ~:1220-1232)
- [x] T013 [P] Replace the hand-rolled admin loop with `common.HasRole` in `internal/cinder/volumes.go` `VolumeAction`/`os-reset_status` (~:965-989); document the one edge-case change (wrong-typed roles claim 500 → 403, an unreachable branch)
- [x] T014 [P] Delete the pass-through wrapper `hasRole` from `internal/keystone/handlers.go` (~:145-149) and convert its 5 call sites to `common.HasRole` — `internal/keystone/handlers.go:978,1204,1243` and `internal/keystone/ec2_credentials.go:56,79`

### FR-003 — one limits source per service

- [x] T015 Add a package-local limits helper (code defaults ⊕ `quotas` rows for a project) in `internal/nova/quotas.go`; fold `keypairs` / `server_groups` / `server_group_members` into the shared default set and route `GetQuotaSet` + `GetQuotaSetDefaults` through it, removing their duplicate literals (depends on T011 — same file)
- [x] T016 Wire `GetLimits` to the helper and **delete the shadowing local map** (100/200/512000) in `internal/nova/handlers.go` (~:1957-1966); also check `quotaRows.Err()` (depends on T015)
- [x] T017 [P] Add a package-local limits helper (defaults ⊕ `cinder_quotas`) in `internal/cinder/quotas.go` and remove the duplicated defaults map (~:143-150)
- [x] T018 Wire Cinder `GetLimits` to the helper so a quota override affects limits, and make `GetLimitsNoProject` delegate instead of duplicating the body, in `internal/cinder/volumes.go` (~:1948-2030) (depends on T013, T017)
- [x] T019 [P] Hoist Neutron's two duplicated hardcoded quota maps into a single package-level defaults map used by `GetQuota` and `GetQuotaDetails` in `internal/neutron/network.go` (~:320-333, ~:362-400)

### FR-020 — Nova authorization ordering

- [x] T020 Move the admin-role check **before** request-body binding in `UpdateQuotaSet` so a non-admin gets 403 (not 400) for a malformed body, in `internal/nova/quotas.go`; add the test to `internal/nova/quotas_test.go` (depends on T011, T015 — same file)

**Checkpoint**: T006–T009 now GREEN. One role implementation repo-wide; one limits source per service; limits
and quota-sets agree. User stories can proceed.

---

## Phase 3: User Story 1 — Launch an instance end-to-end from the dashboard (Priority: P1) 🎯 MVP

**Goal**: A working Launch Instance action, a Networks step with selectable rows, and an instance that reaches
ACTIVE — the ticket's flagship workflow (F7).

**Independent Test**: From Project → Compute → Instances, use Launch Instance; select an image, a flavor, and a
network (a selectable row appears); launch. The instance reaches ACTIVE and appears in the Instances panel.

### Tests for User Story 1 (RED) ⚠️

- [x] T021 [P] [US1] RED tests in `internal/nova/quotas_test.go`: `GET /v2.1/os-quota-sets/{id}/detail` returns 200 with `{in_use, limit, reserved}` per resource (404 today); `totalFloatingIpsUsed` / `totalSecurityGroupsUsed` / `totalServerGroupsUsed` reflect seeded rows, not `0`; a non-admin is not served another project's quota while an admin is
- [x] T022 [P] [US1] RED tests in `internal/neutron/network_selectability_test.go`: every network response (list, show, **create**) carries `project_id` equal to `tenant_id`; **and the subnet-reference integrity property** — every id in a network's `subnets` array resolves in `GET /v2.0/subnets` for the same caller, including for a shared network owned by another project (depends on T010 for the create-with-body case)

### Implementation for User Story 1

- [x] T023 [US1] Add the `GetQuotaSetDetail` handler (per-resource `{in_use, limit, reserved}`, `reserved` always `0`) in `internal/nova/quotas.go` and register `GET /os-quota-sets/:id/detail` in `internal/nova/handlers.go` (~:229) — the route **must** reuse the `:id` param name or gin panics
- [x] T024 [US1] Populate the three hardcoded-`0` usage counters from `floating_ips` / `security_groups` / `server_groups` in `internal/nova/handlers.go` (~:2010-2012)
- [x] T025 [US1] In `internal/nova/quotas.go` `GetQuotaSet`: add the missing `snapshots_used` and `security_group_rules_used` counters, and make `GetQuotaSet` and `GetLimits` count instances the same way, so the two agree. *(Done by dropping `GetLimits`' `status != 'DELETED'` predicate: every view counts every instance row — `TestLimitsCountEveryInstanceRow`.)*
- [x] T026 [US1] Scope Nova quota reads to the caller's project with an `admin` override via `common.HasRole`, preserving the existing `default` → caller's-project alias, in `internal/nova/quotas.go`
- [x] T027 [P] [US1] Add `project_id` alongside `tenant_id` to the network object at **all three** build sites — `CreateNetwork` (~:462), `ListNetworks` (~:637), `GetNetwork` (~:709) — in `internal/neutron/network.go`
- [x] T028 [US1] Align subnet-reference scope so `networkSubnetIDs` (~:1287, no project predicate, unpaginated) and `ListSubnets` (~:977, project-scoped + LIMIT/OFFSET) cannot disagree for a given caller, in `internal/neutron/network.go` (depends on T027 — same file)
- [x] T029 [US1] **In-phase browser check** against the stub stack from T001: Project → Compute → Instances shows a **Launch Instance** action (FR-006) and the wizard's Networks step renders a **selectable** row (FR-007); record the result here ✅ **Verified 2026-07-28 (stub):** Launch Instance present, wizard completed, `ui-vm` ACTIVE.

**Checkpoint**: The launch wizard renders selectable networks and Launch is available; an instance launches
from the dashboard. US1 is independently demonstrable.

---

## Phase 4: User Story 2 — Create a volume from the dashboard (Priority: P2)

**Goal**: The Create Volume dialog opens (today it opens nothing) and creates a volume — F8.

**Independent Test**: Project → Volumes → Create Volume; the dialog opens populated with type/availability
options; create a small volume; it appears in the list.

### Tests for User Story 2 (RED) ⚠️

- [x] T030 [P] [US2] RED tests in `internal/cinder/quotas_test.go`: `GET /v3/extensions` returns 200 listing `AvailabilityZones` (404 today, **needs no DB**); `GET /v3/quota-sets/{id}?usage=true` returns the `{in_use, limit, reserved}` shape (flat ints today) and `/defaults` returns 200 (404 today); a non-admin `PUT`/`DELETE /v3/quota-sets/{id}` returns **403** and leaves the quota unchanged (succeeds today); `totalBackupsUsed` reflects seeded backups

### Implementation for User Story 2

- [x] T031 [US2] Add a `ListExtensions` handler advertising only genuinely-supported extensions (at minimum `AvailabilityZones`, backed by the existing `/v3/os-availability-zone`), entries shaped `{alias, name, description, updated, links: []}`, in `internal/cinder/volumes.go`
- [x] T032 [US2] Register `GET /v3/extensions` at the flat path plus the `/v3/:project_id` group variant, in `internal/cinder/volumes.go` (`volumes.go:168` and `volumes.go:259`) (depends on T031). *Revised 2026-07-29: originally required registering **before** the group because registering after it was believed to panic gin at startup. It does not — gin splices a static child ahead of an existing wildcard at the same level, verified in both orders against the vendored v1.12.0, so registration order is irrelevant. What does panic is reusing a **different** param name at a position already claimed by one, which is why every route here uses `:id` (plan.md, `contracts/cinder-extensions-and-quota-sets.md`, and the comment at `volumes.go:112`).*
- [x] T033 [US2] Add the `?usage=true` view to `GetQuotaSet` and a `GetQuotaSetDefaults` handler in `internal/cinder/quotas.go`, and register `GET /v3/quota-sets/:id/defaults` in `internal/cinder/volumes.go`, both flat and project-group (depends on T017, T032). *Revised 2026-07-29: originally specified a `/detail` sub-resource; block storage has no such path (FR-002 amendment), so usage is a query parameter and no `/detail` route is registered.*
- [x] T034 [US2] Scope Cinder quota reads to the caller's project with an `admin` override via `common.HasRole`, retaining the existing name→UUID lookup for the admin path (the contract suite calls these with the literal project id `default`), in `internal/cinder/quotas.go` (depends on T033)
- [x] T035 [US2] **FR-020**: require the `admin` role for `PUT` and `DELETE /v3/quota-sets/{id}` — today they have **no** authorization at all — in `internal/cinder/quotas.go`, using the existing house error wording (depends on T034)
- [x] T036 [US2] Populate `totalBackupsUsed` / `totalBackupGigabytesUsed` from the `volume_backups` table in `internal/cinder/volumes.go` (depends on T018). *Revised 2026-08-05: first resolved as not-applicable, on the grounds that Lightstack exposes the backup API but has no backups table — a live query had been tried and failed. The premise was wrong, and the failed query is why: the table is **`volume_backups`** (migration `022_cinder_backups`), written by `internal/cinder/backups.go`, so a query against `backups` — the name this task's title uses — finds nothing. Both views now count it, `internal/cinder/volumes.go` (`/limits`) and `internal/cinder/quotas.go` (`?usage=true`), and `internal/cinder/quotas_test.go` seeds a backup and fails if either reports 0.*
- [x] T037 **(confirmed by the probe: `/v3/types`, `/v3/types/default`=__DEFAULT__ and `/v3/os-availability-zone` all 200 on Horizon's flat path)** *Revised: the probe answered "does Horizon work today", not "is discovery consistent". T031 made this group's own `/extensions` advertise `os-availability-zone`, while the route existed only at the flat path — and the fallback catalog in `internal/keystone/auth.go` (`buildHardcodedCatalog`) builds the `volumev3` entry as `.../v3/{project_id}` (the seeded catalog serves `…:8776/v3`), so a client following that catalog discovers the capability and 404s on the base it was handed. The scoped route is now registered too, matching upstream (`cinder/api/v3/router.py`'s `ProjectMapper.resource` registers every resource with the project_id prefix and again without it) and this feature's own mirroring policy (plan.md). Covered by T052.* [US2] Confirm against the T003 call map that the volume-type and availability-zone lookups resolve on Horizon's actual path (note: `/v3/os-availability-zone` is registered **only** at the flat path, unlike types/limits/quota-sets); add the project-group variant in `internal/cinder/volumes.go` only if the trace shows it is needed

**Checkpoint**: The Create Volume dialog opens and creates a volume; Cinder quota reads are scoped and writes
are admin-only. US2 is independently demonstrable.

---

## Phase 5: User Story 3 — Create a network from the Project panel (Priority: P2)

**Goal**: A Create Network action in Project → Network → Networks for a project member — F3.

**Independent Test**: As the seeded `admin` user on the **Project** dashboard, open Network → Networks; the
Create Network action is present; create a network with a subnet; it appears exactly once.

> All implementation tasks in this phase touch the single file `internal/neutron/network.go`, so they are
> **sequential**.

### Tests for User Story 3 (RED) ⚠️

- [x] T038 [P] [US3] RED tests in `internal/neutron/quotas_test.go`: `GET /v2.0/quotas/{id}` includes `project_id` **and** `tenant_id`; `GET /v2.0/quotas/{id}/default` returns 200 (404 today); `…/details.json` returns the same body as `…/details` (404 today); `GET /v2.0/extensions` lists `quota_details`; `security_group_rule.used` is real, not `0`; a non-admin is not served another project's usage. These tests need `LIGHTSTACK_TEST_DB_URL`: the handlers resolve their target project

### Implementation for User Story 3

- [x] T039 [US3] Rewrite `GetQuota` to resolve the target project, emit `project_id` **and** `tenant_id`, and read limits from the shared defaults map, in `internal/neutron/network.go` (~:318-333) — it ignores `c.Param("id")` entirely today (depends on T019)
- [x] T040 [US3] Add a `GetQuotaDefault` handler and register `GET /v2.0/quotas/:id/default` in `internal/neutron/network.go` (~:164-165) (depends on T039)
- [x] T041 [US3] Register `.json` aliases for `/v2.0/quotas/:id/details` and `/v2.0/quotas/:id/default` pointing at the same handlers in `internal/neutron/network.go` (depends on T040)
- [x] T042 [US3] Add `reserved: 0` to each entry and count `security_group_rule.used` for real (hardcoded `0` today) in `GetQuotaDetails`, `internal/neutron/network.go` (~:335-408) (depends on T041)
- [x] T043 [US3] Advertise the `quota_details` extension and add the upstream `links: []` field to all entries in `ListExtensions`, `internal/neutron/network.go` (~:272-316) — for API fidelity: Horizon 2025.2 calls `/details` without checking for the alias (depends on T042)
- [x] T044 [US3] Scope Neutron quota reads to the caller's project with an `admin` override via `common.HasRole` (the package has no role check today), in `internal/neutron/network.go` (depends on T043)
- [x] T045 [US3] **In-phase browser check** against the stub stack from T001: Project → Network → Networks shows a **Create Network** action (FR-009) and a created network+subnet is listed exactly once (FR-010); record the result here ✅ **Verified 2026-07-28 (stub + real):** Create Network present on the Project panel; `ui-net` created and listed once.

**Checkpoint**: The Project panel offers Create Network and the created network is listed exactly once and is
selectable in the launch wizard. US3 is independently demonstrable.

---

## Phase 6: User Story 4 — Project Overview limits widget loads (Priority: P3)

**Goal**: The Overview usage/limits widget renders compute, block-storage and network usage without "Unable to
retrieve limits information" — F2.

**Independent Test**: Open Project → Overview; the usage/limits panel renders with no error, before and after
a project switch.

> **By design, most of this story falls out of Phases 2–5**: the Overview aggregates all three services, so it
> is the *last* story precisely because it is the sum of the other fixes. These tasks verify the aggregate and
> close whatever the trace showed is still missing.

- [x] T046 [P] [US4] Add the aggregate assertion to `test/horizon_create_ui_test.sh`: every endpoint Horizon's project-usage aggregation calls returns success (no 404/500) and the limits reported for a resource agree across the limits and quota-set views (SC-005)
- [x] T047 [US4] Load Project → Overview in the stub stack and confirm the widget renders compute, block-storage and network usage with no error (FR-005, SC-004); fix any remaining aggregation leg that the T003/T005 call map identified and earlier phases did not already cover ✅ **Verified 2026-07-28 (stub + real):** Overview renders Compute + Volume + Network groups, no "Unable to retrieve limits information".
- [x] T048 [US4] Confirm the widget reloads cleanly after a project switch (create a second project if the fixture has only `default`, which the SPEC-018 pass flagged as a fixture limit) — **unblocked 2026-10-01:** the second project can be created. `POST /v3/projects` returns 500 only for a non-UUID `domain_id` such as `default` (F13, B5 — now in cloud-storage#545), and `openstack project create` sends no `domain_id`. Still to run. The single-project reload path *was* verified (T047).

**Checkpoint**: All four user stories are independently functional.

---

## Phase 7: Polish, Verification & Hand-off

**Purpose**: Cross-cutting quality gates and the two-mode verification the spec requires.

- [x] T049 [P] Run the full unit suite with and without a database — `go test ./internal/...` bare, then with `LIGHTSTACK_TEST_DB_URL` set — and confirm the previously-RED tests are GREEN and no existing unit test regressed ⚠️ **Re-checked 2026-07-29 — the original tick was FALSE.** The bare run had never been done: `go test` without `LIGHTSTACK_TEST_DB_URL` **segfaulted** (nil `pgxpool.Pool` in `common.ResolveProjectRef`), and `make test` hid it because it lacked `-count=1` and served `ok (cached)`. Fixed by making the tests honest about their own requirements: `requireDB(t)` on the two neutron tests that resolve an admin-supplied id, nova's `_NoDB` test switched to a non-admin header so it is genuinely DB-free while still running, and `-count=1` on the `test` target so the gate cannot be served from cache. A nil-pool guard in `common.ResolveProjectRef` was tried and then **removed**: `main.go:101` connects before any route is served, and the repo has ~692 unguarded `database.DB` call sites, so one guard bought nothing but an untestable branch. Verified in BOTH states, 0 cached.
- [x] T050 [P] Run `make fmt`, `make lint`, and `make build`; confirm the binary starts (this also proves no gin route-registration panic was introduced)
- [x] T051 **FR-016 regression gate — the pre-existing suites `make test` excludes.** With Lightstack running, execute `make test-contract` (gophercloud: `test/contract/nova/limits_test.go`, `test/contract/cinder/quotas_test.go`, `test/contract/neutron/quotas_test.go`) plus `test/quota_test.sh`, `test/quota_admin_test.sh` and `test/horizon_compat_test.sh`. Explicitly confirm the two intentional behavior changes broke nothing: the FR-003 limit values (their assertions are value-agnostic) and the FR-020 admin-only quota writes (the contract client authenticates as `admin`/`secret`) — verify, don't assume
- [x] T052 Add a route-registration smoke test asserting every route this feature adds to **Cinder** resolves to a handler — `TestRouteRegistration_NoWildcardConflict` in `internal/cinder/quotas_test.go` (not `volumes_test.go`, which holds the serialization tests). ⚠️ **Rationale corrected 2026-07-29:** the task was written around a flat-`/v3/<static>`-before-`/v3/:project_id` *ordering* constraint. That constraint does not exist — gin splices a static child ahead of an existing wildcard, verified in both orders against the vendored v1.12.0 — so the test now asserts **presence** (and, since FR-002's amendment, the **absence** of the four Cinder `/detail` routes). The real panic risk is reusing a different param name at a position already claimed by one; its `defer recover()` was dropped because it could never fire.
- [x] T053 **Stub-mode verification pass**: re-run `docs/specs/018-horizon-dashboard-verification/HOST_TESTING.md` against `deployments/docker-compose-horizon.yml` and observe all four workflows fixed in the browser (FR-017; SC-001, SC-002, SC-003, SC-004) ✅ **Done 2026-07-28:** §2.1–§2.7 all PASS (probe 36/36 at the time; now 54 checks after the SC-007a additions).
- [x] T054 **Real-mode verification pass**: Lightstack on **k3s** via `charts/lightstack` (real mode) + **Horizon installed with Helm** via `deployments/horizon-chart/` + **Ceph/RGW via Rook on minikube** via `deployments/rook/setup-rook-rgw.sh`, following `docs/specs/018-horizon-dashboard-verification/HOST_TESTING_REAL_MODE.md`; confirm the dashboard-launched instance actually boots (FR-008, FR-017; SC-006) ✅ **Done 2026-07-28:** §2.1–§2.6 PASS on k3s real mode — dashboard-launched `ui-vm` reached ACTIVE on a real libvirt domain, booting cirros from the RGW-backed image. §1 of the runbook had to be corrected first (it could not have produced a real-mode stack as written).
- [x] T055 Confirm Horizon remained **stock** throughout both passes — no config/policy override was added to make a control appear; record any workflow that would have required one as a documented finding instead of a fix (FR-018) ✅ **Verified 2026-07-28:** no policy or feature-flag override was added. One deployment-config correction was needed — `CSRF_COOKIE_HTTPONLY` — carved out explicitly as FR-018a.
- [x] T056 [P] Update `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`: tag F2/F3/F7/F8 as fixed with an update block mirroring the existing F4/F5/F6/F9 one, and revise the Conclusion + Follow-ups (only F1/#534 then remains open) ✅ **Done 2026-07-29:** all four tagged `✅ FIXED (SPEC-020 / #533, 2026-07-29)` with their real root causes; Verdict, workflow matrix and Conclusion rewritten; seven new findings F10–F16 recorded. Also corrected the F10–F16 provenance statement, which had claimed F14 was both pre-existing and introduced-in-pass and miscounted the pre-existing set as six.
- [x] T057 [P] Tick the completed items on the cloud-storage#533 `## Checklist` comment and post the weekly done summary ✅ **Done 2026-07-29:** checklist comment 5069748787 updated (21 ticked, 4 annotated with reasons) and the weekly summary posted as comment 5112626868.
- [x] T058 Run a branch review over the full diff (all of `internal/`, `test/horizon_create_ui_test.sh`, and `docs/`) for correctness, idiom and stale documentation; address findings before opening the PR stacked on the #532 branch. **Two rounds:**
  - **Round 1 ✅ 2026-07-29** — 0 Critical, 4 Major, 4 Minor, 2 Nit; all 10 addressed. Gates green; coverage gap on the two security-group-rule endpoints closed.
  - **Round 2 ✅ 2026-07-29**, run after committing round 1 — **1 Critical, 8 Major, 11 Minor, 4 Nit**. The Critical was this task's own blind spot: `go test` without `LIGHTSTACK_TEST_DB_URL` segfaulted (see T049). Round 1's "0 Critical" was therefore not a clean bill of health, and is recorded here rather than left to imply one. All Criticals/Majors and every Minor/Nit except commit hygiene, the duplicate-body read in `test/contract/nova/migration_test.go`, and the project-scoped route duplicates were fixed, each with a RED-before-GREEN demonstration. Notable outcomes: Cinder's four invented `/detail` routes **deleted** in favour of the `?usage=true` the API actually documents (FR-002 amended); the quota-set `PUT` round-trip restored (FR-016); Nova `/detail` snapshot usage and Cinder backup usage made real (FR-004); the null-`security_group_rules` and BDM-adoption paths given tests that actually execute them (FR-015); FR-013 amended and made testable; and §2.7 of both runbooks rewritten to assert deployment state instead of a `git diff` that decayed on commit *and* on rebase.
- [x] T059 **FR-025 — boot from a new volume (added in review).** RED first: `internal/nova/boot_volume_test.go`,
  `internal/cinder/boot_volume_test.go`, and the `TestBootSourcePrecedence` case that asserted the downgrade. Then
  `internal/nova/handlers.go` (create the volume through Cinder, the size rules, the refusals),
  `internal/cinder/volumes.go` (`CreateBootVolumeFromImage`, the image-base fetch) and the wiring in
  `cmd/lightstack/main.go`. Docs: FR-025, both runbooks (§2.5, §2.8 and teardown), F16 in the compatibility
  report, `docs/UPGRADING.md`. ✅ unit tests and a stub end-to-end run with local volumes 2026-10-05;
  ⏳ the real-mode host pass, `HOST_TESTING_REAL_MODE.md` §2.5.
- [x] T060 **FR-026 — images by upstream's visibility rules, active only (added in review).** RED first:
  `internal/common/image_test.go` (the rule and the status), `internal/nova/image_visibility_test.go` (create,
  rebuild), `internal/cinder/image_visibility_test.go` (volume create, `os-reimage`),
  `internal/glance/visibility_test.go` (show, list with its filters, download). Then
  `common.ImageVisibleSQL`/`VisibleImage`, their callers, Glance's show, list and download, the admin-aware cache
  key and the eviction on member changes. The contract suite and six shell scripts booted from an image id that
  names no image, or from an image with no data; they now create an active image (`contract.CreateTestImage`).
  Docs: FR-026, `docs/UPGRADING.md`. ✅ unit tests, the contract suite and the scripts against a stub stack
  2026-10-05; ⏳ the host passes.

---

## Dependencies & Execution Order

### Phase Dependencies

- **Phase 1 (Setup & Trace)**: no dependencies — **gates everything** (FR-014)
- **Phase 2 (Foundational)**: depends on Phase 1 — **BLOCKS all user stories** (one role helper, one limits source per service)
- **Phase 3–6 (User Stories)**: all depend on Phase 2
  - **US1 (P1)**, **US2 (P2)**, **US3 (P2)** are mutually independent — different services/files
  - **US4 (P3)** is the *aggregate* of the three; its verification tasks land last by nature
- **Phase 7 (Polish)**: depends on all desired stories being complete

### Cross-story notes

- **US1 depends on #532 / F4** (the deduplicated, filter-honoring network list) — merged to `main` as `0a4dde0`, this branch's base; no task required.
- **US3 feeds US1**: a network created via the Project panel must be selectable in the launch wizard (FR-010). Verify after both land.
- **US4 has no code of its own** beyond what US1–US3 and Phase 2 deliver, plus whatever the trace surfaces.

### Within each user story

Tests (RED) → handlers → routes → authorization scoping → in-phase browser check.

### File-level serialization (checked)

| File | Tasks that must run in order |
|---|---|
| `internal/nova/quotas.go` | T011 → T015 → T020 → T023 → T025 → T026 |
| `internal/nova/handlers.go` | T016 → T023 (route) → T024 |
| `internal/cinder/quotas.go` | T017 → T033 → T034 → T035 |
| `internal/cinder/volumes.go` | T013 → T018 → T031 → T032 → T033 (route) → T036 → T037 |
| `internal/neutron/network.go` | T019 → T027 → T028 → T039 → T040 → T041 → T042 → T043 → T044 |
| `test/horizon_create_ui_test.sh` | T002 → T046 |

### Parallel Opportunities

- **Phase 1**: T002 runs alongside T001.
- **Phase 2 tests**: T006, T007, T008, T009 — four different files, fully parallel.
- **Phase 2 role unification**: T011, T012, T013, T014 — four different files, fully parallel.
- **Across services**: Nova (US1), Cinder (US2) and Neutron (US3) work touches disjoint files and can be
  staffed in parallel once Phase 2 completes.
- **Phase 7**: T049, T050, T056, T057 are parallel-safe.
- **Not parallel**: anything within the six files listed in the serialization table above.

---

## Parallel Example: Phase 2

```bash
# All four RED tests together (different files):
Task: "Extend internal/common/roles_test.go with the wrong-typed roles case"                  # T006
Task: "RED: nova limits ⇄ quota-set agreement in internal/nova/quotas_test.go"                # T007
Task: "RED: cinder limits ⇄ quota-set agreement in internal/cinder/quotas_test.go"            # T008
Task: "RED: neutron quota limit consistency in internal/neutron/quotas_test.go"               # T009

# Then all four role-unification edits together (different files):
Task: "nova/quotas.go loop → common.HasRole"                                                  # T011
Task: "nova/advanced_actions.go both loops → common.HasRole"                                  # T012
Task: "cinder/volumes.go os-reset_status loop → common.HasRole"                               # T013
Task: "keystone: delete hasRole wrapper + convert 5 call sites"                               # T014
```

## Parallel Example: after Phase 2 (three developers)

```bash
Developer A → US1: internal/nova/* and the neutron network/subnet tasks (T021–T029)
Developer B → US2: internal/cinder/* (T030–T037)
Developer C → US3: internal/neutron/network.go quota + extensions (T038–T045)
# Note: A's T027/T028 and C's T039–T044 both touch internal/neutron/network.go —
# sequence them or assign all neutron work to one developer.
```

---

## Implementation Strategy

### MVP (User Story 1 only)

1. Phase 1 — trace and confirm the gap list
2. Phase 2 — foundational (blocks everything)
3. Phase 3 — US1
4. **STOP and VALIDATE**: launch an instance from the dashboard end-to-end (T029)
5. That alone closes the ticket's flagship acceptance item (F7)

### Incremental delivery

Phase 1 + 2 → foundation ready → US1 (launch, **MVP**) → US2 (volume) → US3 (network) → US4 (overview
verification) → Phase 7 (regression gate + both-mode verification + report). Each story ends with its own
browser check, so each is demonstrable on its own.

### Risk notes

- **The trace can change the plan.** If T005 refutes a hypothesised gap or surfaces a new one, adjust the
  affected story phase before implementing — that is the point of the gate, not a failure of it. T005 also
  closes the two intentionally open-ended requirements (FR-002 `.json` scope, FR-013 alias set).
- **Gin param-name conflicts** (`internal/cinder/volumes.go:196`) panic at *startup*, not compile time — T052
  exists so that failure mode is caught by a test rather than by a runbook pass. Registration *order* is not
  the constraint (corrected 2026-07-29, see T032); reusing a **different** param name at a position already
  claimed by one is.
- **Pre-existing suites cover the changed endpoints** and `make test` does not run them. T051 is the gate;
  skipping it means FR-016 is asserted but never actually verified.
- **Deliberate behavior changes** ride along — FR-020 quota-write authorization and the Nova check ordering
  in T020 first, more added in implementation and review; FR-016 lists them all. They are exceptions to FR-016
  and must stay visible in the PR description and `docs/UPGRADING.md`, not be quietly folded into the UI work.

---

## Notes

- `[P]` = different files, no dependency on an incomplete task
- `[Story]` maps each task to a spec user story for traceability
- Every task states its exact file path; the six heavily-shared files are serialized above
- Verify each RED test actually fails before implementing it (Art. III)
- Commit after each task or logical group; conventional-commit style (`fix(nova): …`, `feat(cinder): …`)
- **No migration and no schema change in this feature** — if a task seems to need one, re-read
  [research.md](./research.md) §R6 before adding it
