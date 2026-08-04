---
description: "Task list for 022-project-tenant-isolation (cloud-storage#545)"
---

# Tasks: Project/Tenant Lifecycle & Isolation Fixes

**Input**: Design documents from `docs/specs/022-project-tenant-isolation/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md
**Branch**: `feature/545` | **Ticket**: [cloud-storage#545](https://github.com/cobaltcore-dev/cloud-storage/issues/545)

**Tests**: **REQUIRED, not optional.** Constitution Article III makes test-first non-negotiable, and the
spec makes it explicit: FR-021 requires a failing-before / passing-after pair per defect, FR-022
requires automated coverage of the routing shape and the entitlement decision.

**Organization**: One phase per user story. Story order follows the ticket's real dependency chain
(US1 → US4 → US2), with the independent P2 story last.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel — different file, no dependency on an incomplete task
- **[Story]**: US1 / US2 / US3 / US4, mapped to spec.md's user stories

## Path Conventions

Single Go module, services under `internal/<service>/`, bash suites under `test/`. All paths are
repository-relative from `/home/fnk/code/sap/ora/o3k/feature/545`.

## Story map

| Story | Priority | Defect | Files |
|---|---|---|---|
| **US1** — An operator can create a tenant | P1 | B5 / P1 | `internal/keystone/{domain_resolve.go,handlers.go,groups.go,domains.go}` |
| **US4** — The isolation suite reports on something real | P1 | T1 + guards | `test/multi_tenant_test.sh` |
| **US2** — A tenant cannot write into another tenant's network 🔒 | P1 | B8 | `internal/neutron/network.go` |
| **US3** — The project-scoped volume URL answers | P2 | B2 | `internal/cinder/volumes.go` |

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: A working stack and a green baseline, so any later failure is attributable to this feature.

- [X] T001 Bring up the stub stack and confirm it answers: `export HORIZON_HTTP_PORT=8080 O3K_JWT_SECRET=dev-secret-change-me`, then `docker compose -f deployments/docker-compose-horizon.yml up -d --build`, then export the `OS_*` variables from `docs/specs/022-project-tenant-isolation/quickstart.md` §0 and confirm `openstack token issue` returns a non-empty id
- [X] T002 [P] Provision the unit-test database and export `O3K_TEST_DB_URL=postgres://o3k:secret@localhost:5432/o3k_test?sslmode=disable` (matching `.github/workflows/ci.yml:100`); confirm `go test ./internal/keystone/...` runs DB-backed tests rather than skipping them
- [X] T003 [P] Record the baseline gate state before any edit: run `make test`, `make lint`, `make test-integration` and `go test ./test/contract/keystone/...` and note the result of each in the evidence log
- [X] T004 [P] Create the evidence log `docs/specs/022-project-tenant-isolation/EVIDENCE.md` with one section per defect (B5/P1, B8, B2, harness), each holding a RED and a GREEN subsection — this is the artifact FR-021 and SC-007 are checked against

**Checkpoint**: stack reachable, DB-backed tests actually run, baseline gates recorded.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Capture the failing-before evidence. **This phase is genuinely blocking and irreversible**:
once US1 lands, B5's `500` can no longer be reproduced, so its RED state must be recorded first.

**⚠️ CRITICAL**: No implementation task in Phase 3+ may begin until T005–T010 (including T007a) are
recorded in `EVIDENCE.md`.

- [X] T005 Record B5/P1 RED: `openstack project create --domain default probe` → `500`; `openstack user create --domain default probe-u --password x` → `400 specified domain not found or disabled`; `openstack domain show default` → `404`; `GET /v3/domains?name=Default` returns more than one domain; `POST /v3/projects` with an unknown `domain_id` → `500`; the same project name twice → `500` on the second (quickstart §2 RED)
- [X] T006 [P] Record B8 RED: the static check `awk '/func \(svc \*Service\) CreateSubnet/,/^}/' internal/neutron/network.go | grep -c 'FROM networks'` → `0`; and, once two tenants exist, tenant B posting a subnet onto tenant A's network → `201`, with tenant A's subnet list showing it (quickstart §4 RED). If tenant creation is blocked by B5, record the static half now and the live half immediately after US1 lands
- [X] T007 [P] Record B2 RED: `awk '/v3 := r.Group\("\/v3\/:project_id"\)/,0' internal/cinder/volumes.go | grep -c 'v3.GET("/volumes",'` → `0`; `GET /v3/$P/volumes` → `405`; `GET /v3/$P/volumes/detail` → `404` or `500` (quickstart §3 RED)
- [X] T007a [P] Record that `test/horizon_compat_test.sh` **already fails** on B2 — it is a pre-existing failing suite this feature repairs, so it is free RED→GREEN evidence. Run it and record the two failing checks: "Cinder - List volumes" hits `GET /v3/${PROJECT_ID}/volumes` (line 131, the `405`) and the Volumes-tab check hits `GET /v3/${PROJECT_ID}/volumes/detail` (line 210, the shadowed route). Note the total pass/fail counts so the GREEN comparison in T054a is exact
- [X] T008 [P] Record harness RED: `make test-multi-tenant` fails at setup with `PROJECT1=null`, or reports passes for checks that ran against a tenant that was never created (quickstart §5 RED)
- [X] T009 [P] Verify FR-020 — the three suites repaired in the #534 review still extract a real token from the `X-Subject-Token` header, not a body field: confirm `test/multi_tenant_test.sh:41,96,101`, `test/performance_test.sh:28` and `test/vxlan_multinode_test.sh:243,315`; record the check. Do **not** re-fix them
- [X] T010 [P] Confirm the R6 finding still holds before relying on it: the `/v3/:project_id` group in `internal/cinder/volumes.go` registers **zero** `backups` routes, so `GET /v3/$P/backups/detail` is absent rather than shadowed and must stay a `404` throughout

