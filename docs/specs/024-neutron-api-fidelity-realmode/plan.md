# Implementation Plan: Neutron API Fidelity & Real-Mode Networking

**Branch**: `feature/546` (feature dir `024-neutron-api-fidelity-realmode`) | **Date**: 2026-08-11
**Spec**: [spec.md](./spec.md) | **Ticket**: [cloud-storage#546](https://github.com/cobaltcore-dev/cloud-storage/issues/546)
**Base**: `f34d498` — the tip of `feature/549`, so #545 and #549 are both beneath this branch.

## Summary

Six Neutron defects: a port cannot be created at all, errors answer in the compute service's
envelope, a requested owner is discarded with a `201`, a list filter is ignored, a rule reads back
differently than it was written, and external networks are not modelled so egress cannot be
configured from the dashboard.

Phase 0 changed the shape of two of them:

- **B10 shrank.** The ticket frames it as 244 `common.SendError` call sites across 14 files. In fact
  *every* error path — handlers, shared auth middleware, panic recovery, `c.Errors`, the unrouted-404
  and 405 handlers, the six database helpers — already funnels through that one function. Marking the
  Neutron **engine** once and rendering the envelope inside `SendError` converts all of them and
  changes **zero** call sites. It also satisfies the "error before the handler" edge case by
  construction. The only bypass in the package is the hand-rolled correct instance FR-009 already
  requires folding in.
- **B9 and F11 grew.** B9's write path conflates a deliberate `0` with unset (`Valid: portMin > 0`),
  so FR-028 needs a storage fix, not just a response fix — settling the question spec A6 left open.
  F11's `UpdateNetwork` is owner-scoped (`WHERE id = $n AND project_id = $n+1`), so an administrator
  marking a *tenant's* network external updates zero rows and reports success — the same
  dishonest-success defect as B11, in the fix for a different one.

F10's direction is forced rather than chosen: the bridge, dnsmasq and the SNAT rules are already in
the root namespace, so the port's TAP is the lone outlier, and the namespace it names is never
created.

**No new Go module. One migration (`066`). Two new non-test source files.**

## Technical Context

**Language/Version**: Go 1.26 (module targets 1.21+, per Constitution *Required Dependencies*)
**Primary Dependencies**: `gin-gonic/gin` (HTTP), `jackc/pgx/v5` + `pgxpool` (PostgreSQL),
`vishvananda/netlink` (real-mode bridge/TAP), `google/uuid`. **No new modules.**
**Storage**: External PostgreSQL. **One** migration — `066_neutron_external_networks` adds
`networks.external BOOLEAN NOT NULL DEFAULT false`, reversible. Five of the six defects need no
schema change; the nullable columns they mishandle (`subnets.gateway_ip`,
`security_group_rules.port_range_min/max`) have always been able to hold the state the API refuses to
produce.
**Testing**: `go test ./internal/neutron/...` (unit, with the existing `TestMain`/`requireDB` harness
in `network_db_test.go`), `test/contract/neutron/` (gophercloud, 59 tests — regression guard),
`test/error_handling_test.sh` (the committed RED for B10), `test/horizon_compat_test.sh` (dashboard),
plus a host runbook for the real-mode halves of F10 and F11.
**Target Platform**: Linux (real mode); stub mode anywhere. Real-mode acceptance needs genuine root —
rootless podman cannot create host network namespaces.
**Project Type**: Single-binary web service — six HTTP services in one process (`cmd/o3k/main.go`).
Neutron on `:9696`.
**Performance Goals**: unchanged. No new hot path; the added predicates are on tables with tens of
rows. Constitution's < 100ms metadata target and 1s fail-fast timeouts are untouched.
**Constraints**: Synchronous operations only (Constitution II) — no retries, no async reconciliation
added. Fail-fast (III). Both modes must work (IV). Error status codes stay as they are; only bodies
change.
**Scale/Scope**: 6 user stories, 48 functional requirements, 6 contract files, ~10 source files.
Touched: `cmd/o3k/main.go` (marker registration only), `internal/neutron/{network,ports,router,
floatingip,reconciler,auto_allocated_topology}.go`, `internal/common/errors.go`,
`pkg/networking/{netns,dhcp}.go`, `migrations/066_*`.

## Constitution Check

*GATE: must pass before Phase 0. Re-checked after Phase 1 — see below.*

| Principle | Assessment |
|---|---|
| **I. Flamingo API compatibility (NON-NEGOTIABLE)** | **PASS — this feature is entirely this principle.** All six defects are compatibility gaps. Every behavioural decision was taken from upstream *source* rather than inference: the error envelope from `neutron/api/api_common.py`, type names from `neutron_lib/exceptions`, the project-attribute rule from `neutron_lib/api/attributes.py` + `context.py`, the external-network role gate from `neutron/conf/policies/network.py`. Verified with real clients: python-openstackclient, gophercloud contract suite, Horizon. |
| **II. Synchronous operations** | **PASS.** No queue, no worker, no state machine. Port creation stays synchronous. The one periodic component touched — `reconcileNAT` — already exists and is deliberately *not* given new logic (FR-035a). |
| **III. Fail-fast** | **PASS.** No retries or backoff added. FR-005 tightens failure behaviour: name the failing step and leave nothing behind. The stub-mode fix *removes* a spurious failure rather than tolerating one. |
| **IV. Multi-mode architecture** | **PASS, and this is where F10 lives.** The defect is that stub and real mode fail for unrelated reasons; both are fixed, and both are covered — stub by unit tests, real by the host runbook. FR-002 makes the recorded network the authority in stub mode, which is what "returns fake data, no external dependencies" requires. |
| **V. Project isolation** | **PASS, and strengthened.** B11 stops three emitters reporting the caller as owner of resources they may not own (I1). F11's new visibility is read-only and explicitly preserves #545's rule that visibility grants no authority (I2, FR-033b). FR-039 forbids re-opening the cross-tenant subnet-write refusal. The one relaxation — an administrator may create into and update another project — is upstream's own rule, gated on `common.RoleAdmin`, matching the "explicit admin role check" this principle requires. |
| **VI. Single binary** | **PASS.** No new process, port or service. Two new non-test source files inside existing packages, plus one marker registration in the existing `createNeutronServer`. |
| **Test-First (workflow)** | **PASS.** Every defect has a named RED source (research R8); B10's and B9's already exist and are committed, so their RED is free. Test tasks precede implementation in every wave. |
| **Backwards compatibility (workflow)** | **PASS.** Migration `066` is reversible up/down. FR-035a is precisely this principle applied: the flag defaults to false, so validating stored rows would withdraw working egress on upgrade — grandfathering prevents it. |
| **Quality gates (before merge)** | Addressed: CLI compatibility (the ticket's own probes), Horizon (`test/horizon_compat_test.sh` for B11 ownership and F11's dropdown), contract tests, both modes, migration up **and** down, no credentials touched. |

**Result: PASS — no deviations. Complexity Tracking is empty.**

Two points worth recording, neither a violation:

1. **Article V and FR-013 look like a conflict and are not.** "No cross-project resource access
   without explicit admin role check" is satisfied by the explicit check; upstream Neutron permits
   exactly this, and spec A10/A11 record why this differs from #549's compute decision.
2. **Article I outranks internal symmetry.** Governance says API compatibility always wins, which is
   the tie-break that makes B11 (`400`, honour for admin) correct here even though the equivalent
   compute path refuses outright.

## Project Structure

### Documentation (this feature)

```text
docs/specs/024-neutron-api-fidelity-realmode/
├── spec.md                                  # 6 stories, 48 FRs, 7 clarifications
├── plan.md                                  # this file
├── research.md                              # Phase 0 — R1–R8
├── data-model.md                            # Phase 1 — migration 066, entities, I1–I8
├── quickstart.md                            # Phase 1 — reproduce all six, then verify
├── checklists/requirements.md               # spec quality gate (all pass)
└── contracts/                               # Phase 1 — one per story
    ├── neutron-errors.md                    # E1–E12  + the type mapping table
    ├── network-ownership.md                 # N1–N10
    ├── subnet-listing.md                    # S1–S16
    ├── security-group-rule-shape.md         # G1–G8
    ├── external-networks.md                 # X1–X18
    └── port-and-dhcp.md                     # P1–P15
```

### Source code (repository root)

```text
cmd/o3k/
└── main.go                        # MODIFY  register the envelope marker as the FIRST r.Use() in
│                                  #         createNeutronServer (:626) — engine level, NOT the route
│                                  #         group, or NoRoute/NoMethod/auth are missed  [B10]
internal/
├── common/
│   ├── errors.go                  # MODIFY  envelope selection in SendError/ToJSON; +Resource field
│   └── neutron_errors.go          # NEW     the (status, Code, Resource) → type table  [B10]
├── middleware/                    # unchanged — already routes every error through SendError
└── neutron/
    ├── errors.go                  # NEW     route-group marker + Neutron renderer wiring  [B10]
    ├── network.go                 # MODIFY  B11 bind+emitters, B6 filters+gateway, F11 flag+filter+update
    ├── ports.go                   # MODIFY  F10 namespace, B9 stop flattening
    ├── router.go                   # MODIFY  F11 gateway must be external
    ├── floatingip.go              # MODIFY  F11 FIP network must be external
    ├── reconciler.go              # MODIFY  F11 log grandfathered gateways
    └── auto_allocated_topology.go # MODIFY  delete the hand-rolled envelope (FR-009)
pkg/networking/
├── netns.go                       # MODIFY  F10 stub attach stops consulting the in-process map
└── dhcp.go                        # MODIFY  FR-023a split router option from metadata route
migrations/
├── 066_neutron_external_networks.up.sql     # NEW
└── 066_neutron_external_networks.down.sql   # NEW
test/
├── error_handling_test.sh         # existing — the committed RED for B10
└── contract/neutron/              # existing — regression guard
```

### Documentation targets (FR-040)

Stale text that this feature falsifies. Each is a claim in the repo that becomes wrong the moment the
corresponding story lands, so they are part of the work, not an afterthought:

| Target | What is wrong | Story |
|---|---|---|
| `internal/common/errors.go` — the `NewIdentityNotFoundError` doc comment | Attributes the envelope divergence to `cloud-storage#518`. **#518 is the S3 access-key enumeration oracle** — an unrelated ticket. The envelope work is *this* feature | B10 |
| `internal/neutron/network.go:549-551`, `:569-570`, `:636` | Three comments assert "O3K models no external/provider networks" and explain the deliberately-empty result. All three become false | F11 |
| `internal/neutron/network.go:479` | "Create bridge in default namespace (not project namespace) for libvirt access" — still true, and now the *reason* the TAP moves too. Worth extending rather than deleting, so the next reader does not re-introduce the split | F10 |
| `docs/` API/compat notes describing the Neutron error format or network attributes | Must match the delivered envelope and the new attribute | B10, F11 |
| `CLAUDE.md` — *Project Isolation* | Says "Network namespaces per project for network isolation". After F10 the ports' L2 isolation comes from one bridge per network; the per-project namespace remains only for auto-allocated topology and the overlay coordinator. Narrow the claim rather than delete it | F10 |

**Structure Decision**: no new package, no new service, no new binary. Two new **non-test source**
files, both small and both inside packages that already exist — `internal/common/neutron_errors.go`
(the type table, one place so the vocabulary cannot drift per FR-007a) and `internal/neutron/errors.go`
(the marker middleware). Everything else is a modification. tasks.md additionally creates eight new
`*_test.go` files, which are not counted here.

⚠️ **The marker is registered at engine level in `cmd/o3k/main.go`, not on the route group.** Neutron's
auth middleware sits at engine level (`:632`) and `NoRoute`/`NoMethod` (`:633`, `:635`) never run inside
a group at all, so a group-level marker would miss contracts E5, E6 and E7 while the rest passed —
exactly the half-fix FR-008 forbids. See `contracts/neutron-errors.md`. This matches the codebase's existing shape and the
"≤3 projects / anti-abstraction" posture: no new indirection is introduced, because the choke point
that makes B10 tractable is one that already exists.

## Wave plan

Stories are independent except where noted. Ordering favours turning the suite green first, because
every other story's test run is currently assessed against a red baseline (D4).

| Wave | Story | Why here | Parallel-safe with |
|---|---|---|---|
| **W0** | Baseline capture | `make test-all` is already red on B10, and the shared `o3k-postgres` is used by ~12 worktrees. Record the pre-change state or this branch wears someone else's failure | — |
| **W1** | **US2 / B10** (P1) | Single choke point, ~2 files, and it turns `make test-all` green so every later wave has a real baseline. RED is already committed | everything |
| **W2** | **US1 / F10** (P1) | Highest user value; unblocks ports, metadata and the guest console symptom. Touches `ports.go` + `netns.go` only | W4, W5 |
| **W3** | **US5 / B9** (P3) | Smallest slice in the feature — stop flattening five pointers. Out of priority order deliberately: it is a self-contained `ports.go` change that can ship in a day | W4, W5 |
| **W4** | **US4 / B6** (P2) | `network.go` subnet handlers + `dhcp.go`. Includes FR-023a, which must land with F10's real-mode verification to be meaningful | W2, W3 |
| **W5** | **US3 / B11** (P2) | `network.go` network handlers. Both halves (bind + emitters) must land together or the ticket's own probe stays red | W2, W3 |
| **W6** | **US6 / F11** (P3) | Largest; the only migration. **Must follow W5** — same three response builders, same two `SELECT`s | nothing |

⚠️ **W5 → W6 is a hard sequence**, not a preference: `network-ownership.md` and
`external-networks.md` both modify `CreateNetwork`/`ListNetworks`/`GetNetwork` and both add a column to
the same two queries. W2/W3/W4 touch different functions or different files and can overlap.

**If the branch needs trimming, split out W6 (F11).** It is the only story with a migration, the only
one that adds a concept, and the spec already places it at P3 for that reason. Do **not** trim W1 — it
is a broken gate. Do not trim W2 — it is the reason the ticket exists.

## Risks

| Risk | Mitigation |
|---|---|
| The envelope change leaks into another service | The marker is set only on the Neutron engine, which is its own `gin.New()`; contract E10 asserts compute/identity/block-storage/image envelopes are byte-unchanged |
| A partial B10 conversion that *looks* finished | E2 asserts the **absence** of `code`, not just the presence of the wrapper — this is the assertion FR-008 exists for |
| The marker is registered on the route group and silently misses auth / unrouted-404 / 405 | Contracts E5, E6, E7 target exactly those paths; the registration point is pinned to engine level in `contracts/neutron-errors.md` and in the task text |
| F10's real half is unverifiable in CI | Split acceptance: stub half in unit tests (P3), real half in the host runbook (P5–P6, P11). Needs genuine root |
| F11's migration withdraws working egress on upgrade | FR-035a/b + X13; the reconciler is deliberately **not** given the new check |
| B11's cache leaks one tenant's view to another | N10 — once `project_id` is row-derived the entry is caller-independent, but it must be asserted, not assumed |
| Fixing B6's paging destabilises existing pages | Adopt the `(created_at, id)` total order `ListNetworks` already uses; S6 walks pages and asserts each subnet appears exactly once |
| The shared PostgreSQL poisons a run | W0 records the baseline; check the project list and quotas before believing a red result |

## Complexity Tracking

> Fill ONLY if Constitution Check has violations that must be justified.

None. Constitution Check passed with no deviations.
