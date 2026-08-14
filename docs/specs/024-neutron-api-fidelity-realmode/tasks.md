# Tasks: Neutron API Fidelity & Real-Mode Networking

**Input**: Design documents from `docs/specs/024-neutron-api-fidelity-realmode/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md
**Branch**: `feature/546` | **Base**: `f34d498`

**Tests**: **MANDATORY**, not optional. Constitution *Test-First Development* is NON-NEGOTIABLE, and
FR-038 requires each of the six defects to be demonstrated failing before the change and passing
after. Test tasks precede implementation in every phase, and each phase has an explicit "confirm RED"
gate.

**Organization**: one phase per user story. Phase order follows the plan's wave plan rather than
strict priority order — see the note below Phase 2.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: parallelizable — different file, no dependency on an incomplete task
- **[Story]**: US1–US6, mapping to spec.md

## Path conventions

Single-binary Go service. Source under `internal/` and `pkg/`, migrations in `migrations/`,
integration scripts in `test/`. Unit tests live beside their source as `*_test.go`.

---

## Phase 1: Setup (Baseline capture)

**Purpose**: record the pre-change state. `make test-all` is already red on B10, and the shared
`o3k-postgres` container is used by around a dozen worktrees — without a baseline this branch gets
blamed for someone else's stale row.

- [X] T001 Bring up the stack per quickstart.md and capture `make test-all` output to `/tmp/546-baseline-testall.txt`, confirming the single failure is `test/error_handling_test.sh` TEST 6
- [X] T002 [P] Record the shared-database state before trusting any red result: `openstack project list` and the `quotas` rows, per the known-poisoning gotcha
- [X] T003 [P] Capture the current pass counts for `make test-contract` (neutron 59) and `go test ./internal/neutron/...` to `/tmp/546-baseline-suites.txt`

**Checkpoint**: a known-good baseline exists; every later failure is attributable.

---

## Phase 2: Foundational (Blocking prerequisite)

**Purpose**: two things. A shared-type change in `internal/common/`, which every service uses — it
lands and is proven harmless *before* US2 builds the Neutron renderer on top of it, so "did I break
Nova?" and "did I build the envelope right?" stay separable. And the Constitution's test-strategy
approval, which gates the first RED in every story.

**⚠️ T004–T005 block US2 only** — US1, US3, US4, US5 and US6 do not need the shared-type change.
**T006 blocks every story**, because no test may be written before the strategy is approved
(Constitution *Test-First Development* step 2).

- [X] T004 Add an additive `Resource` field to `OpenStackError` in `internal/common/errors.go` and populate it in the resource-taking constructors (`NewNotFoundError`, `NewResourceNotFoundError`, `NewResourceConflictError`, `NewQuotaExceededError`) — the `type` value is derived from it, and today the resource is formatted into the message and discarded (research R1)
- [X] T005 Add `internal/common/errors_envelope_test.go` asserting the compute/identity/block-storage envelope is byte-identical before and after T004 — no wrapper key, field set or status changes for any non-Neutron service (FR-010)
- [X] T006 Review the test strategy with the user and get approval before the first RED gate — Constitution *Test-First Development* step 2, which sits between "write tests first" and "confirm RED". Present the per-defect RED sources from research R8 (two are already-committed checks, four are new) and the contract IDs each phase will assert, so the shape of the evidence is agreed before any test is written

**Checkpoint**: the shared type carries the resource; no other service's output moved; the test
strategy is approved.

---

> **Phase order deviates from spec priority, deliberately.** The plan sequences US2 (B10) ahead of
> US1 (F10) because B10 is the committed broken gate: until it lands, every other story's suite run
> is assessed against a red baseline (spec D4). US5 (B9, P3) is pulled ahead of US4/US3 because it is
> the smallest self-contained slice in the feature. Priorities in spec.md are unchanged.
>
> **File-level conflicts to respect** — these are not parallelizable even though the stories are
> logically independent:
> - US1 and US5 both edit `internal/neutron/ports.go` (different functions)
> - US4, US3 and US6 all edit `internal/neutron/network.go` (different functions)
> - **US3 → US6 is a hard sequence**: both modify the same three network response builders and add a
>   column to the same two `SELECT`s

---

## Phase 3: User Story 2 — Read a Neutron error with a Neutron client (Priority: P1) 🎯 First increment

**Goal**: every error from `:9696` carries Neutron's envelope, and `make test-all` stops being red.

**Independent Test**: request an absent network, subnet, port, router, floating IP and security group;
each carries `{"NeutronError": {type, message, detail}}` with no `code`. Then `make test-errors` is
10/10.

### Tests for User Story 2 ⚠️ write first, confirm RED

- [X] T007 [P] [US2] Contract tests E1 and E2 in `internal/neutron/error_envelope_test.go`: a 404 body has exactly the key `NeutronError`, and its inner object has exactly `type`/`message`/`detail` with **no `code`** — E2 asserts an *absence*, which is what catches a wrapper-only half-fix (FR-006, FR-008)
- [X] T008 [US2] Contract test E3 in `internal/neutron/error_envelope_test.go`: absent network/subnet/port/router/floating-IP/security-group each return their own `type` (`NetworkNotFound`, `SubnetNotFound`, …), not one shared value (FR-007)
- [X] T009 [US2] Contract tests E5, E6 and E7 in `internal/neutron/error_envelope_test.go`: an unauthenticated request, an unrouted path and a wrong method on `:9696` all return the Neutron envelope — these go through shared middleware and are the paths most likely to be missed (FR-008)
- [X] T010 [US2] Contract tests E4 and E8 in `internal/neutron/error_envelope_test.go`: a malformed body gives `type` `BadRequest`; an injected handler panic gives `500` with `type` `NeutronException` (FR-007a)
- [X] T011 [US2] Contract test E9 in `internal/neutron/error_envelope_test.go`: the auto-allocated-topology 404 is produced by the shared path, proving the hand-rolled block is gone (FR-009)
- [X] T012 [P] [US2] Gophercloud contract test in `test/contract/neutron/error_envelope_test.go`: drive an absent network, an unauthenticated call and an unrouted path through a real SDK client and assert the `NeutronError` envelope with no `code`, via `contract.SetupNeutronClient` + `skipIfO3KNotRunning`. The unit tests exercise the handler; this proves a real client parses the result (Constitution Art. I verification clause; E1, E2, E5, E6)
- [X] T013 [US2] Run the new tests and record RED in `docs/specs/024-neutron-api-fidelity-realmode/EVIDENCE.md`, alongside the already-failing `make test-errors` TEST 6 (FR-011, FR-038)

### Implementation for User Story 2

- [X] T014 [US2] Create `internal/common/neutron_errors.go` holding the **single** `(StatusCode, Code, Resource) → type` table from `contracts/neutron-errors.md`, including all 20 resource names and the `computeFault → NeutronException` generic; one place so the vocabulary cannot drift (FR-007a)
- [X] T015 [US2] Render the Neutron shape in `internal/common/errors.go` — `ToJSON`/`SendError` emit `{"NeutronError": {type, message, detail}}` when the request is marked, omitting `code` and mapping `Details` → `detail` (FR-006)
- [X] T016 [US2] Create `internal/neutron/errors.go` with the marker middleware, and register it in `cmd/o3k/main.go` `createNeutronServer` (`:626-637`) as the **first** `r.Use(...)`, ahead of `ErrorHandlingMiddleware`. **Engine level, not the route group**: auth runs at `:632` before any group exists, and `NoRoute` (`:633`) / `NoMethod` (`:635`) never run inside a group — a group-level marker would leave E5, E6 and E7 in the compute envelope while everything else passed, which is the half-fix FR-008 forbids. Neutron owns its `gin.New()` engine, so this cannot leak to another service. All 244 handler sites and every middleware path convert with no call-site edits (research R1, contracts/neutron-errors.md)
- [X] T017 [US2] Delete the hand-rolled envelope at `internal/neutron/auto_allocated_topology.go:30-34` and route it through `common.SendError` (FR-009)
- [X] T018 [US2] Prefer the id-carrying message form matching upstream's `Network <id> could not be found.` — **only where `NewResourceNotFoundError` is already used**, since it already receives the id. Do not change `NewNotFoundError` call sites to pass one: this is a quality improvement, not a requirement (research R2)
- [X] T019 [US2] Verify E11: `make test-errors` reports 10/10 and exits 0; append GREEN to `EVIDENCE.md`
- [X] T020 [US2] Verify E12 and E10: `make test-contract` still passes (it keys off status, which is why it never caught this), and Nova/Cinder/Glance/Keystone 404s are unchanged

**Checkpoint**: `make test-all` is no longer red on B10. Every later phase now has a real baseline.

---

## Phase 4: User Story 1 — Attach a guest to a tenant network (Priority: P1) 🎯 MVP

**Goal**: a port can be created, so a guest reaches the network and its metadata.

**Independent Test**: create a network and subnet, **restart the service**, then create a port — it
succeeds. In real mode the TAP and its bridge are in the same namespace and a booted guest fetches
metadata.

### Tests for User Story 1 ⚠️ write first, confirm RED

- [X] T021 [P] [US1] Contract test P3 in `internal/neutron/port_namespace_test.go`: creating a port on a network absent from the in-process bridge map succeeds — this is the stub half, and it reproduces the reported `500 failed to attach TAP to bridge` (FR-001, FR-002)
- [X] T022 [US1] Contract test P9 in `internal/neutron/port_namespace_test.go`: an attach failure leaves no TAP and no port row, and the error names the failing step (FR-005)
- [X] T023 [US1] Contract tests P7 and P8 in `internal/neutron/port_namespace_test.go`: a port on another project's shared network is allowed and lands where that network's bridge is; a by-name port never lands on another tenant's same-named network
- [X] T024 [US1] Run and record RED in `EVIDENCE.md` (FR-038)

### Implementation for User Story 1

- [X] T025 [P] [US1] In `pkg/networking/netns.go`, stop the stub branch of `AttachToBridge` (`:363-368`) consulting `stubBridges` — a recorded network is a valid attach target; process-local bookkeeping is not authority (FR-002)
- [X] T026 [US1] In `internal/neutron/ports.go` `CreatePort`, create and attach the TAP in the **root** namespace: `:89-90` and `:98` stop passing `inNamespace=true`/`nsName`. The bridge, dnsmasq and SNAT are already there, so the TAP is the lone outlier (FR-003, research R3)
- [X] T027 [US1] Update both rollback paths in `CreatePort` (`internal/neutron/ports.go:100`, `:115`) to delete the TAP from the namespace it was actually created in (FR-005)
- [X] T028 [US1] In `internal/neutron/ports.go` `DeletePort` (`:440`, `:471`), delete the TAP from the root namespace so create and delete agree — contract P10
- [X] T029 [US1] Distinguish the failing step in `CreatePort`'s error messages so a failure names what could not be done, rather than a generic server error (FR-005)
- [X] T030 [US1] Confirm `internal/neutron/reconciler.go` agrees with the new placement — it already ensures bridges and runs dnsmasq with an empty namespace, so verify no change is needed rather than assuming it
- [X] T031 [US1] Leave `internal/neutron/auto_allocated_topology.go:113-117` alone and note why in a comment: it creates its namespace first, so it is a separate self-consistent path
- [X] T032 [US1] Run and record GREEN; verify P4 (the ticket's own `openstack port create` repro) from quickstart.md

**Checkpoint**: ports can be created in stub mode across restarts. Real-mode verification is T092.

---

## Phase 5: User Story 5 — A security-group rule reads back the way it was created (Priority: P3)

**Goal**: all four descriptions of a rule agree, and a deliberate `0` survives storage.

**Independent Test**: create a rule with the optional fields unset; the create response, the by-id
read, the list and the group view are identical.

### Tests for User Story 5 ⚠️ write first, confirm RED

- [X] T033 [P] [US5] Extend `TestRuleValuesAgreeAcrossAllThreeEmitters` in `internal/neutron/security_group_shape_test.go:245` to cover the **create response** as a fourth emitter, and rename it accordingly — the current name is the evidence the create path was never in scope. Covers G1, G2, G3, G4, G5 and G8 (identical key sets across all four) (FR-029)
- [X] T034 [US5] Add contract test G6 in `internal/neutron/security_group_shape_test.go`: a rule created with `protocol: icmp, port_range_min: 0, port_range_max: 0` reports `0` **and stores `0`**, not `NULL` — this fails in storage today via `Valid: portMin > 0` (FR-028)
- [X] T035 [US5] Run and record RED in `EVIDENCE.md` (FR-038)

### Implementation for User Story 5

- [X] T036 [US5] In `internal/neutron/ports.go` `CreateSecurityGroupRule`, stop flattening the request pointers to sentinels at `:968-975` — the struct already declares `*int`/`*string` at `:945-949`, so the wire distinction arrives intact and is then destroyed (research R5)
- [X] T037 [US5] Fix the INSERT at `internal/neutron/ports.go:1020-1027`: `Valid` must reflect whether the pointer was supplied, not `> 0` / `!= ""`, so a deliberate `0` and an explicit `""` are stored as given (FR-028)
- [X] T038 [US5] Emit the pointers directly in the create response at `internal/neutron/ports.go:1042-1053` so unset values marshal to `null`, matching the other three emitters (FR-026, FR-027)
- [X] T039 [US5] Keep a local dereference with an explicit default **only** for the `networking.SecurityGroupRule` hand-off at `internal/neutron/ports.go:995` — a kernel rule has no concept of "unset"; that is the one legitimate sentinel and it must not leak back
- [X] T040 [US5] Run and record GREEN; verify G7 (the extended agreement test now passes) and G3/G4 by round-tripping a rule through all four descriptions

**Checkpoint**: the create response is no longer the odd one out.

---

## Phase 6: User Story 4 — Subnet listings answer the question asked (Priority: P2)

**Goal**: subnet filters are applied, gatewayless subnets are expressible and visible, and no row
disappears silently.

**Independent Test**: two networks with a subnet each; filtering by one returns only its subnet.
Separately, a subnet created with no gateway is listed, shown and updated.

### Tests for User Story 4 ⚠️ write first, confirm RED

- [X] T041 [P] [US4] Pure unit test for `buildSubnetListFilter` in `internal/neutron/network_test.go`, mirroring `TestBuildNetworkListFilter` at `:95` — placeholder numbering and condition set, no database (FR-019, FR-020)
- [X] T042 [P] [US4] Contract tests S1, S2, S3, S4 and S5 in `internal/neutron/subnet_listing_test.go`: filter by network, by each of the six other filters, and confirm an unknown parameter leaves the result **unchanged** (FR-020a)
- [X] T043 [US4] Contract tests S6 and S7 in `internal/neutron/subnet_listing_test.go`: seed subnets sharing a `created_at`, walk pages with `marker`, assert each appears exactly once; and that a filter applies across the whole set, not just one page (FR-025)
- [X] T044 [P] [US4] Contract tests S8, S9, S10, S11, S12, S13 and S14 in `internal/neutron/subnet_gateway_test.go`: the gateway tri-state — omitted auto-assigns, explicit `null` stores NULL, and a gatewayless subnet lists, shows and updates without error (FR-021, FR-022, FR-023)
- [X] T045 [US4] Contract tests S15 and S16 in `internal/neutron/subnet_listing_test.go`: an unreadable row is logged with its id and the rest of the listing still returns `200`; no partial-result field is added (FR-024, FR-024a)
- [X] T046 [P] [US4] Contract test P12 in `pkg/networking/dhcp_options_test.go`: a gatewayless subnet's generated config omits `dhcp-option=3` but **retains** `dhcp-option=121` (FR-023a)
- [X] T047 [US4] Run and record RED in `EVIDENCE.md` (FR-038)

### Implementation for User Story 4

- [X] T048 [P] [US4] Split the two DHCP options in `pkg/networking/dhcp.go:239-241` — guard the router option on the gateway, always emit the metadata route via the subnet's on-link address. Today both are inside one `{{ if .GatewayIP }}`, so honouring "no gateway" would silently drop metadata and reintroduce F10's guest symptom (FR-023a, FR-004)
- [X] T049 [US4] Add `buildSubnetListFilter` to `internal/neutron/network.go` as a pure function returning `(conds, args)`, covering the seven contracted filters (FR-019, FR-020)
- [X] T050 [US4] Wire it into `ListSubnets` (`internal/neutron/network.go:1081`), composing with the existing visibility scope and the marker arguments
- [X] T051 [US4] Give `ListSubnets` a total order `(created_at, id)` with an exclusive marker, as `ListNetworks` already uses at `:684`/`:700` — `created_at DESC` alone leaves ties and unstable page boundaries (FR-025)
- [X] T052 [US4] Change `CreateSubnetRequest.Subnet.GatewayIP` to `*string` in `internal/neutron/network.go:900` and honour an explicit `null`; keep auto-assignment only when the attribute is **omitted** (`:1027-1030`) (FR-023)
- [X] T053 [US4] Scan the gateway as `*string` at all three read sites — `ListSubnets:1149`, `GetSubnet:1218`, `UpdateSubnet:1379` — so a NULL marshals to `null` instead of dropping the row or returning 500 (FR-021, FR-022)
- [X] T054 [US4] Allow `UpdateSubnet` to clear the gateway and persist the absence (S14)
- [X] T055 [US4] Replace the bare `continue` at `internal/neutron/network.go:1150` with a warning naming the row id, and check `rows.Err()` — the pattern already exists at `internal/neutron/ports.go:1087` (FR-024)
- [X] T056 [US4] Verify the gatewayless path through its consumers — `applyDHCPForNetwork`, `EnsureBridgeIP`, `internal/neutron/reconciler.go:238` and `internal/neutron/teardown.go:117` — none may assume a gateway exists. Covers P14 (config regenerated when a gateway is cleared) and P15 (a subnet with a gateway is unchanged)
- [X] T057 [US4] Run and record GREEN

**Checkpoint**: filtered listings are correct, and a gatewayless subnet is a first-class state.

---

## Phase 7: User Story 3 — A network's owning project is honest (Priority: P2)

**Goal**: a named project is honoured for administrators and refused otherwise, and every network
response reports its real owner.

**Independent Test**: as an administrator, create a network naming a second project, then compare the
reported owner **against the stored row**.

**⚠️ Both halves must land together.** The ticket's own probe reads back through `GetNetwork`, which
echoes the caller — binding the request without fixing the emitters leaves it red for a different
reason.

### Tests for User Story 3 ⚠️ write first, confirm RED

- [X] T058 [P] [US3] Contract tests N1, N2 and N3 in `internal/neutron/network_ownership_test.go`: an administrator's cross-project create lands in the target (asserted **against storage**, not the echo); a non-administrator is refused `400`; disagreeing spellings are refused `400` (FR-012, FR-013, FR-014, FR-015)
- [X] T059 [US3] Contract tests N4 and N5 in `internal/neutron/network_ownership_test.go`: an unused UUID and a non-UUID string are both refused `400`, never `500` from the foreign key, and neither discloses whether another project exists (FR-016)
- [X] T060 [US3] Contract tests N6, N7 and N10 in `internal/neutron/network_ownership_test.go`: a shared network is reported with its real owner from list and show, and `GetNetwork`'s 30-minute cache never serves one tenant's view to another (FR-017)
- [X] T061 [US3] Run and record RED in `EVIDENCE.md` (FR-038)

### Implementation for User Story 3

- [X] T062 [US3] Add `ProjectID` and `TenantID` as `*string` to `CreateNetworkRequest.Network` in `internal/neutron/network.go:401-407` — neither spelling is declared today, so the value is discarded at bind time and never reaches the handler (FR-012)
- [X] T063 [US3] Normalise the two spellings into each other in `CreateNetwork` and refuse `400` when both are supplied and disagree, matching upstream's `populate_project_info` (FR-015)
- [X] T064 [US3] Resolve the effective owner once: caller's project by default; a different project only for `common.RoleAdmin` via `common.HasRole`; otherwise refuse `400` with upstream's message text (FR-013, FR-014)
- [X] T065 [US3] Refuse `400` when the named project does not exist, checked before the insert so the foreign key never surfaces as a `500` (FR-016)
- [X] T066 [US3] Report the resolved owner in the `CreateNetwork` response at `internal/neutron/network.go:512-513` for both `tenant_id` and `project_id` (FR-017)
- [X] T067 [US3] Add `project_id` to the `ListNetworks` `SELECT` and report the row's owner at `internal/neutron/network.go:686-687` — the query does not select it today (FR-017)
- [X] T068 [US3] Add `project_id` to the `GetNetwork` `SELECT` and report the row's owner at `internal/neutron/network.go:760-761`; confirm the cached entry is now caller-independent (FR-017)
- [X] T069 [US3] Report the row's owner from `UpdateNetwork` as well, so all four emitters agree (FR-017)
- [X] T070 [US3] Log a refused cross-project request naming the caller's project and the requested project, mirroring the neutron refusal logging #545 introduced — contract N9 (FR-018)
- [X] T071 [US3] Verify N8 — the ticket's own probe: `openstack network create --project $OTHER b11-net` then `openstack network show b11-net -c project_id` returns `$OTHER`; record GREEN

**Checkpoint**: no network response claims a resource belongs to the caller when it does not.

---

## Phase 8: User Story 6 — Configure guest egress from the dashboard (Priority: P3)

**Goal**: networks can be marked external, tenants can see them, and gateways and floating IPs are
validated against them.

**Independent Test**: mark a network external, list external networks, and select it in the
dashboard's *Set Gateway* dialog.

**⚠️ Depends on User Story 3.** Both modify the same three response builders and add a column to the
same two `SELECT`s. Do not start before Phase 7 is complete.

### Tests for User Story 6 ⚠️ write first, confirm RED

- [X] T072 [P] [US6] Extend `TestNetworkListDecision` and `TestBuildNetworkListFilter` in `internal/neutron/network_test.go` for X6: external-only and non-external-only are complements, neither unconditionally empty (FR-032)
- [X] T073 [P] [US6] Contract tests X1, X2, X3 and X5 in `internal/neutron/external_networks_test.go`: an administrator sets the attribute on create and update; a non-administrator cannot; the value persists (FR-030, FR-031, FR-033)
- [X] T074 [US6] Contract test X4 in `internal/neutron/external_networks_test.go`: an administrator marking a **tenant's** network external actually applies — not "success with zero rows updated", which is what the owner-scoped `UPDATE` produces today (research R6.4)
- [X] T075 [US6] Contract tests X7, X8 and X9 in `internal/neutron/external_networks_test.go`: a tenant sees another project's external, **not shared** network reported with its real owner, and still cannot attach a subnet, rename, delete or unmark it (FR-033a, FR-033b)
- [X] T076 [US6] Contract tests X10, X11 and X12 in `internal/neutron/external_networks_test.go`: a router gateway and a floating IP on a non-external network are refused; on an external network they succeed (FR-034, FR-035)
- [X] T077 [US6] Contract tests X13, X14 and X15 in `internal/neutron/external_networks_test.go`: a gateway configured before the migration keeps working and is logged; unmarking an in-use network leaves existing routers working but refuses a new gateway; nothing assumes a single external network (FR-035a, FR-035b)
- [X] T078 [P] [US6] Gophercloud contract test in `test/contract/neutron/external_networks_test.go`: as an administrator create an external network, list it back through `networks.List`, and set a router gateway on it through a real SDK client — proving the attribute round-trips through a client rather than only our handler (X1, X6, X10, X12)
- [X] T079 [US6] Run and record RED in `EVIDENCE.md` (FR-038)

### Implementation for User Story 6

- [X] T080 [P] [US6] Create `migrations/066_neutron_external_networks.up.sql` adding `networks.external BOOLEAN NOT NULL DEFAULT false`, and `066_neutron_external_networks.down.sql` dropping it — reversible per the Constitution. Do **not** back-fill (FR-030, data-model.md)
- [X] T081 [US6] Accept `router:external` on create in `internal/neutron/network.go:401-407` and gate the write to `common.RoleAdmin`, matching upstream's `create_network:router:external` → `ADMIN` (FR-033)
- [X] T082 [US6] Accept `router:external` in `UpdateNetwork`'s request struct (`internal/neutron/network.go:841-846`) **and** exempt an administrator from the owner predicate at `:880-881` — otherwise marking a tenant's network updates zero rows and reports success, the same dishonest-success defect as B11 (research R6.4)
- [X] T083 [US6] Replace the three hardcoded `"router:external": false` emitters at `internal/neutron/network.go:517`, `:693`, `:766` with the stored value, adding the column to the list and show `SELECT`s (FR-031)
- [X] T084 [US6] Turn `networkListDecision` (`internal/neutron/network.go:553`) and `buildNetworkListFilter` (`:573`) into a real predicate and delete the `empty` short-circuit at `:636`; `neutronBool` already distinguishes absent from explicit-false (FR-032)
- [X] T085 [US6] Add `OR external = true` as a **third independent disjunct** to the `ListNetworks` and `GetNetwork` visibility scope — external must not require `shared`, or the tenant-facing gateway selection stays empty and F11 is fixed for administrators only (FR-033a)
- [X] T086 [US6] Confirm read-only: `callerMaySubnetNetwork` and the rename/delete/unmark paths must reject a non-owner who can merely see the network (FR-033b)
- [X] T087 [US6] Require the gateway network to be external in `configureExternalGateway` (`internal/neutron/router.go:596-620`), which accepts any network today (FR-034)
- [X] T088 [US6] Require the floating-IP network to be external in `CreateFloatingIP` (`internal/neutron/floatingip.go:168-178`), which accepts any network today and falls back to a hardcoded `192.0.2.0/24` (FR-035)
- [X] T089 [US6] Log any router whose gateway network is not external from `internal/neutron/reconciler.go:70`, and **do not** give `reconcileNAT` the new check — it must keep asserting SNAT for grandfathered gateways or an upgrade silently withdraws working egress (FR-035a, FR-035b)
- [X] T090 [US6] Verify X18: `migrate up` then `migrate down` both succeed and the down migration restores prior behaviour exactly
- [X] T091 [US6] Run and record GREEN

**Checkpoint**: all six stories are independently functional.

---

## Phase 9: Polish & Cross-Cutting Concerns

### Host validation (real mode — needs genuine root; rootless podman cannot create host namespaces)

*Completed on k3s 2026-08-13; R1–R11 of `HOST_TESTING_REAL_MODE.md` all pass. Evidence: `EVIDENCE.md` Phase 10.*

- [X] T092 Verify US1's real-mode half (P5, P6, P11) on a privileged Linux host: the TAP and its bridge share a namespace, a booted guest gets an address and reaches `169.254.169.254`, and repeated create/delete leaks no TAPs (FR-003, FR-004)
- [X] T093 Verify US4's gatewayless path (P13) on the same host: a guest on a **gatewayless** subnet gets an address, reaches metadata, and has no default route (FR-023a)
- [X] T094 Verify US6's egress (X17) on the same host: guest egress through an external gateway reaches the outside and a floating IP from that network reaches the guest (FR-036)
- [X] T095 Verify US6's dashboard dropdown (X16): Horizon's router *Set Gateway* dialog offers the external network, via `test/horizon_compat_test.sh` (FR-037)
- [X] T096 Verify US3's Horizon-visible half of ownership in `test/horizon_compat_test.sh` — a shared network shows its real owner

### Documentation (FR-040)

- [X] T097 [P] Fix the stale cross-reference in `internal/common/errors.go`: the doc comment blames the envelope divergence on cloud-storage#518, which is the **S3 access-key enumeration oracle** — an unrelated ticket
- [X] T098 [P] Update the three comments in `internal/neutron/network.go:549-551`, `:569-570`, `:636` that assert "O3K models no external networks" — all three become false
- [X] T099 Extend the comment at `internal/neutron/network.go:479` explaining that the bridge stays in the default namespace *and* that this is why the TAP goes there too, so the split is not reintroduced
- [X] T100 [P] Narrow the *Project Isolation* claim in `CLAUDE.md`: after F10, port-level L2 isolation comes from one bridge per network; the per-project namespace remains only for auto-allocated topology and the overlay coordinator
- [X] T101 [P] Update any `docs/` API/compatibility notes describing the Neutron error format or network attributes to match the delivered behaviour

### Gates and regression

- [X] T102 Confirm FR-039: #545's `callerMaySubnetNetwork` cross-tenant subnet-write refusal, #545's subnet owner-reporting rule and #549's console 404-not-400 behaviour are all unchanged
- [X] T103 Run `make fmt`, `make lint`, `make test`, `make test-contract`
- [X] T104 Run `make test-all` and compare against `/tmp/546-baseline-testall.txt` — it must now exit 0 where the baseline exited non-zero
- [X] T105 Walk quickstart.md end to end and confirm every "Now → Expected after" row flipped
- [X] T106 Complete `EVIDENCE.md`: a RED→GREEN record for each of the six defects (FR-038, SC-010)
- [X] T107 Validate the success criteria explicitly and record the result in `EVIDENCE.md`: SC-001 and SC-002 (end-to-end port/guest path, including a network created before the last restart), SC-003 (no network-service response carries the compute envelope), SC-004 (`make test-all` exits 0), SC-005 (cross-project creates are applied or refused, checked against stored owner), SC-006 and SC-007 (filtered listings exact; gatewayless subnet visible and readable), SC-008 (all four rule descriptions agree), SC-009 (egress configurable from the dashboard) and SC-011 (no previously passing check regressed)

---

## Dependencies & Execution Order

### Phase dependencies

- **Phase 1 (Setup)**: no dependencies
- **Phase 2 (Foundational)**: after Setup. T004–T005 **block US2 only**; **T006 (test-strategy approval) blocks every story's first test**
- **Phase 3 (US2)**: after Phase 2. Recommended first because it unblocks the test gate
- **Phase 4 (US1)**, **Phase 5 (US5)**, **Phase 6 (US4)**: independent of each other in logic; see file conflicts below
- **Phase 7 (US3)**: independent
- **Phase 8 (US6)**: **hard dependency on Phase 7**
- **Phase 9 (Polish)**: after the stories it validates

### Story dependencies

| Story | Depends on | Note |
|---|---|---|
| US2 (B10) | Phase 2 (T004–T006) | The only story needing the shared-type change |
| US1 (F10) | T006 | `ports.go` + `netns.go` only |
| US5 (B9) | T006 | `ports.go` only |
| US4 (B6) | T006 | `network.go` subnet handlers + `dhcp.go` |
| US3 (B11) | T006 | `network.go` network handlers |
| US6 (F11) | **US3**, T006 | Same response builders, same `SELECT`s |

### File conflicts (logically independent, not concurrently editable)

- `internal/neutron/ports.go` — US1 (CreatePort/DeletePort) and US5 (CreateSecurityGroupRule)
- `internal/neutron/network.go` — US4 (subnet handlers), US3 (network handlers), US6 (network handlers)
- `internal/neutron/auto_allocated_topology.go` — US2 (T017 deletes the hand-rolled envelope) and US1 (T031 adds a comment). Smallest of the three; if the two stories run concurrently, land T017 first and let T031 amend it
- `internal/neutron/network_test.go` — US4 (T041) and US6 (T072)

### Within each story

RED tests → confirm RED → implementation → confirm GREEN. Non-negotiable per the Constitution.

---

## Parallel opportunities

`[P]` is assigned strictly by file: a task is parallel only if no other `[P]` task in the same phase
writes the same file. Several test groups are logically independent but share one test file, so they
are **not** marked parallel.

| Phase | Genuinely parallel | Sequential within the phase (shared file) |
|---|---|---|
| 1 Setup | T002, T003 | — |
| 3 US2 tests | T007 only | T008–T011 join T007 in `error_envelope_test.go` |
| 4 US1 tests | T021 only | T022, T023 in `port_namespace_test.go` |
| 4 US1 impl | T025 (`netns.go`) | T026–T029 in `ports.go` |
| 5 US5 tests | T033 only | T034 in `security_group_shape_test.go` |
| 6 US4 tests | T041, T042, T044, T046 — four distinct files | T043, T045 join T042 in `subnet_listing_test.go` |
| 6 US4 impl | T048 (`dhcp.go`) | T049–T056 in `network.go` |
| 7 US3 tests | T058 only | T059, T060 in `network_ownership_test.go` |
| 8 US6 tests | T072 (`network_test.go`), T073, T080 (migration) | T074–T077 join T073 in `external_networks_test.go` |
| 9 Docs | T097, T098, T100, T101 | T099 joins T098 in `network.go` |

**Across stories**: with two workers, the clean split is **US1 + US5 on `ports.go`/`netns.go`** for one
and **US4 → US3 → US6 on `network.go`** for the other, after US2 lands. That respects every file
conflict above — but note US2's T017 and US1's T031 both touch `auto_allocated_topology.go`, and US4's
T041 and US6's T072 both touch `network_test.go`.

---

## Implementation Strategy

### Increment 1 — turn the gate green (US2)

Phases 1–3. Small, mechanical, and it converts B10 from "244 call sites" to two new files plus one
shared-type field. Ends with `make test-all` passing, which every later increment depends on for a
trustworthy signal.

### Increment 2 — MVP (US1)

Phase 4. The reason the ticket exists: ports can be created, so guests reach the network and their
metadata. Stub half is verifiable anywhere; real half is T092.

### Increment 3 — the fidelity fixes (US5, US4, US3)

Phases 5–7, in that order — smallest first, and US3 last because US6 sits behind it.

### Increment 4 — external networks (US6)

Phase 8. The largest slice, the only migration, the only new concept. **If the branch needs trimming,
this is the piece to split into a follow-up** — the spec placed it at P3 for exactly that reason. Do
not trim Increment 1 (a broken gate) or Increment 2 (the ticket's purpose).

---

## Notes

- `[P]` means a different file with no incomplete dependency; see *File conflicts* before assuming two
  stories can be worked concurrently.
- Every phase writes its RED and GREEN evidence to `EVIDENCE.md` — FR-038 and SC-010 require a
  recorded before-and-after for each of the six defects, not just a green suite at the end.
- Commit per task or per logical group; stop at any checkpoint to validate a story independently.
- The shared `o3k-postgres` container is used across worktrees. If a suite goes red unexpectedly,
  re-check the baseline from T001–T003 before assuming this branch caused it.