**Checkpoint**: every defect has recorded RED evidence. Implementation may begin.

---

## Phase 3: User Story 1 — An operator can create a tenant (Priority: P1) 🎯 MVP

**Goal**: `domain_id: "default"` — the value every OpenStack client sends — resolves instead of reaching
a `uuid` column, on both the creation and the retrieval paths. Unknown domains and duplicate names get
honest client errors instead of generic faults.

**Independent Test**: create a tenant naming the default domain the standard way; confirm it appears in
the tenant list and can be scoped to. Repeat with a nonexistent domain and a duplicate name; confirm
each is refused with a message naming the cause. Retrieve the default domain by alias and filter the
domain list by name.

**Why first**: nothing else in this feature or the parent issue is demonstrable until a project can be
created — the multi-tenancy suite cannot reach its first assertion.

### Tests for User Story 1 ⚠️ Write first, confirm RED

- [X] T011 [P] [US1] Write resolver unit tests in `internal/keystone/domain_resolve_test.go`: alias `"default"` → the Default domain id; `""` → same id; a valid UUID of an enabled domain → that id; a UUID of a **disabled** domain → not-found; a UUID naming no domain → not-found; a malformed string → not-found **without issuing a query**; a database failure → query-failed (distinct from not-found). Follow the `O3K_TEST_DB_URL` gating in `internal/keystone/maindb_test.go`
- [X] T012 [P] [US1] Write handler tests in `internal/keystone/identity_domain_alias_test.go` per `contracts/keystone-domain-resolution.md` (FR-001, FR-002, FR-004, FR-005, FR-006): `POST /v3/projects` with alias → `201` and `domain_id` in the response is the stored **UUID** (FR-007); omitted → `201`, default domain (FR-003); valid UUID → `201`; unknown UUID → `400` whose message names the domain field; disabled domain → `400`; duplicate `name` → `409` naming the duplicate; `POST /v3/users` with alias → `201` and a non-admin caller still gets `403` before any domain lookup; `POST /v3/groups` with alias → `201`, unknown → `400`
- [X] T012a [P] [US1] In the same file, add a **table-driven no-5xx case** over the enumerated rejected creation payloads SC-002 now names — unknown domain, disabled domain, non-identifier domain reference, duplicate name, missing required `name` — asserting each returns a **4xx** and that **none** returns a `5xx`. Bounded table, not a fuzzer: SC-002 was narrowed from an unbounded "any well-formed request" claim to exactly this set, so the test and the criterion state the same thing
- [X] T013 [P] [US1] Write domain read tests in the same file: `GET /v3/domains/default` → `200` with the Default domain; `GET /v3/domains/<uuid naming nothing>` → Keystone-verbatim `404`; `GET /v3/domains?name=Default` → exactly one; `?name=default` → **empty** (names stay case-sensitive); `/v3/domains` unfiltered → all domains; `?enabled=true` → all domains (other filters remain unsupported)
- [X] T014 [P] [US1] Extend `TestRegisterRoutesMiddlewareWiring` in `internal/keystone/routes_wiring_test.go:19` for the moved route: `GET /v3/domains/not-a-uuid` must still return the verbatim `404` body `Could not find domain: not-a-uuid.` — now produced by the handler rather than the middleware — while `PATCH` and `DELETE /v3/domains/:id` still get it from the middleware. Needs no database
- [X] T015 [US1] Run T011–T014 (including T012a) and confirm they **FAIL** (RED). Record which assertions fail and why in `EVIDENCE.md`

### Implementation for User Story 1

- [X] T016 [US1] Create `internal/keystone/domain_resolve.go` with `resolveDomainRef(ctx, ref) (id string, outcome)` per `contracts/keystone-domain-resolution.md` (FR-001, FR-003, FR-008): `""` and `"default"` → `SELECT id FROM domains WHERE name = 'Default' AND enabled = true`; a `uuid.Validate`-passing ref → `SELECT id FROM domains WHERE id = $1 AND enabled = true`; anything else → not-found with no query. Return a tri-state (`resolved` / `notFound` / `queryFailed`) so callers can answer `400` for an absent domain and `500` for an unreachable database. Model the doc comment on `common.ResolveProjectRef` (`internal/common/project.go`) and state why resolution is by **name**: migrations `009` and `037` seed the same domain name with different UUIDs, so a hard-coded id would depend on migration ordering (research R2)
- [X] T017 [US1] `CreateProject` in `internal/keystone/handlers.go:441-486`: replace the hard-coded `domainID = "00000000-0000-0000-0000-000000000100"` at line 456 with a `resolveDomainRef` call. On not-found answer `400` via `common.NewBadRequestError` with a message naming the domain field; on query-failed answer `500`. Resolve **before** the `INSERT` so a refused request creates no project (FR-002)
- [X] T018 [US1] `CreateProject` (same file, insert error path) (FR-004): map the `projects_name_key` unique violation to `409` via `common.NewConflictError`, naming the duplicate project name. Detect it by SQLSTATE `23505`, **not** by the string matching used at `handlers.go:1049` (research R3). Branch review moved the test itself into `common.IsUniqueViolation` once `CreateGroup` became a third caller (T046e)
- [X] T019 [US1] `CreateUser` in `internal/keystone/handlers.go:999-1022` (FR-001, FR-005): route its domain lookup through `resolveDomainRef` so the alias is accepted. Preserve the admin gate at line 972 (it must still run first), the existing `400 "specified domain not found or disabled"` message text, and the existing duplicate-user `409` path including its string-matching implementation — rewriting that is out of scope (research R3)
- [X] T020 [US1] `RegisterRoutes` in `internal/keystone/handlers.go:115-120`: move `GET /domains/:id` from the `val` group to the `mgmt` group so `uuidParamMiddleware` no longer pre-empts the alias. Leave `PATCH /domains/:id`, `DELETE /domains/:id` and `GET /domains/:id/config` on `val`. Comment the move in the style of the val/mgmt note at lines 57-66, stating that the exception is one route and one alias (research R4, plan Complexity Tracking)
- [X] T021 [P] [US1] `CreateGroup` in `internal/keystone/groups.go:73-77`: replace its copy of the hard-coded default with `resolveDomainRef`, answering `400` on not-found. This handler carries `CreateProject`'s identical failure mode (FR-006, A3)
- [X] T022 [P] [US1] `GetDomain` in `internal/keystone/domains.go:109`: resolve the alias via `resolveDomainRef` before the lookup, then validate the id in-handler and emit the Keystone-verbatim `404` (`Could not find domain: <id>.`) for any other non-UUID value, preserving what the middleware used to produce. Response body stays unchanged — the stored UUID, never the alias (FR-007, A2)
- [X] T023 [US1] `ListDomains` in `internal/keystone/domains.go:14`: honour the `name` query parameter with an exact match. It currently issues an unfiltered `SELECT … ORDER BY name ASC` and ignores every parameter, so a client's name lookup silently receives all domains. Keep returning `{"domains": [...]}` with `[]` — never `null` — when empty, and leave all other filters unsupported (FR-001a, Out of Scope)
- [X] T024 [US1] Retire the now-dead hard-coded default-domain literals in `handlers.go` and `groups.go` (FR-003). Keep `defaultDomainID` in `internal/keystone/bootstrap.go:20` — bootstrap creates the row and legitimately needs a fixed id. Leave `CreateRole` (`handlers.go:658`) untouched: it parses `domain_id` and discards it, so it has no failure mode (A3)
- [X] T025 [US1] Run T011–T014 (including T012a) and confirm they **PASS** (GREEN)
- [X] T026 [US1] Confirm `go test ./test/contract/keystone/...` still passes — **note: these tests hit a live server at `localhost:35357`, so they must be run against a build of THIS branch to mean anything; a run against another instance validates that instance, not this change** — it asserts the stored UUID `…0100` in five places and is the regression guard for A2/FR-007 and for the name-based resolution in T016
- [X] T027 [US1] Record B5/P1 GREEN in `EVIDENCE.md` against the T005 RED: project create with alias, user create with alias, `openstack domain show default`, `?name=Default` → 1, `?name=default` → 0, unknown domain → `400` naming the field, duplicate → `409`, group create with alias → `201`, and `openstack project show probe -f value -c domain_id` returning a UUID rather than the alias (quickstart §2 GREEN)

**Checkpoint**: tenants can be created by the standard command. US4 is now unblocked.

---

## Phase 4: User Story 4 — The isolation suite reports on something real (Priority: P1)

**Goal**: the multi-tenancy suite either exercises two genuinely distinct tenants or stops with a clear
failure. It never reports success while having created nothing, and it is re-runnable.

**Independent Test**: run the suite against a stack where tenant creation is broken and confirm it stops
with a named failure and zero reported passes; then run it against a healthy stack and confirm it
exercises both tenants and passes. Abort a run mid-way and confirm the next run starts clean.

**Why here**: fixing the harness *before* the isolation fix is what makes US2 provable rather than
asserted. It also depends on US1 — the suite's setup creates two tenants.

- [X] T028 [US4] Add a guard helper to `test/multi_tenant_test.sh` that fails on an empty value **or** the literal string `null`, printing what could not be created and exiting non-zero. The `null` half is the defect, not defensiveness: `jq -r` prints `null` for a missing field and `[ -z "null" ]` is false, so the existing `ADMIN_TOKEN` guard shape at line 43 could never have caught it
- [X] T029 [US4] Apply the guard to all seven setup values: `PROJECT1_ID` (line 48), `PROJECT2_ID` (55), `USER1_ID` (63), `USER2_ID` (70), `MEMBER_ROLE_ID` (76), `USER1_TOKEN` (92), `USER2_TOKEN` (99). Only `ADMIN_TOKEN` is guarded today. Guarantee: when setup cannot complete, **zero** checks run and **zero** passes are reported (FR-017, SC-006)
- [X] T029a [US4] Guard the two **role assignments** as well (`test/multi_tenant_test.sh:81-86`), which FR-017 names explicitly and which T029 does not reach — `MEMBER_ROLE_ID` is the role *lookup*, not the assignment. Both `PUT /v3/projects/$P/users/$U/roles/$R` calls currently pipe to `/dev/null` with **no status check**; capture the status and require `204` (what `AssignRole` returns, `internal/keystone/handlers.go:796`). This is the sharpest remaining vacuous-pass risk: a silently failed assignment yields role-less tokens, every isolation check then refuses for the wrong reason, and the suite reports passes for isolation it never exercised (FR-017)
- [X] T030 [US4] Move the end-of-script cleanup block (`test/multi_tenant_test.sh:263-305`, between the `# Cleanup` header and `# Summary` at line 307) into an `EXIT` trap registered **before** the first resource is created. Move, do not duplicate. It must tolerate ids that were never created (unset or `null`) silently, delete dependants before dependencies (servers → volumes/networks/images → users → projects), and preserve the script's exit code rather than masking a failure with its own success (FR-017a)
- [X] T031 [US4] Verify the guards fire, using a mechanism that survives US1 having fixed tenant creation: run the suite with a deliberately invalid admin token (`ADMIN_TOKEN=not-a-token bash test/multi_tenant_test.sh`, after making the script honour a pre-set value, or temporarily point `OS_AUTH_URL`'s port at a closed socket). Project creation then returns `401`, `jq -r '.project.id'` prints `null`, and T028's guard must fire. Confirm: exit code non-zero, a message naming what could not be created, and **zero** `✓` lines in the output. Prefer the invalid-token route — a closed socket fails at `ADMIN_TOKEN` and so exercises the pre-existing guard rather than the new ones
- [X] T032 [US4] Verify re-runnability: run `make test-multi-tenant` twice and confirm both runs pass and agree; then abort a run mid-way and confirm the next run starts clean rather than failing on a leftover `test-project-1` conflict — the failure mode T018's `409` and T029's early exit would otherwise create together (SC-006a)
- [X] T033 [US4] Record harness GREEN in `EVIDENCE.md` against the T008 RED, including the deliberately-broken-setup run and the two consecutive runs

**Note**: the two *new checks* this suite must gain (FR-018, FR-019) are implemented inside US2 (T045)
and US3 (T053), where their subject matter lives — a check asserting refusal cannot be written before
the refusal exists. spec.md's US4 acceptance scenario 3 is worded to match: it is satisfied by the end
of US2 and US3, not within this phase. US4's *Independent Test* is unaffected and is fully satisfiable
here (T031, T032).

**Checkpoint**: the suite reports on something real and can be run repeatedly. US2 can now be proven.

---

## Phase 5: User Story 2 — A tenant cannot write into another tenant's network (Priority: P1) 🔒

**Goal**: adding a subnet verifies the caller's entitlement to the target network. Only the owner or an
administrator may define a network's address ranges; a shared network grants no such right.

**Independent Test**: with two tenants, have tenant B attempt to add a subnet to tenant A's network;
confirm refusal and that A's network and subnet list show no trace. Confirm B can still add a subnet to
B's own network, and that an administrator can add one anywhere.

**Why last among the P1 stories**: it is the security fix, and it lands on a harness that now
authenticates — so the refusal is demonstrated, not asserted.

### Tests for User Story 2 ⚠️ Write first, confirm RED

- [X] T034 [P] [US2] Write the entitlement matrix in `internal/neutron/subnet_ownership_test.go` — all eight rows of `contracts/neutron-subnet-create.md` (FR-009, FR-011, FR-012, FR-022): own network → `201`; foreign private → `404`; foreign **shared** → `403`; admin on a foreign network → `201`; admin on own → `201`; well-formed id naming nothing → `404`; malformed id → `400`; `network_id` absent → `400`. Use the `newNeutronTestRouter` helper (`internal/neutron/network_db_test.go:54`) and the `O3K_TEST_DB_URL` gating
- [X] T034a [P] [US2] Assert row 1's **response shape** is unchanged, not merely its status (FR-013): the `201` body must still carry every field it does today — `id`, `name`, `network_id`, `tenant_id`, `cidr`, `gateway_ip`, `ip_version`, `enable_dhcp`, `dns_nameservers` (an empty slice, never `null`), `allocation_pools`, `host_routes`, `service_types`, `created_at`, `updated_at`. "Same result" is easy to satisfy accidentally; "same response shape" is what FR-013 actually promises a working client
- [X] T035 [P] [US2] In the same file, assert **no residue** after each refusal: no row in `subnets` for rows 2, 3, 6 and 7, and the target network's own `GET /v2.0/networks/{id}` and `GET /v2.0/subnets` views unchanged (FR-010, SC-003)
- [X] T036 [P] [US2] In the same file, assert the audit record: exactly **one** `warn` carrying the calling project, the target network and the network's owner for rows 2 and 3; and **none** for rows 1, 4, 5, 6, 7, 8 — a malformed or unlucky request is not a cross-tenant attempt, and a legitimate creation is not an event (FR-011b, SC-004b). Capture the `zerolog` output via a test writer
- [X] T037 [US2] Run T034–T036 (including T034a) and confirm they **FAIL** (RED) — specifically that the foreign-network rows currently return `201`. Record in `EVIDENCE.md`

### Implementation for User Story 2

- [X] T038 [US2] `CreateSubnet` in `internal/neutron/network.go:908-946`: validate `req.Subnet.NetworkID` with `uuid.Validate` and answer `400` naming the network field on failure, before any query. Without this the string reaches a `uuid`-typed comparison and Postgres raises `22P02`, surfacing as the `500` this feature exists to remove (FR-011a)
- [X] T039 [US2] `CreateSubnet` (FR-009): add `SELECT project_id, shared FROM networks WHERE id = $1` before the `INSERT`. Select the two columns rather than filtering on them — `CreatePort` (`internal/neutron/ports.go:58`) folds the decision into its `WHERE` clause and therefore cannot separate `403` from `404` (research R5)
- [X] T040 [US2] `CreateSubnet`: implement the decision — no row → `404` via `common.NewNotFoundError("network")`; owner match → allow; `common.HasRole(c, common.RoleAdmin)` → allow; `shared = true` → `403` via `common.NewForbiddenError`; otherwise → `404`. This is `internal/neutron`'s first use of `HasRole`, so keep the call shape identical to the existing users in `internal/common/project.go` (FR-010, FR-010a, FR-012)
- [X] T041 [US2] `CreateSubnet` (FR-009, FR-013): place the whole check **before** both the `INSERT` (line 943) and the `applyDHCPForNetwork` call (line 958), so a refused request starts no address assignment on the victim's network. Note in the code that `applyDHCPForNetwork` returns early in stub mode (`internal/neutron/reconciler.go:232`), so this ordering is observable only in real mode — which is why it is a requirement rather than something a stub test would catch (FR-010)
- [X] T042 [US2] `CreateSubnet`: emit the `warn` audit record for the two cross-tenant refusals only, with the calling project, target network and network owner, using the package's existing `zerolog` logger (as at `network.go:949`) (FR-011b)
- [X] T043 [US2] Comment the divergence from `CreatePort` at the point of decision in `CreateSubnet`: port creation admits shared networks, subnet creation does not, because upstream's `create_subnet` policy is `admin_or_network_owner` — sharing lets other tenants attach to a network, never define it. Two neighbouring handlers that read alike but decide differently is the shape that gets "unified" back into a defect (FR-010, A5, spec Note 3)
- [X] T044 [US2] Run T034–T036 (including T034a) and confirm they **PASS** (GREEN). Confirm the static check `awk '/func \(svc \*Service\) CreateSubnet/,/^}/' internal/neutron/network.go | grep -c 'FROM networks'` is now non-zero
- [X] T045 [US2] Extend the read-only network isolation check in `test/multi_tenant_test.sh:146-166` with the cross-tenant **write** (FR-018): user2 posts a subnet onto user1's network → refused, never `201`; user1's `GET /v2.0/networks/{id}` and `GET /v2.0/subnets` stay clean; **and** user2 creates a subnet on user2's own network → `201`. The last step matters as much as the first — a check that only proves refusal would pass against a handler that refuses everything
- [X] T046 [US2] Record B8 GREEN in `EVIDENCE.md` against the T006 RED: private foreign → `404`, shared foreign → `403`, no residue in the victim's views, own-network and admin paths both still `201`, nonexistent → `404`, malformed → `400`, and the warn count (quickstart §4 GREEN)

Added during the branch review (2026-08-06) — B8's administrator exemption (FR-012) leaves a subnet
whose project differs from its network's, and the read paths did not account for it:

- [X] T046a [US2] `GetSubnet` (`network.go:1189`), `UpdateSubnet` (`network.go:1296`) and `DeleteSubnet`
  (`network.go:1234`): add upstream's `network_owner` disjunct — `OR n.project_id = $2` — so the owner of a
  network can retrieve, rename and delete a subnet another project attached to it (FR-013a). `Update` and
  `Delete` need a join on `networks` to see the column; `GetSubnet` already has one. **No** `shared` disjunct
  on the two mutating paths: upstream puts `rule:shared` in `get_subnet` and deliberately not in
  `update_subnet`/`delete_subnet`, whose check_strs are identical. No administrator bypass on any of the
  three — that is #549's sweep
- [X] T046b [US2] `DeleteSubnet` and `UpdateSubnet`: drop `project_id` from the final `DELETE`/`UPDATE`,
  which the entitlement query above has already decided. Keeping both made a network owner's call a silent
  no-op — `204`/`200` with the row unchanged, because the two predicates disagreed. Caught by T046c, not by
  review
- [X] T046b1 [US2] `UpdateSubnet`, `GetSubnet` and `ListSubnets`: report `tenant_id` from the subnet row,
  not from the token. All three used `projectID`, correct only while each could return nothing but the
  caller's own rows — an assumption SPEC-020 broke for the listing and FR-013a broke for the by-id read.
  A network owner would otherwise be told another project's subnet is theirs. `CreateSubnet` keeps the
  caller, which is the project it just wrote. The two read paths were found by the **host pass** (§2.4a),
  after T046c had gone green — see T046g
- [X] T046c [P] [US2] Tests in `internal/neutron/subnet_ownership_test.go` (FR-013a): network owner fetches,
  renames and deletes a foreign-owned subnet on their own network — asserting the rename is **stored**, not
  merely answered `200`, and that `tenant_id` names the owner; a caller owning neither still gets `404` on
  all three and the row is unchanged; a shared network grants read but not delete
- [X] T046d [US2] Reconcile the docs that recorded these paths as sound before the review —
  `contracts/neutron-subnet-create.md` (scope-boundary table + closing claim + test obligations),
  `data-model.md` (adjacent-paths table), `research.md` R5, and `spec.md` (FR-013a, A5a, FR-023)
- [X] T046e [P] Extract `common.IsUniqueViolation` (`internal/common/pgerror.go`) and route all three
  SQLSTATE `23505` sites through it — `CreateProject`, `CreateGroup` and `internal/nova/flavors.go:62`.
  T018 chose the right test but left a copy per caller; `CreateGroup` made that three. The extraction
  also repairs `flavors.go`, whose bare `err.(*pgconn.PgError)` assertion misses a wrapped error and
  would downgrade a duplicate flavor name from `409` to `500`. Retires the package-local
  `pgUniqueViolation` in `internal/keystone/identity_errors.go`; update research R3 (T046f)
- [X] T046f [P] Test the helper (`internal/common/pgerror_test.go`): the bare violation, wrapped and
  doubly-wrapped, a different SQLSTATE, a plain error and `nil`. The wrapped cases are the point —
  they are what the assertion form got wrong
- [X] T046g [US2] Close the gap the host pass exposed: `TestForeignSubnetReportsItsOwnTenantID` asserts
  `tenant_id` on the **by-id read** and **in the listing**, not only on a mutation's response body. T046c
  passed while both read paths were wrong because it asserted on the `PUT` response, which T046b1 had
  already fixed — an assertion that never reads the field it is judging cannot fail on it. Record the
  live §2.4a run in `EVIDENCE.md`, including the defect it found
- [X] T046h [US2] Run the harness end to end against a live stack: `make test-multi-tenant` twice back to
  back with no manual cleanup (FR-017a), then confirm from the server log that teardown deletes
  `project2-subnet` **before** `project2-network` and that the network delete now answers `204` rather
  than the `409` the review found. Check residue in `subnets`, `networks` and `projects` afterwards

**Checkpoint**: the cross-tenant write is closed and provable. All P1 stories complete.

---

## Phase 6: User Story 3 — The project-scoped volume URL answers (Priority: P2)

**Goal**: the project-in-path form of the block-storage URL lists volumes, and the three sub-resource
names a sibling by-identifier route currently swallows return their own view.

**Independent Test**: list volumes on the project-scoped URL form and confirm the same collection the
flat form returns; request the three shadowed sub-resource views and confirm each returns that view.

**Independent of US1, US2 and US4** — may be implemented at any point after Phase 2.

### Tests for User Story 3 ⚠️ Write first, confirm RED

- [X] T047 [P] [US3] Extend `TestRouteRegistration_NoWildcardConflict` (`internal/cinder/quotas_test.go:349`) into a **no-shadow guard** over the project-scoped group — it guards only flat routes today. This is not a parity guard: full flat↔project parity is decision Q2's rejected option C (FR-022). Assert all four added routes resolve to a handler rather than falling through to `404`, and that `RegisterRoutes` still registers without panicking
- [X] T048 [P] [US3] Write handler tests using `newCinderTestRouter` (`internal/cinder/volumes_db_test.go:63`): `GET /v3/:project_id/volumes` returns the caller's volumes and `[]` (never `null`) when there are none; a **foreign** `:project_id` in the path returns only the caller's own volumes (FR-015); the three shadowed paths (`volumes/detail`, `snapshots/detail`, `types/default`) return their view rather than a by-identifier lookup; and `GET /v3/:project_id/backups/detail` still returns `404`, guarding the R6 finding that it is absent rather than shadowed
- [X] T049 [US3] Run T047–T048 and confirm they **FAIL** (RED) — `GET /volumes` as `405`, the three shadowed paths as a by-identifier lookup. Record in `EVIDENCE.md`

### Implementation for User Story 3

- [X] T050 [US3] In the `/v3/:project_id` group of `internal/cinder/volumes.go` (declared at line 197), add `v3.GET("/volumes", svc.ListVolumes)` beside the existing `POST /volumes` at line 199. Every handler in that group reads `project_id` from the token and ignores the path segment, so this is behaviourally identical to the flat route and the segment cannot widen scope (FR-014, FR-015)
- [X] T051 [US3] Add the three shadowed sub-resources to the same group, wired to the handlers the flat routes already use: `v3.GET("/volumes/detail", svc.ListVolumesDetail)` (`volumes.go:495`), `v3.GET("/snapshots/detail", svc.ListSnapshotsDetail)` (`:1153`), `v3.GET("/types/default", svc.GetDefaultVolumeType)` (`:1204`). Do **not** add `GET /backups/detail`: the group registers no backup route at all, so it is absent rather than shadowed and already answers an honest `404` (FR-016, FR-016a, research R6)
- [X] T052 [US3] Rewrite the comment above the group (`internal/cinder/volumes.go:185-196`, immediately preceding `v3 := r.Group("/v3/:project_id")` at line 197). It currently states that the one gap is `GET /v3/{project}/volumes` and that it *"is filed as a real defect (cloud-storage#536 B2, a 405 that surprised a client)"* — false once this lands. Replace it with what is then true: the ticketed route and the three shadowed sub-resources are registered, and **15** routes remain absent by decision. List them, and record that they are absent rather than wrong so a later ticket closes them deliberately (FR-016a)
- [X] T053 [US3] Add a project-scoped volume **list** check to `test/multi_tenant_test.sh` (FR-019): user1 lists on `GET /v3/$PROJECT1_ID/volumes` → `200` including the volume created at line 171 (not `405`); and user2 lists on `GET /v3/$PROJECT1_ID/volumes` → `200` with user1's volume **absent**. The suite creates and deletes on this URL form (lines 171, 285) but never lists, which is exactly how B2 escaped it
- [X] T054 [US3] Run T047–T048 and confirm they **PASS** (GREEN). Record B2 GREEN in `EVIDENCE.md` against the T007 RED, including the `diff` of the two URL forms' collections (SC-005) and `backups/detail` still `404`ing as a deliberate non-change (quickstart §3 GREEN)
- [X] T054a [US3] Re-run `test/horizon_compat_test.sh` and confirm both previously failing volume checks now pass, with no new failures against the T007a baseline. SC-008 requires this suite to go from failing to passing rather than merely staying as it was

Added during the o3k#82 branch review (2026-08-27) — the shadowed set was four, not three:

- [X] T054b [US3] Register `v3.GET("/os-volume-transfer/detail", svc.ListVolumeTransfers)`. The group
  registers `GET /os-volume-transfer/:id` (`volumes.go:282`), so the literal `detail` bound to `:id` and
  `GetVolumeTransfer` answered `500 "failed to get transfer"` — the same signature as `snapshots/detail`
  and `types/default`, and as #533 F8. It had been filed among the merely-absent routes (FR-016, R6)
- [X] T054c [P] [US3] Add the case to `TestProjectScopedSubResourcesNotShadowed` (both the response-key
  assertion and the flat-vs-scoped agreement loop). RED proven by deleting the route from T054b and
  re-running: `code=500 body={"computeFault":…"failed to get transfer"}`
- [X] T054d [US3] Correct the census everywhere it appears — four shadowed, **14** absent: the group
  comment in `volumes.go`, `research.md` R6 (whose candidate table never listed `os-volume-transfer`),
  `spec.md` (FR-016, FR-016a, SC-005a, A10, Q2), `checklists/requirements.md`, `EVIDENCE.md`,
  `quickstart.md`, `plan.md` and `docs/blueprint.md` §2. Note `GET /os-volume-transfer` (the collection,
  POST-registered but not GET → `405`) stays in the absent set: same shape as the ticketed B2 defect,
  left for the follow-up rather than widened into this change

**Checkpoint**: all four user stories independently functional.

---

## Phase 7: Polish & Cross-Cutting Concerns

- [X] T055 [P] Reconcile `docs/blueprint.md:75`, which describes the flat and project-in-path volume forms as *"the **same** operation"* — true only after US3
- [X] T056 [P] Record the pre-existing gaps found during this work but deliberately left to #549, in `research.md` observations and the ticket: `CreateProject` has no admin gate while `CreateUser` does (A6/FR-023); `GET /v3/domains/default/config` still cannot resolve the alias (only the retrieval route was moved off the validating group); O3K's Neutron error envelope diverges from upstream's `NeutronError` service-wide (research R7); `CreateUser`'s duplicate detection still string-matches (research R3); migration `037`'s dead second `Default` seed (research R9)
- [X] T057 Run the full gate set and confirm every one is green: `make test`, `make lint`, `make test-integration`, `make test-multi-tenant`, `go test ./test/contract/keystone/...` and `bash test/horizon_compat_test.sh` — the last of which must be **better** than baseline, not merely unchanged (SC-008, T007a/T054a)
- [X] T058 Confirm no migration was added and no schema changed — `git status migrations/` must be empty (A9, data-model.md verdict)
- [X] T059 Execute `docs/specs/022-project-tenant-isolation/quickstart.md` end to end on the stub stack as a single host pass, including the teardown in §7
- [X] T060 Complete the evidence checklist at the end of quickstart.md — four defects, four recorded RED→GREEN pairs — and confirm `EVIDENCE.md` satisfies FR-021 and SC-007
- [X] T061 [P] Re-read the changed hunks for scope creep: five production files only (`internal/keystone/{domain_resolve.go,handlers.go,groups.go,domains.go}`, `internal/neutron/network.go`, `internal/cinder/volumes.go`), no new dependency in `go.mod`, no new package
- [ ] T062 Branch review against the ticket, then tick the checklist on cloud-storage#545 and post the weekly status comment

---

## Dependencies & Execution Order

### Phase dependencies

- **Phase 1 (Setup)** — no dependencies
- **Phase 2 (Foundational)** — depends on Phase 1. **BLOCKS every user story**, and irreversibly so: once US1 lands, B5's `500` cannot be reproduced again
- **Phase 3 (US1)** — depends on Phase 2
- **Phase 4 (US4)** — depends on **US1** (its setup creates two tenants)
- **Phase 5 (US2)** — depends on **US4** (so the refusal is proven by a suite that authenticates) and on US1 (it needs two tenants)
- **Phase 6 (US3)** — depends on Phase 2 only; independent of US1/US2/US4
- **Phase 7 (Polish)** — depends on all stories intended for the change

### Story dependency graph

```
Phase 1 → Phase 2 ─┬─→ US1 ──→ US4 ──→ US2 ──┐
                   │                          ├─→ Phase 7
                   └─→ US3 ───────────────────┘
```

This chain is the ticket's own, and it is real rather than stylistic: without US1 no tenant exists, and
without US4 the suite that proves US2 reports on nothing.

### Within each story

- Tests are written first and confirmed RED before implementation (Constitution Article III)
- The shared resolver (T016) precedes every call site (T017–T024)
- `internal/keystone/handlers.go` tasks (T017, T018, T019, T020) touch one file and run **sequentially**
- `internal/keystone/domains.go` tasks (T022, T023) touch one file and run **sequentially**
- Cinder route tasks (T050, T051, T052) touch one file and run **sequentially**
- Neutron `CreateSubnet` tasks (T038–T043) touch one function and run **sequentially**

### Parallel opportunities

| Group | Tasks | Why safe |
|---|---|---|
| Setup | T002, T003, T004 | independent commands / new file |
| Foundational evidence | T006, T007, T007a, T008, T009, T010 | read-only checks on different subjects |
| US1 tests | T011, T012, T012a, T013, T014 | two new files plus one existing test file, all distinct |
| US1 implementation tail | T021 (`groups.go`), T022 (`domains.go`) | different files, both depend only on T016 |
| US2 tests | T034, T034a, T035, T036 | one new file — write together, they share fixtures |
| US3 tests | T047 (`quotas_test.go`), T048 (`volumes_db_test.go`) | different files |
| Polish | T055, T056, T061 | docs and review, no code |
| Whole stories | US3 alongside US1/US4/US2 | different service package, no shared file |

---

## Parallel Example: User Story 1

```bash
# Launch the four US1 test tasks together (all distinct files):
Task: "Resolver unit tests in internal/keystone/domain_resolve_test.go"
Task: "Handler tests in internal/keystone/identity_domain_alias_test.go"
Task: "Domain read tests in internal/keystone/identity_domain_alias_test.go"
Task: "Extend TestRegisterRoutesMiddlewareWiring in internal/keystone/routes_wiring_test.go"

# After T016 (the resolver) lands, these two call sites are independent:
Task: "CreateGroup adopts resolveDomainRef in internal/keystone/groups.go"
Task: "GetDomain resolves the alias in internal/keystone/domains.go"
```

## Parallel Example: whole stories

```bash
# US3 shares no file with the P1 chain, so it can run alongside it:
Task: "US3 — Cinder project-scoped routes in internal/cinder/volumes.go"
Task: "US1 — domain resolution in internal/keystone/"
```

---

## Implementation Strategy

### MVP (User Story 1 only)

1. Phase 1 — Setup
2. Phase 2 — Foundational **(critical and irreversible: capture RED first)**
3. Phase 3 — US1
4. **STOP and VALIDATE**: `openstack project create --domain default probe` succeeds; the contract suite is green
5. This alone unblocks `make test-multi-tenant` and everything in #536 that needs a tenant

### Incremental delivery

1. Setup + Foundational → RED evidence banked
2. **US1** → tenants can be created → the parent issue becomes demonstrable
3. **US4** → the suite reports on something real
4. **US2** 🔒 → the cross-tenant write is closed, and provable
5. **US3** → the project-scoped URL form answers
6. Polish → docs, gates, host pass, review

Each step is independently valuable, and steps 2–4 are the ticket's stated order for good reason.

### Parallel team strategy

With two developers: one takes the P1 chain (US1 → US4 → US2) in order, the other takes US3 in
`internal/cinder/` — no shared file, no coordination beyond Phase 2. US3 is small enough that the
second developer should then pick up Phase 7's documentation tasks.

---

## Notes

- **67 tasks**: 4 setup, 7 foundational, 18 US1, 7 US4, 14 US2, 9 US3, 8 polish. Five were added by the
  cross-artifact analysis pass and carry letter suffixes — T007a, T012a, T029a, T034a, T054a — so the
  original T001–T062 numbering stays stable for anyone already referencing it
- Every test task is mandatory (Article III, FR-021, FR-022) — none is marked optional
- DB-backed tests need `O3K_TEST_DB_URL`; without it they skip silently and prove nothing
- **No migration.** If a task seems to need one, the plan is wrong — stop and re-read data-model.md
- Commit per task or per logical group; the branch is `feature/545`
- Stop at any checkpoint to validate a story independently
